package proxy

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"domain-forwarder/pkg/forwarder"
	"domain-forwarder/pkg/mitm"
)

// handleMITM terminates TLS for one intercepted CONNECT: the client connection
// is hijacked, the TLS session is served locally from a certificate minted for
// the requested host, and each decrypted request is re-originated to the
// original target through the normal forwarding pipeline.
func (s *Server) handleMITM(w http.ResponseWriter, r *http.Request, group *mitm.Group) {
	start := time.Now()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		log.Printf("Hijacking not supported")
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, brw, err := hijacker.Hijack()
	if err != nil {
		log.Printf("Error hijacking connection: %v", err)
		http.Error(w, "Hijack failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		if s.config.Debug {
			log.Printf("Error sending connection established to %s: %v", r.Host, err)
		}
		_ = clientConn.Close()
		return
	}

	// The client sends its ClientHello only after the 200 above. Sniff it so a
	// CONNECT that carries plaintext (port 80, SSH, ...) keeps working as a raw
	// tunnel, which matters when a catch-all group matches every host.
	sniffTimeout := s.config.TLSHandshakeTimeout
	if sniffTimeout <= 0 {
		sniffTimeout = 10 * time.Second
	}
	_ = clientConn.SetReadDeadline(time.Now().Add(sniffTimeout))
	first, err := brw.Reader.Peek(1)
	_ = clientConn.SetReadDeadline(time.Time{})
	if err != nil {
		if s.config.Debug {
			log.Printf("[MITM] source=%s target=%s: waiting for client hello: %v", r.RemoteAddr, r.Host, err)
		}
		_ = clientConn.Close()
		return
	}
	if first[0] != 0x16 { // TLS handshake record
		s.dialAndRelay(r, start, clientConn, brw.Reader)
		return
	}

	authority := r.Host
	connectHost := mitm.Host(authority)
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := mitm.Host(hello.ServerName)
			if name == "" {
				name = connectHost
			} else if !group.Match(name) {
				return nil, fmt.Errorf("SNI %q is outside interception group %q", hello.ServerName, group.Name)
			}
			return group.CA.CertificateFor(name)
		},
	}

	var requests atomic.Int64
	done := make(chan struct{})
	notify := &notifyConn{Conn: &bufferedConn{Conn: clientConn, r: brw.Reader}, done: done}
	tlsConn := tls.Server(notify, tlsConfig)

	// TLSConfig is deliberately left nil: net/http then configures HTTP/2 itself
	// when Serve receives *tls.Conn values, so h2 needs no extra dependency.
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			requests.Add(1)
			s.serveIntercepted(w, req, authority)
		}),
		ReadHeaderTimeout: s.config.ReadHeaderTimeout,
		IdleTimeout:       s.config.IdleTimeout,
		MaxHeaderBytes:    s.config.MaxHeaderBytes,
		ErrorLog:          log.New(mitmErrorWriter{s.config.Debug}, "", 0),
	}

	err = srv.Serve(&singleConnListener{conn: tlsConn, done: done})
	if s.config.Debug && err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("[MITM] source=%s target=%s: %v", r.RemoteAddr, authority, err)
	}
	_ = tlsConn.Close()

	log.Printf("[MITM] source=%s target=%s group=%s requested=%d duration=%v",
		r.RemoteAddr, authority, group.Name, requests.Load(), time.Since(start).Round(time.Millisecond))
}

// serveIntercepted forwards one decrypted request. The target always comes from
// the CONNECT authority, never the Host header, so a client cannot redirect the
// re-originated request to another host.
func (s *Server) serveIntercepted(w http.ResponseWriter, r *http.Request, authority string) {
	if r.Method == http.MethodConnect {
		http.Error(w, "Not Implemented: extended CONNECT is not supported", http.StatusNotImplemented)
		return
	}

	r.URL.Scheme = "https"
	r.URL.Host = authority
	r.Host = authority

	if isUpgrade(r) {
		s.relayUpgrade(w, r, authority)
		return
	}
	s.forward(w, r, r.URL.String(), "https")
}

// isUpgrade reports whether r is an HTTP/1.1 protocol upgrade such as WebSocket.
func isUpgrade(r *http.Request) bool {
	if r.ProtoMajor != 1 || r.Header.Get("Upgrade") == "" {
		return false
	}
	for _, value := range r.Header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

// relayUpgrade forwards an HTTP/1.1 upgrade handshake with the configured edits
// applied, then relays raw bytes in both directions once the upstream switches
// protocols.
func (s *Server) relayUpgrade(w http.ResponseWriter, r *http.Request, authority string) {
	if s.config.AddProxyHeaders {
		forwarder.AddForwardedHeaders(r, "https")
	}
	applyRequestEdits(r, s.config.EditHeaders)
	if s.config.Mirror != nil {
		s.config.Mirror.prepare(r)
	}

	out := r.Clone(r.Context())
	out.URL.Scheme = "https"
	out.URL.Host = authority
	out.Host = authority
	out.Close = false

	upstream, err := s.dialUpstream(r, authority)
	if err != nil {
		log.Printf("Error connecting to %s: %v", authority, err)
		http.Error(w, "Bad Gateway: cannot connect to target", http.StatusBadGateway)
		return
	}
	if err := out.Write(upstream); err != nil {
		_ = upstream.Close()
		log.Printf("Error forwarding upgrade request to %s: %v", authority, err)
		http.Error(w, "Bad Gateway: upstream request failed", http.StatusBadGateway)
		return
	}

	upstreamReader := bufio.NewReader(upstream)
	resp, err := http.ReadResponse(upstreamReader, out)
	if err != nil {
		_ = upstream.Close()
		log.Printf("Error reading upgrade response from %s: %v", authority, err)
		http.Error(w, "Bad Gateway: upstream response failed", http.StatusBadGateway)
		return
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer upstream.Close()
		defer resp.Body.Close()
		if err := resp.Write(w); err != nil && s.config.Debug {
			log.Printf("Error relaying upgrade response from %s: %v", authority, err)
		}
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, brw, err := hijacker.Hijack()
	if err != nil {
		_ = upstream.Close()
		log.Printf("Error hijacking upgrade connection: %v", err)
		return
	}

	// Write the 101 by hand: the response has no body, and flushing the hijacked
	// writer is required before any tunnelled bytes go out.
	if _, err := fmt.Fprintf(brw.Writer, "HTTP/1.1 %s\r\n", resp.Status); err != nil {
		_ = clientConn.Close()
		_ = upstream.Close()
		return
	}
	if err := resp.Header.Write(brw.Writer); err != nil {
		_ = clientConn.Close()
		_ = upstream.Close()
		return
	}
	if _, err := brw.Writer.WriteString("\r\n"); err != nil {
		_ = clientConn.Close()
		_ = upstream.Close()
		return
	}
	if err := brw.Writer.Flush(); err != nil {
		_ = clientConn.Close()
		_ = upstream.Close()
		return
	}

	buffers := s.engine.Buffers()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		buf := buffers.Get()
		defer buffers.Put(buf)
		_, _ = io.CopyBuffer(writerOnly{upstream}, readerOnly{brw.Reader}, buf)
		closeWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		buf := buffers.Get()
		defer buffers.Put(buf)
		_, _ = io.CopyBuffer(writerOnly{clientConn}, readerOnly{upstreamReader}, buf)
		closeWrite(clientConn)
	}()
	wg.Wait()

	_ = upstream.Close()
	_ = clientConn.Close()
}

// dialUpstream opens a verified TLS connection to the CONNECT authority.
func (s *Server) dialUpstream(r *http.Request, authority string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: s.config.DialTimeout, KeepAlive: s.config.DialKeepAlive}
	rawConn, err := dialer.DialContext(r.Context(), "tcp", authority)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if s.config.TLSClientConfig != nil {
		tlsConfig = s.config.TLSClientConfig.Clone()
	}
	if tlsConfig.ServerName == "" {
		tlsConfig.ServerName = mitm.Host(authority)
	}
	conn := tls.Client(rawConn, tlsConfig)
	if err := conn.HandshakeContext(r.Context()); err != nil {
		_ = rawConn.Close()
		return nil, err
	}
	return conn, nil
}

// dialAndRelay keeps an intercepted-but-plaintext CONNECT as a raw tunnel.
func (s *Server) dialAndRelay(r *http.Request, start time.Time, clientConn net.Conn, clientReader io.Reader) {
	targetConn, err := (&net.Dialer{
		Timeout:   s.config.DialTimeout,
		KeepAlive: s.config.DialKeepAlive,
	}).Dial("tcp", r.Host)
	if err != nil {
		log.Printf("Error connecting to %s: %v", r.Host, err)
		_ = clientConn.Close()
		return
	}
	c2t, t2c := s.relay(start, clientConn, clientReader, targetConn)
	s.logTunnel(r, targetConn, c2t, t2c, time.Since(start))
}

func closeWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// bufferedConn reads through a bufio.Reader so bytes that arrived together with
// the CONNECT request (typically the TLS ClientHello) are not lost.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// notifyConn reports when the wrapped connection closes, which unblocks the
// second Accept of singleConnListener.
type notifyConn struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *notifyConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.Conn.Close()
}

// singleConnListener hands one accepted connection to http.Server and then
// blocks until that connection closes, so Serve returns once the tunnel ends.
// The connection is passed through unwrapped: net/http only recognizes a
// *tls.Conn for ALPN/HTTP-2 dispatch.
type singleConnListener struct {
	conn  net.Conn
	taken atomic.Bool
	done  chan struct{}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	if l.taken.CompareAndSwap(false, true) {
		return l.conn, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *singleConnListener) Close() error { return nil }

func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

// mitmErrorWriter routes net/http's per-connection errors to the debug log.
type mitmErrorWriter struct{ debug bool }

func (w mitmErrorWriter) Write(p []byte) (int, error) {
	if w.debug {
		log.Printf("[MITM] %s", strings.TrimRight(string(p), "\n"))
	}
	return len(p), nil
}
