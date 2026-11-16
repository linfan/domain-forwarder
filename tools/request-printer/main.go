package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the request printer configuration
type Config = RequestPrinterConfig

// RequestPrinterConfig represents request printer configuration
type RequestPrinterConfig struct {
	Addr        string       `yaml:"addr"`
	MaxBodyRead int64        `yaml:"max_body_read"`
	OutputDir   string       `yaml:"output_dir"`
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
	outputDir  = flag.String("output-dir", "", "Directory to save each request as .header/.body files (overrides config)")
)

// Version and BuildTime are injected at build time via -ldflags.
var (
	Version   = "dev"
	BuildTime = "unknown"
)

// Default configuration values
func defaultConfig() *Config {
	return &Config{
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
	flag.StringVar(outputDir, "o", "", "Directory to save requests (shorthand)")
	flag.Parse()

	// Load configuration
	cfg, err := loadConfig(*configFile)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Command-line flags override config file
	if *listenAddr != "" {
		cfg.Addr = *listenAddr
	}
	if *outputDir != "" {
		cfg.OutputDir = *outputDir
	}

	if cfg.OutputDir != "" {
		if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
			log.Fatalf("Failed to create output directory %s: %v", cfg.OutputDir, err)
		}
	}

	// Set global config for handler
	globalConfig = cfg

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if *configFile != "" {
		log.Printf("Loaded configuration from %s", *configFile)
	}
	log.Printf("Request Printer %s (built %s)", Version, BuildTime)
	log.Printf("Starting request-printer service on %s", cfg.Addr)
	if cfg.OutputDir != "" {
		log.Printf("Saving requests to %s", cfg.OutputDir)
	}

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           http.HandlerFunc(handleRequest),
		ReadTimeout:       cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout,
		IdleTimeout:       cfg.Server.IdleTimeout,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		MaxHeaderBytes:    cfg.Server.MaxHeaderBytes,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("Shutting down request-printer service...")
		ctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
		defer cancel()
		server.Shutdown(ctx)
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Failed to start server: %v", err)
	}
	log.Println("Server stopped")
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	receivedAt := time.Now()
	timestamp := receivedAt.Format(time.RFC3339Nano)

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
		limitReader := io.LimitReader(r.Body, globalConfig.MaxBodyRead)
		bodyBytes, _ = io.ReadAll(limitReader)
		r.Body.Close()
	}

	// Save the raw request to files when an output directory is configured
	if globalConfig.OutputDir != "" {
		baseName := receivedAt.Format("20060102-150405.000000")
		saved, err := saveRequestFiles(globalConfig.OutputDir, baseName, formatRequestHeaders(r), bodyBytes)
		if err != nil {
			log.Printf("Failed to save request files: %v", err)
		} else {
			log.Printf("Saved request to %s.{header,body}", filepath.Join(globalConfig.OutputDir, saved))
		}
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

	// Log to console, unless the full request is already saved on disk
	if globalConfig.OutputDir == "" {
		log.Printf("\n========== Incoming Request ==========\n%s\n=======================================\n", string(jsonData))
	}

	// Return JSON response
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(jsonData)
}

// saveMu serializes file name allocation so concurrent requests cannot pick
// the same base name.
var saveMu sync.Mutex

// saveRequestFiles writes the request headers and body to dir, using baseName
// (or baseName-2, baseName-3, ... on collision) as the file stem. It returns
// the stem actually used.
func saveRequestFiles(dir, baseName, headers string, body []byte) (string, error) {
	saveMu.Lock()
	defer saveMu.Unlock()

	for i := 1; ; i++ {
		name := baseName
		if i > 1 {
			name = fmt.Sprintf("%s-%d", baseName, i)
		}

		headerPath := filepath.Join(dir, name+".header")
		bodyPath := filepath.Join(dir, name+".body")
		if fileExists(headerPath) || fileExists(bodyPath) {
			continue
		}

		if err := os.WriteFile(headerPath, []byte(headers), 0o644); err != nil {
			return "", err
		}
		if err := os.WriteFile(bodyPath, body, 0o644); err != nil {
			return "", err
		}
		return name, nil
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// formatRequestHeaders renders the received request line and headers as text.
func formatRequestHeaders(r *http.Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s\n", r.Method, r.RequestURI, r.Proto)
	fmt.Fprintf(&b, "Host: %s\n", r.Host)

	names := make([]string, 0, len(r.Header))
	for name := range r.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, value := range r.Header[name] {
			fmt.Fprintf(&b, "%s: %s\n", name, value)
		}
	}
	b.WriteString("\n")
	return b.String()
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
