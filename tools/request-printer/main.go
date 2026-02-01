package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents the full configuration file structure
type Config struct {
	RequestPrinter RequestPrinterConfig `yaml:"request_printer"`
}

// RequestPrinterConfig represents request printer configuration
type RequestPrinterConfig struct {
	Addr        string       `yaml:"addr"`
	MaxBodyRead int64        `yaml:"max_body_read"`
	Server      ServerConfig `yaml:"server"`
}

// ServerConfig represents HTTP server settings
type ServerConfig struct {
	ReadTimeout       time.Duration `yaml:"read_timeout"`
	WriteTimeout      time.Duration `yaml:"write_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	MaxHeaderBytes    int           `yaml:"max_header_bytes"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
}

var (
	configFile = flag.String("config", "", "Path to config file (YAML)")
	listenAddr = flag.String("addr", "", "Listen address (overrides config)")
)

// Default configuration values
func defaultConfig() *Config {
	return &Config{
		RequestPrinter: RequestPrinterConfig{
			Addr:        ":9090",
			MaxBodyRead: 10 * 1024 * 1024,
			Server: ServerConfig{
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      30 * time.Second,
				IdleTimeout:       60 * time.Second,
				ReadHeaderTimeout: 10 * time.Second,
				MaxHeaderBytes:    1 << 20,
				ShutdownTimeout:   10 * time.Second,
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

// Global config reference for handler
var globalConfig *Config

// RequestInfo represents the detailed information about a request
type RequestInfo struct {
	Timestamp   string            `json:"timestamp"`
	SourceIP    string            `json:"source_ip"`
	SourcePort  string            `json:"source_port,omitempty"`
	Method      string            `json:"method"`
	URL         string            `json:"url"`
	Proto       string            `json:"proto"`
	Host        string            `json:"host"`
	RequestURI  string            `json:"request_uri"`
	Headers     map[string]string `json:"headers"`
	BodySize    int64             `json:"body_size"`
	BodyPreview string            `json:"body_preview"`
	RemoteAddr  string            `json:"remote_addr"`
}

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
		cfg.RequestPrinter.Addr = *listenAddr
	}

	// Set global config for handler
	globalConfig = cfg

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if *configFile != "" {
		log.Printf("Loaded configuration from %s", *configFile)
	}
	log.Printf("Starting request-printer service on %s", cfg.RequestPrinter.Addr)

	server := &http.Server{
		Addr:              cfg.RequestPrinter.Addr,
		Handler:           http.HandlerFunc(handleRequest),
		ReadTimeout:       cfg.RequestPrinter.Server.ReadTimeout,
		WriteTimeout:      cfg.RequestPrinter.Server.WriteTimeout,
		IdleTimeout:       cfg.RequestPrinter.Server.IdleTimeout,
		ReadHeaderTimeout: cfg.RequestPrinter.Server.ReadHeaderTimeout,
		MaxHeaderBytes:    cfg.RequestPrinter.Server.MaxHeaderBytes,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("Shutting down request-printer service...")
		ctx, cancel := context.WithTimeout(context.Background(), cfg.RequestPrinter.Server.ShutdownTimeout)
		defer cancel()
		server.Shutdown(ctx)
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Failed to start server: %v", err)
	}
	log.Println("Server stopped")
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	timestamp := time.Now().Format(time.RFC3339Nano)

	// Extract source IP and port
	sourceIP, sourcePort, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		sourceIP = r.RemoteAddr
		sourcePort = ""
	}

	// Check for X-Forwarded-For header (if coming through proxy)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Take the first IP in the chain (original client)
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			sourceIP = strings.TrimSpace(parts[0]) + " (via X-Forwarded-For)"
		}
	}

	// Collect all headers
	headers := make(map[string]string)
	for name, values := range r.Header {
		headers[name] = strings.Join(values, ", ")
	}

	// Read body with limit
	var bodyBytes []byte
	if r.Body != nil {
		limitReader := io.LimitReader(r.Body, globalConfig.RequestPrinter.MaxBodyRead)
		bodyBytes, _ = io.ReadAll(limitReader)
		r.Body.Close()
	}

	bodySize := int64(len(bodyBytes))
	bodyPreview := formatBodyPreview(bodyBytes)

	// Build request info
	info := RequestInfo{
		Timestamp:   timestamp,
		SourceIP:    sourceIP,
		SourcePort:  sourcePort,
		Method:      r.Method,
		URL:         r.URL.String(),
		Proto:       r.Proto,
		Host:        r.Host,
		RequestURI:  r.RequestURI,
		Headers:     headers,
		BodySize:    bodySize,
		BodyPreview: bodyPreview,
		RemoteAddr:  r.RemoteAddr,
	}

	// Marshal to JSON with indentation for readability
	jsonData, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		log.Printf("Error marshaling JSON: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Log to console
	log.Printf("\n========== Incoming Request ==========\n%s\n=======================================\n", string(jsonData))

	// Return JSON response
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(jsonData)
}

// formatBodyPreview formats the body content for preview
// If total characters <= 100, print all
// Otherwise, print first 50 + "..." + last 50
func formatBodyPreview(body []byte) string {
	if len(body) == 0 {
		return ""
	}

	// Convert to string (handle UTF-8)
	bodyStr := string(body)
	runes := []rune(bodyStr)

	if len(runes) <= 100 {
		return bodyStr
	}

	// Get first 50 and last 50 characters
	first50 := string(runes[:50])
	last50 := string(runes[len(runes)-50:])

	return first50 + "..." + last50
}
