package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"domain-forwarder/pkg/forwarder"

	"gopkg.in/yaml.v3"
)

// TLSConfig represents the TLS configuration of the proxy's own listener.
type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// ForwardConfig represents the forwarding engine configuration. The plain
// values are prepopulated with defaults before the YAML file is overlaid, so
// explicitly configured zeros stay zero; the two *bool fields are tri-state
// (unset = default via the accessors).
type ForwardConfig struct {
	BufferSize            int           `yaml:"buffer_size"`
	MaxIdleConns          int           `yaml:"max_idle_conns"`
	MaxIdleConnsPerHost   int           `yaml:"max_idle_conns_per_host"`
	MaxConnsPerHost       int           `yaml:"max_conns_per_host"`
	IdleConnTimeout       time.Duration `yaml:"idle_conn_timeout"`
	TLSHandshakeTimeout   time.Duration `yaml:"tls_handshake_timeout"`
	ExpectContinueTimeout time.Duration `yaml:"expect_continue_timeout"`
	ResponseHeaderTimeout time.Duration `yaml:"response_header_timeout"`
	DialTimeout           time.Duration `yaml:"dial_timeout"`
	DialKeepAlive         time.Duration `yaml:"dial_keep_alive"`
	RequestTimeout        time.Duration `yaml:"request_timeout"`       // Max wait for upstream response headers; does not limit response body transfer (0 = disabled)
	StreamIdleTimeout     time.Duration `yaml:"stream_idle_timeout"`   // Abort the response body if the upstream sends no bytes for this long (0 = disabled)
	ResponseBodyTimeout   time.Duration `yaml:"response_body_timeout"` // Total duration cap for a non-streaming response body (0 = disabled)
	StreamingFlush        *bool         `yaml:"streaming_flush"`       // Flush streaming (SSE) responses chunk by chunk (default true)
	DisableCompression    *bool         `yaml:"disable_compression"`   // Never negotiate compression with the upstream (default true)
	ForceAttemptHTTP2     bool          `yaml:"force_attempt_http2"`
}

// StreamingFlushEnabled reports whether streaming responses are relayed chunk by
// chunk with an explicit flush after each chunk (default true).
func (fc *ForwardConfig) StreamingFlushEnabled() bool {
	return fc.StreamingFlush == nil || *fc.StreamingFlush
}

// CompressionDisabled reports whether compression negotiation with the upstream
// is disabled (default true). A compressing gateway buffers SSE streams in the
// compressor, so events pile up there and reach the client in bursts.
func (fc *ForwardConfig) CompressionDisabled() bool {
	return fc.DisableCompression == nil || *fc.DisableCompression
}

// Validate checks the forward section for semantic errors.
func (fc *ForwardConfig) Validate() error {
	if fc.BufferSize <= 0 {
		return fmt.Errorf("forward.buffer_size must be positive, got %d", fc.BufferSize)
	}
	if fc.MaxIdleConns < 0 || fc.MaxIdleConnsPerHost < 0 || fc.MaxConnsPerHost < 0 {
		return fmt.Errorf("forward connection pool sizes must not be negative")
	}
	for _, d := range []struct {
		name  string
		value time.Duration
	}{
		{"idle_conn_timeout", fc.IdleConnTimeout},
		{"tls_handshake_timeout", fc.TLSHandshakeTimeout},
		{"expect_continue_timeout", fc.ExpectContinueTimeout},
		{"response_header_timeout", fc.ResponseHeaderTimeout},
		{"dial_timeout", fc.DialTimeout},
		{"dial_keep_alive", fc.DialKeepAlive},
		{"request_timeout", fc.RequestTimeout},
		{"stream_idle_timeout", fc.StreamIdleTimeout},
		{"response_body_timeout", fc.ResponseBodyTimeout},
	} {
		if d.value < 0 {
			return fmt.Errorf("forward.%s must not be negative, got %s", d.name, d.value)
		}
	}
	return nil
}

// EngineConfig converts the YAML forward section into the shared forwarding
// engine configuration. stripAcceptEncoding drops the client's Accept-Encoding
// before forwarding: gateways that must not let the upstream compress set it,
// transparent forward proxies leave it false.
func (fc *ForwardConfig) EngineConfig(stripAcceptEncoding bool) forwarder.Config {
	return forwarder.Config{
		BufferSize:            fc.BufferSize,
		MaxIdleConns:          fc.MaxIdleConns,
		MaxIdleConnsPerHost:   fc.MaxIdleConnsPerHost,
		MaxConnsPerHost:       fc.MaxConnsPerHost,
		IdleConnTimeout:       fc.IdleConnTimeout,
		TLSHandshakeTimeout:   fc.TLSHandshakeTimeout,
		ExpectContinueTimeout: fc.ExpectContinueTimeout,
		ResponseHeaderTimeout: fc.ResponseHeaderTimeout,
		DialTimeout:           fc.DialTimeout,
		DialKeepAlive:         fc.DialKeepAlive,
		RequestTimeout:        fc.RequestTimeout,
		StreamIdleTimeout:     fc.StreamIdleTimeout,
		ResponseBodyTimeout:   fc.ResponseBodyTimeout,
		StreamingFlush:        fc.StreamingFlushEnabled(),
		StripAcceptEncoding:   stripAcceptEncoding,
		DisableCompression:    fc.CompressionDisabled(),
		ForceAttemptHTTP2:     fc.ForceAttemptHTTP2,
	}
}

// ServerConfig represents HTTP server settings.
type ServerConfig struct {
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	MaxHeaderBytes    int           `yaml:"max_header_bytes"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
}

// LogConfig represents the logging configuration.
// When both Dir and Filename are set, all runtime logs are written to the file
// with size-based rotation; otherwise logs go to stderr.
type LogConfig struct {
	Dir        string `yaml:"dir"`         // Directory for log files
	Filename   string `yaml:"filename"`    // Log file name
	MaxSize    int    `yaml:"max_size"`    // Max size in MB of a single log file before rotation
	MaxBackups int    `yaml:"max_backups"` // Max number of rotated log files to keep
}

// Enabled reports whether file logging is enabled
func (lc *LogConfig) Enabled() bool {
	return lc.Dir != "" && lc.Filename != ""
}

// Config represents the application configuration.
type Config struct {
	Addr              string        `yaml:"addr"`
	Debug             bool          `yaml:"debug"`
	AddProxyHeaders   bool          `yaml:"add_proxy_headers"`   // Add X-Forwarded-For/Proto/Host when forwarding
	SourceIPBlacklist []string      `yaml:"source_ip_blacklist"` // Client IPs or CIDR ranges rejected with 403
	Forward           ForwardConfig `yaml:"forward"`
	Server            ServerConfig  `yaml:"server"`
	Log               LogConfig     `yaml:"log"`
	TLS               TLSConfig     `yaml:"tls"`
}

// defaultForwardConfig returns the built-in forward section defaults. The
// values mirror pkg/forwarder.DefaultConfig and the sibling project
// domain-dispatcher; keep all three in sync.
func defaultForwardConfig() ForwardConfig {
	return ForwardConfig{
		BufferSize:            32 * 1024,
		MaxIdleConns:          1000,
		MaxIdleConnsPerHost:   100,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DialTimeout:           10 * time.Second,
		DialKeepAlive:         30 * time.Second,
		RequestTimeout:        60 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}

// defaultConfig returns the full built-in defaults; the YAML file is overlaid
// on top, so values omitted there keep these defaults.
func defaultConfig() *Config {
	return &Config{
		Addr:    ":8080",
		Forward: defaultForwardConfig(),
		Server: ServerConfig{
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    1 << 20,
			ShutdownTimeout:   30 * time.Second,
		},
		Log: LogConfig{MaxSize: 100, MaxBackups: 10},
	}
}

// LoadConfig loads configuration from a YAML file, overlaying it on the built-in
// defaults and validating the result.
func LoadConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	cfg := defaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks the configuration for semantic errors.
func (cfg *Config) Validate() error {
	if err := validateAddr(cfg.Addr); err != nil {
		return err
	}

	if err := cfg.Forward.Validate(); err != nil {
		return err
	}

	if cfg.Server.MaxHeaderBytes <= 0 {
		return fmt.Errorf("server.max_header_bytes must be positive, got %d", cfg.Server.MaxHeaderBytes)
	}
	if cfg.Server.ReadHeaderTimeout < 0 || cfg.Server.IdleTimeout < 0 || cfg.Server.ShutdownTimeout < 0 {
		return fmt.Errorf("server timeouts must not be negative")
	}

	for _, entry := range cfg.SourceIPBlacklist {
		if err := validateBlacklistEntry(entry); err != nil {
			return fmt.Errorf("source_ip_blacklist: %w", err)
		}
	}

	if cfg.TLS.Enabled {
		if cfg.TLS.CertFile == "" {
			return fmt.Errorf("tls.cert_file is required when tls is enabled")
		}
		if cfg.TLS.KeyFile == "" {
			return fmt.Errorf("tls.key_file is required when tls is enabled")
		}
		if _, err := os.Stat(cfg.TLS.CertFile); os.IsNotExist(err) {
			return fmt.Errorf("tls cert file not found: %s", cfg.TLS.CertFile)
		}
		if _, err := os.Stat(cfg.TLS.KeyFile); os.IsNotExist(err) {
			return fmt.Errorf("tls key file not found: %s", cfg.TLS.KeyFile)
		}
	}

	return nil
}

// validateAddr checks that addr is a host:port listen address with a valid port.
func validateAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid addr %q: %w", addr, err)
	}
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum < 1 || portNum > 65535 {
		return fmt.Errorf("invalid addr %q: port must be between 1 and 65535", addr)
	}
	return nil
}

// validateBlacklistEntry checks a single source_ip_blacklist entry, which may be
// an individual IP or a CIDR range.
func validateBlacklistEntry(entry string) error {
	if strings.Contains(entry, "/") {
		if _, _, err := net.ParseCIDR(entry); err != nil {
			return fmt.Errorf("invalid CIDR range %q", entry)
		}
		return nil
	}
	if net.ParseIP(entry) == nil {
		return fmt.Errorf("invalid IP address %q", entry)
	}
	return nil
}
