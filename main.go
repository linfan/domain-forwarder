package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents the full configuration file structure
type Config struct {
	Proxy ProxyConfig `yaml:"proxy"`
}

// ProxyConfig represents proxy server configuration
type ProxyConfig struct {
	Addr       string           `yaml:"addr"`
	BufferSize int              `yaml:"buffer_size"`
	Connection ConnectionConfig `yaml:"connection"`
	Transport  TransportConfig  `yaml:"transport"`
	Server     ServerConfig     `yaml:"server"`
	TLS        TLSConfig        `yaml:"tls"`
}

// ConnectionConfig represents connection settings
type ConnectionConfig struct {
	DialTimeout       time.Duration `yaml:"dial_timeout"`
	KeepAlive         time.Duration `yaml:"keep_alive"`
	TargetDialTimeout time.Duration `yaml:"target_dial_timeout"`
}

// TransportConfig represents HTTP transport settings
type TransportConfig struct {
	MaxIdleConns          int           `yaml:"max_idle_conns"`
	MaxIdleConnsPerHost   int           `yaml:"max_idle_conns_per_host"`
	MaxConnsPerHost       int           `yaml:"max_conns_per_host"`
	IdleConnTimeout       time.Duration `yaml:"idle_conn_timeout"`
	TLSHandshakeTimeout   time.Duration `yaml:"tls_handshake_timeout"`
	ExpectContinueTimeout time.Duration `yaml:"expect_continue_timeout"`
	ResponseHeaderTimeout time.Duration `yaml:"response_header_timeout"`
	ForceAttemptHTTP2     bool          `yaml:"force_attempt_http2"`
	DisableCompression    bool          `yaml:"disable_compression"`
}

// ServerConfig represents HTTP server settings
type ServerConfig struct {
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	MaxHeaderBytes    int           `yaml:"max_header_bytes"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
}

// TLSConfig represents TLS/SSL configuration for the proxy listener
type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

var (
	configFile = flag.String("config", "", "Path to config file (YAML)")
	listenAddr = flag.String("addr", "", "Proxy listen address (overrides config)")
)

// Default configuration values
func defaultConfig() *Config {
	return &Config{
		Proxy: ProxyConfig{
			Addr:       ":8080",
			BufferSize: 32 * 1024,
			Connection: ConnectionConfig{
				DialTimeout:       30 * time.Second,
				KeepAlive:         30 * time.Second,
				TargetDialTimeout: 30 * time.Second,
			},
			Transport: TransportConfig{
				MaxIdleConns:          1000,
				MaxIdleConnsPerHost:   100,
				MaxConnsPerHost:       0,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				ForceAttemptHTTP2:     true,
				DisableCompression:    false,
			},
			Server: ServerConfig{
				ReadHeaderTimeout: 10 * time.Second,
				IdleTimeout:       120 * time.Second,
				MaxHeaderBytes:    1 << 20,
				ShutdownTimeout:   30 * time.Second,
			},
		},
	}
}

// loadConfig loads configuration from file
func loadConfig(path string) (*Config, error) {
	cfg := defaultConfig()

	if path == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Buffer pool for efficient memory reuse
var bufferPool sync.Pool

func main() {
	flag.StringVar(configFile, "c", "", "Path to config file (YAML) (shorthand)")
	flag.Parse()

	// Handle -c shorthand
	if *configFile == "" {
		flag.Visit(func(f *flag.Flag) {
			if f.Name == "c" {
				*configFile = f.Value.String()
			}
		})
	}

	// Load configuration
	cfg, err := loadConfig(*configFile)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Command-line flags override config file
	if *listenAddr != "" {
		cfg.Proxy.Addr = *listenAddr
	}

	// Initialize buffer pool with configured size
	bufferPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, cfg.Proxy.BufferSize)
		},
	}

	// Validate TLS config
	if cfg.Proxy.TLS.Enabled {
		if cfg.Proxy.TLS.CertFile == "" || cfg.Proxy.TLS.KeyFile == "" {
			log.Fatalf("TLS enabled but cert_file or key_file is not configured")
		}
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if *configFile != "" {
		log.Printf("Loaded configuration from %s", *configFile)
	}
	if cfg.Proxy.TLS.Enabled {
		log.Printf("Starting HTTPS proxy server on %s (TLS)", cfg.Proxy.Addr)
	} else {
		log.Printf("Starting HTTP proxy server on %s", cfg.Proxy.Addr)
	}

	proxy := &ProxyServer{
		config: cfg.Proxy,
		transport: &http.Transport{
			Proxy: nil,
			DialContext: (&net.Dialer{
				Timeout:   cfg.Proxy.Connection.DialTimeout,
				KeepAlive: cfg.Proxy.Connection.KeepAlive,
			}).DialContext,
			ForceAttemptHTTP2:     cfg.Proxy.Transport.ForceAttemptHTTP2,
			MaxIdleConns:          cfg.Proxy.Transport.MaxIdleConns,
			MaxIdleConnsPerHost:   cfg.Proxy.Transport.MaxIdleConnsPerHost,
			MaxConnsPerHost:       cfg.Proxy.Transport.MaxConnsPerHost,
			IdleConnTimeout:       cfg.Proxy.Transport.IdleConnTimeout,
			TLSHandshakeTimeout:   cfg.Proxy.Transport.TLSHandshakeTimeout,
			ExpectContinueTimeout: cfg.Proxy.Transport.ExpectContinueTimeout,
			ResponseHeaderTimeout: cfg.Proxy.Transport.ResponseHeaderTimeout,
			DisableCompression:    cfg.Proxy.Transport.DisableCompression,
		},
	}

	server := &http.Server{
		Addr:              cfg.Proxy.Addr,
		Handler:           proxy,
		ReadTimeout:       0, // No timeout for long-lived connections
		WriteTimeout:      0,
		IdleTimeout:       cfg.Proxy.Server.IdleTimeout,
		ReadHeaderTimeout: cfg.Proxy.Server.ReadHeaderTimeout,
		MaxHeaderBytes:    cfg.Proxy.Server.MaxHeaderBytes,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("Shutting down proxy server...")
		ctx, cancel := context.WithTimeout(context.Background(), cfg.Proxy.Server.ShutdownTimeout)
		defer cancel()
		server.Shutdown(ctx)
	}()

	if cfg.Proxy.TLS.Enabled {
		if err := server.ListenAndServeTLS(cfg.Proxy.TLS.CertFile, cfg.Proxy.TLS.KeyFile); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start TLS server: %v", err)
		}
	} else {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start server: %v", err)
		}
	}
	log.Println("Server stopped")
}

// ProxyServer handles HTTP and HTTPS proxy requests
type ProxyServer struct {
	config    ProxyConfig
	transport *http.Transport
}

func (p *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	if r.Method == http.MethodConnect {
		p.handleHTTPS(w, r)
	} else {
		p.handleHTTP(w, r)
	}

	log.Printf("[%s] %s %s - %v", r.Method, r.Host, r.URL.Path, time.Since(startTime))
}

// handleHTTP handles regular HTTP proxy requests
func (p *ProxyServer) handleHTTP(w http.ResponseWriter, r *http.Request) {
	// Ensure the URL has a scheme for proxy requests
	if r.URL.Scheme == "" {
		r.URL.Scheme = "http"
	}
	if r.URL.Host == "" {
		r.URL.Host = r.Host
	}

	// Create a new request to forward
	outReq := r.Clone(r.Context())

	// Remove hop-by-hop headers
	removeHopByHopHeaders(outReq.Header)

	// Set X-Forwarded headers
	if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if prior := outReq.Header.Get("X-Forwarded-For"); prior != "" {
			clientIP = prior + ", " + clientIP
		}
		outReq.Header.Set("X-Forwarded-For", clientIP)
	}

	// Forward the request
	resp, err := p.transport.RoundTrip(outReq)
	if err != nil {
		log.Printf("Error forwarding request: %v", err)
		http.Error(w, "Proxy Error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Remove hop-by-hop headers from response
	removeHopByHopHeaders(resp.Header)

	// Copy response headers
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	// Write status code
	w.WriteHeader(resp.StatusCode)

	// Copy response body using buffer pool
	buf := bufferPool.Get().([]byte)
	defer bufferPool.Put(buf)
	io.CopyBuffer(w, resp.Body, buf)
}

// handleHTTPS handles CONNECT method for HTTPS tunneling
func (p *ProxyServer) handleHTTPS(w http.ResponseWriter, r *http.Request) {
	// Connect to the target host
	targetConn, err := net.DialTimeout("tcp", r.Host, p.config.Connection.TargetDialTimeout)
	if err != nil {
		log.Printf("Error connecting to %s: %v", r.Host, err)
		http.Error(w, "Connection failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Hijack the client connection
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		log.Printf("Hijacking not supported")
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		targetConn.Close()
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		log.Printf("Error hijacking connection: %v", err)
		http.Error(w, "Hijack failed: "+err.Error(), http.StatusInternalServerError)
		targetConn.Close()
		return
	}

	// Send 200 Connection Established
	_, err = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	if err != nil {
		log.Printf("Error sending connection established: %v", err)
		clientConn.Close()
		targetConn.Close()
		return
	}

	// Bidirectional copy
	var wg sync.WaitGroup
	wg.Add(2)

	copyConn := func(dst, src net.Conn) {
		defer wg.Done()
		buf := bufferPool.Get().([]byte)
		defer bufferPool.Put(buf)
		io.CopyBuffer(dst, src, buf)
		// Close write side to signal EOF
		if tcpConn, ok := dst.(*net.TCPConn); ok {
			tcpConn.CloseWrite()
		}
	}

	go copyConn(targetConn, clientConn)
	go copyConn(clientConn, targetConn)

	wg.Wait()
	clientConn.Close()
	targetConn.Close()
}

// Hop-by-hop headers that should not be forwarded
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
	"Proxy-Connection",
}

func removeHopByHopHeaders(h http.Header) {
	// Remove headers listed in Connection header
	if c := h.Get("Connection"); c != "" {
		for _, f := range parseCSV(c) {
			h.Del(f)
		}
	}
	// Remove standard hop-by-hop headers
	for _, hdr := range hopByHopHeaders {
		h.Del(hdr)
	}
}

func parseCSV(s string) []string {
	var result []string
	for _, v := range splitAndTrim(s, ",") {
		if v != "" {
			result = append(result, v)
		}
	}
	return result
}

func splitAndTrim(s, sep string) []string {
	var result []string
	for _, part := range splitString(s, sep) {
		trimmed := trimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func splitString(s, sep string) []string {
	var result []string
	start := 0
	for i := 0; i <= len(s)-len(sep); i++ {
		if s[i:i+len(sep)] == sep {
			result = append(result, s[start:i])
			start = i + len(sep)
			i += len(sep) - 1
		}
	}
	result = append(result, s[start:])
	return result
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
