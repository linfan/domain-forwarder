// Package proxy implements the forward proxy handler: plain HTTP requests are
// relayed through the forwarding engine, CONNECT requests get a raw TCP tunnel.
package proxy

import (
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
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

	// Bidirectional copy reusing the engine's buffer pool
	buffers := s.engine.Buffers()
	var wg sync.WaitGroup
	wg.Add(2)

	var clientToTarget, targetToClient int64
	var clientToTargetErr, targetToClientErr error

	go func() {
		defer wg.Done()
		buf := buffers.Get()
		defer buffers.Put(buf)
		clientToTarget, clientToTargetErr = io.CopyBuffer(targetConn, clientConn, buf)
		if tcpConn, ok := targetConn.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		buf := buffers.Get()
		defer buffers.Put(buf)
		targetToClient, targetToClientErr = io.CopyBuffer(clientConn, targetConn, buf)
		if tcpConn, ok := clientConn.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		}
	}()

	wg.Wait()
	_ = clientConn.Close()
	_ = targetConn.Close()

	switch {
	case clientToTargetErr != nil || targetToClientErr != nil:
		log.Printf("[%s] source=%s target=%s tunnel aborted: client->target: %v, target->client: %v (sent=%d recv=%d duration=%v)",
			r.Method, r.RemoteAddr, r.Host, clientToTargetErr, targetToClientErr, clientToTarget, targetToClient, time.Since(start))
	case clientToTarget == 0 && targetToClient == 0:
		log.Printf("[%s] source=%s target=%s tunnel closed with no data exchanged (duration=%v)",
			r.Method, r.RemoteAddr, r.Host, time.Since(start))
	}

	log.Printf("[%s] source=%s target=%s sent=%d recv=%d duration=%v",
		r.Method, r.RemoteAddr, r.Host, clientToTarget, targetToClient, time.Since(start))
}

// logSummary prints the unified per-request summary line.
func logSummary(r *http.Request, target string, result forwarder.Result) {
	log.Printf("[%s] source=%s target=%s req_size=%d resp_status=%d resp_size=%d duration=%v",
		r.Method, r.RemoteAddr, target, r.ContentLength, result.StatusCode, result.RespSize, result.Duration)
}
