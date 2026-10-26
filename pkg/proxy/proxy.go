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
	SourceIPBlacklist []string
	DialTimeout       time.Duration // CONNECT tunnel dial timeout
	DialKeepAlive     time.Duration // CONNECT tunnel TCP keep-alive interval
}

// Server is the forward proxy HTTP handler.
type Server struct {
	engine *forwarder.Forwarder
	config Config
}

// New creates a proxy handler on top of the forwarding engine.
func New(engine *forwarder.Forwarder, config Config) *Server {
	return &Server{engine: engine, config: config}
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

	target := r.URL.String()
	result, err := s.engine.ForwardRequest(w, r, target, nil)
	logSummary(r, target, result)
	if err != nil {
		log.Printf("Error forwarding request to %s: %v", target, err)
		if !errors.Is(err, forwarder.ErrResponseStarted) {
			http.Error(w, "Bad Gateway: upstream request failed", http.StatusBadGateway)
		}
	}
}

// handleConnect establishes a raw TCP tunnel for HTTPS (CONNECT) requests.
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
		log.Printf("Error sending connection established: %v", err)
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

	var c2t, t2c copyResult

	go func() {
		defer wg.Done()
		buf := buffers.Get()
		defer buffers.Put(buf)
		c2t.bytes, c2t.err = io.CopyBuffer(targetConn, clientConn, buf)
		c2t.end = time.Since(start)
		if tcpConn, ok := targetConn.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		buf := buffers.Get()
		defer buffers.Put(buf)
		t2c.bytes, t2c.err = io.CopyBuffer(clientConn, targetConn, buf)
		t2c.end = time.Since(start)
		if tcpConn, ok := clientConn.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		}
	}()

	wg.Wait()
	_ = clientConn.Close()
	_ = targetConn.Close()

	s.logTunnel(r, clientConn, targetConn, c2t, t2c, time.Since(start))
}

// copyResult is one direction of a CONNECT tunnel.
type copyResult struct {
	bytes int64
	err   error
	end   time.Duration // elapsed time from tunnel start until this copy returned
}

// logTunnel emits exactly one line per CONNECT tunnel. upstream is the resolved
// address, which makes front-end IP rotation visible; client2target and
// target2client carry the outcome and end time of each direction so a half-close
// can be told apart from a reset.
func (s *Server) logTunnel(r *http.Request, clientConn, targetConn net.Conn, c2t, t2c copyResult, duration time.Duration) {
	outcome := "ok"
	switch {
	case c2t.err != nil || t2c.err != nil:
		outcome = "aborted"
	case c2t.bytes == 0 && t2c.bytes == 0:
		outcome = "empty"
	}

	line := fmt.Sprintf("[%s] source=%s target=%s upstream=%s sent=%d recv=%d duration=%v outcome=%s client2target=%s@%v target2client=%s@%v",
		r.Method, clientConn.RemoteAddr(), r.Host, targetConn.RemoteAddr(),
		c2t.bytes, t2c.bytes, duration, outcome,
		errKind(c2t.err), c2t.end.Round(time.Millisecond),
		errKind(t2c.err), t2c.end.Round(time.Millisecond))
	if c2t.err != nil {
		line += fmt.Sprintf(" client2target_err=%q", c2t.err.Error())
	}
	if t2c.err != nil {
		line += fmt.Sprintf(" target2client_err=%q", t2c.err.Error())
	}
	if s.config.Debug {
		line += fmt.Sprintf(" client_local=%s upstream_local=%s", clientConn.LocalAddr(), targetConn.LocalAddr())
	}
	log.Print(line)
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
