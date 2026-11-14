// Package proxy implements the forward proxy handler: plain HTTP requests are
// relayed through the forwarding engine, CONNECT requests get a raw TCP tunnel.
package proxy

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"

	"domain-forwarder/pkg/forwarder"
)

// Config carries the settings the proxy handler needs.
type Config struct {
	Debug             bool
	AddProxyHeaders   bool
	EditHeaders       map[string]string // Headers set on forwarded plain-HTTP requests; nil disables editing
	Mirror            *Mirror           // Async shadow delivery; nil disables mirroring
	SourceIPBlacklist []string
	Auth              *Auth         // nil disables proxy authentication
	DialTimeout       time.Duration // CONNECT tunnel dial timeout
	DialKeepAlive     time.Duration // CONNECT tunnel TCP keep-alive interval
}

// Server is the forward proxy HTTP handler.
type Server struct {
	engine *forwarder.Forwarder
	config Config
	auth   *authenticator
}

// New creates a proxy handler on top of the forwarding engine.
func New(engine *forwarder.Forwarder, config Config) *Server {
	s := &Server{engine: engine, config: config}
	if config.Auth != nil {
		s.auth = newAuthenticator(config.Auth)
	}
	return s
}

// ServeHTTP dispatches CONNECT requests to the tunnel handler and everything
// else through the forwarding engine.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && forwarder.IPBlacklisted(s.config.SourceIPBlacklist, clientIP) {
		if s.config.Debug {
			log.Printf("[BLOCKED] source=%s - IP is blacklisted", r.RemoteAddr)
		}
		http.Error(w, "Forbidden: request is not allowed", http.StatusForbidden)
		return
	}

	if s.auth != nil && !s.auth.allow(r) {
		if s.config.Debug {
			log.Printf("[DENIED] source=%s method=%s target=%s - proxy authentication failed", r.RemoteAddr, r.Method, r.Host)
		}
		w.Header().Set("Proxy-Authenticate", `Basic realm="domain-forwarder", charset="UTF-8"`)
		http.Error(w, "Proxy Authentication Required", http.StatusProxyAuthRequired)
		return
	}

	if r.Method == http.MethodConnect {
		s.handleConnect(w, r)
	} else {
		s.handleHTTP(w, r)
	}
}

// handleHTTP forwards a plain HTTP proxy request through the shared engine.
// Forward proxies stay transparent, so the client's Accept-Encoding is kept.
func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	// Ensure the URL has a scheme and host for proxy requests
	if r.URL.Scheme == "" {
		r.URL.Scheme = "http"
	}
	if r.URL.Host == "" {
		r.URL.Host = r.Host
	}

	if s.config.AddProxyHeaders {
		forwarder.AddForwardedHeaders(r, r.URL.Scheme)
	}
	applyRequestEdits(r, s.config.EditHeaders)
	if s.config.Mirror != nil {
		s.config.Mirror.prepare(r)
	}

	target := r.URL.String()
	result, err := s.engine.ForwardRequest(w, r, target, nil)
	logSummary(r, target, result)
	if err != nil {
		if errors.Is(err, forwarder.ErrResponseStarted) {
			// The failure came after the response reached the client, usually
			// because the client hung up mid-body. The summary line still records
			// the request, so the detail is debug-only.
			if s.config.Debug {
				log.Printf("Error forwarding request to %s: %v", target, err)
			}
			return
		}
		log.Printf("Error forwarding request to %s: %v", target, err)
		http.Error(w, "Bad Gateway: upstream request failed", http.StatusBadGateway)
	}
}

// handleConnect establishes a tunnel for HTTPS (CONNECT) requests. HTTP/1.1
// clients get a hijacked raw TCP tunnel; HTTP/2 clients tunnel data inside the
// CONNECT stream, since HTTP/2 supports neither connection hijacking nor
// per-connection takeover.
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	targetConn, err := (&net.Dialer{
		Timeout:   s.config.DialTimeout,
		KeepAlive: s.config.DialKeepAlive,
	}).Dial("tcp", r.Host)
	if err != nil {
		log.Printf("Error connecting to %s: %v", r.Host, err)
		http.Error(w, "Bad Gateway: cannot connect to target", http.StatusBadGateway)
		return
	}

	var c2t, t2c copyResult
	if r.ProtoMajor >= 2 {
		c2t, t2c = s.tunnelH2(w, r, targetConn, start)
	} else {
		c2t, t2c = s.tunnelH1(w, r, targetConn, start)
	}

	s.logTunnel(r, targetConn, c2t, t2c, time.Since(start))
}

// tunnelH1 relays a CONNECT tunnel over a hijacked HTTP/1.1 connection.
func (s *Server) tunnelH1(w http.ResponseWriter, r *http.Request, targetConn net.Conn, start time.Time) (c2t, t2c copyResult) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		log.Printf("Hijacking not supported")
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		_ = targetConn.Close()
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		log.Printf("Error hijacking connection: %v", err)
		http.Error(w, "Hijack failed: "+err.Error(), http.StatusInternalServerError)
		_ = targetConn.Close()
		return
	}

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		// Only reachable when the client closes between CONNECT and the reply,
		// which is routine client behavior rather than a proxy failure.
		if s.config.Debug {
			log.Printf("Error sending connection established to %s: %v", r.Host, err)
		}
		_ = clientConn.Close()
		_ = targetConn.Close()
		return
	}

	// Bidirectional copy reusing the engine's buffer pool. Per-direction end
	// times are recorded so the log shows which side stopped first: a nil error
	// means EOF (the peer half-closed), an error means the copy was aborted.
	buffers := s.engine.Buffers()
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		buf := buffers.Get()
		defer buffers.Put(buf)
		c2t.bytes, c2t.err = io.CopyBuffer(writerOnly{targetConn}, readerOnly{clientConn}, buf)
		c2t.end = time.Since(start)
		if tcpConn, ok := targetConn.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		buf := buffers.Get()
		defer buffers.Put(buf)
		t2c.bytes, t2c.err = io.CopyBuffer(writerOnly{clientConn}, readerOnly{targetConn}, buf)
		t2c.end = time.Since(start)
		if tcpConn, ok := clientConn.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		}
	}()

	wg.Wait()
	_ = clientConn.Close()
	_ = targetConn.Close()
	return
}

// tunnelH2 relays a CONNECT tunnel inside an HTTP/2 stream. The request body
// carries client-to-upstream bytes and the response body carries the other
// direction, so every chunk is flushed explicitly; there is no connection to
// hijack and no way to half-close the response side except by returning.
func (s *Server) tunnelH2(w http.ResponseWriter, r *http.Request, targetConn net.Conn, start time.Time) (c2t, t2c copyResult) {
	defer targetConn.Close()

	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		// The client went away before the tunnel reply; nothing to relay.
		return
	}

	buffers := s.engine.Buffers()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := buffers.Get()
		defer buffers.Put(buf)
		c2t.bytes, c2t.err = io.CopyBuffer(writerOnly{targetConn}, r.Body, buf)
		c2t.end = time.Since(start)
		if tcpConn, ok := targetConn.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		}
	}()

	buf := buffers.Get()
	defer buffers.Put(buf)
	for {
		n, readErr := targetConn.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				t2c.err = writeErr
				break
			}
			t2c.bytes += int64(n)
			if err := rc.Flush(); err != nil {
				t2c.err = err
				break
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				t2c.err = readErr
			}
			break
		}
	}
	t2c.end = time.Since(start)

	// Ending the response stream requires returning from the handler, and the
	// client only finishes its request stream after that. Close the upstream
	// connection to release the request-body copy, then wait so the summary
	// line carries the final byte counters.
	_ = targetConn.Close()
	<-done
	if t2c.err == nil && (r.Context().Err() != nil || errors.Is(c2t.err, net.ErrClosed)) {
		// An HTTP/2 client ends a CONNECT stream with RST_STREAM (or by closing
		// the connection), which surfaces as a request-body read error; and the
		// forced close above does the same. If the upstream side completed
		// cleanly, this is a normal teardown rather than a proxy failure.
		c2t.err = nil
	}
	return
}

// readerOnly and writerOnly hide ReaderFrom/WriterTo so io.CopyBuffer cannot
// use splice(2), which fails with EIO on the biased page refcounts GVE uses.
type readerOnly struct{ io.Reader }

type writerOnly struct{ io.Writer }

// copyResult is one direction of a CONNECT tunnel.
type copyResult struct {
	bytes int64
	err   error
	end   time.Duration // elapsed time from tunnel start until this copy returned
}

// logTunnel emits exactly one line per CONNECT tunnel. upstream is the resolved
// address, which makes front-end IP rotation visible. A clean tunnel only shows
// counters and duration; aborted tunnels replace the duration (equal to the
// later direction's end time) with per-direction detail. upstream_local on an
// abort carries the ephemeral port needed to match NAT/conntrack state.
func (s *Server) logTunnel(r *http.Request, targetConn net.Conn, c2t, t2c copyResult, duration time.Duration) {
	line := fmt.Sprintf("[%s] proto=%s source=%s target=%s upstream=%s sent=%d recv=%d",
		r.Method, protoName(r), r.RemoteAddr, r.Host, targetConn.RemoteAddr(), c2t.bytes, t2c.bytes)

	aborted := c2t.err != nil || t2c.err != nil
	if !aborted {
		line += fmt.Sprintf(" duration=%v", duration.Round(time.Millisecond))
	} else {
		line += fmt.Sprintf(" outcome=aborted upstream_local=%s c2t=%s@%v t2c=%s@%v",
			targetConn.LocalAddr(),
			errKind(c2t.err), c2t.end.Round(time.Millisecond),
			errKind(t2c.err), t2c.end.Round(time.Millisecond))
	}
	if detail := errDetail(c2t.err); detail != "" {
		line += fmt.Sprintf(" c2t_err=%q", detail)
	}
	if detail := errDetail(t2c.err); detail != "" {
		line += fmt.Sprintf(" t2c_err=%q", detail)
	}
	if s.config.Debug {
		if clientLocal, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
			line += fmt.Sprintf(" client_local=%s", clientLocal)
		}
		if !aborted {
			line += fmt.Sprintf(" upstream_local=%s", targetConn.LocalAddr())
		}
	}
	log.Print(line)
}

// protoName names the proxy-side protocol the tunnel runs over.
func protoName(r *http.Request) string {
	if r.ProtoMajor >= 2 {
		return "h2"
	}
	return "h1"
}

// errDetail only surfaces errors errKind cannot classify; a known errno is
// already named by the kind, so repeating its message adds no information.
func errDetail(err error) string {
	if err == nil || errKind(err) != "error" {
		return ""
	}
	var sysErr *os.SyscallError
	if errors.As(err, &sysErr) {
		return sysErr.Syscall + ": " + sysErr.Err.Error()
	}
	return err.Error()
}

// errKind classifies a copy error. splice(2) reports a socket that was reset or
// otherwise torn down as EIO instead of ECONNRESET, so eio and reset both point
// to an abrupt abort rather than a normal close.
func errKind(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, net.ErrClosed):
		return "closed"
	case errors.Is(err, syscall.ECONNRESET):
		return "reset"
	case errors.Is(err, syscall.ECONNABORTED):
		return "conn_aborted"
	case errors.Is(err, syscall.EPIPE):
		return "broken_pipe"
	case errors.Is(err, syscall.ETIMEDOUT):
		return "timeout"
	case errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout"
	case errors.Is(err, syscall.EIO):
		return "eio"
	default:
		return "error"
	}
}

// logSummary prints the unified per-request summary line.
func logSummary(r *http.Request, target string, result forwarder.Result) {
	log.Printf("[%s] source=%s target=%s req_size=%d resp_status=%d resp_size=%d duration=%v",
		r.Method, r.RemoteAddr, target, r.ContentLength, result.StatusCode, result.RespSize, result.Duration)
}
