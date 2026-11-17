package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"domain-forwarder/pkg/forwarder"
	"domain-forwarder/pkg/mitm"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// TLSConfig represents the TLS configuration of the proxy's own listener.
type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// AuthConfig configures HTTP Basic authentication for proxy clients. Password
// is either plaintext or a bcrypt hash starting with "$2".
type AuthConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// EditConfig configures request edits applied before forwarding. AddHeaders
// sets (overwrites) headers on every forwarded request. HTTPS requests are only
// editable when their CONNECT target matches a mitm group.
type EditConfig struct {
	Enabled    bool              `yaml:"enabled"`
	AddHeaders map[string]string `yaml:"add_headers"`
}

// MITMGroupConfig is one interception group: the hosts it covers and the CA
// that signs the certificates presented to clients for those hosts.
type MITMGroupConfig struct {
	Name       string   `yaml:"name"`
	CACertFile string   `yaml:"ca_cert_file"`
	CAKeyFile  string   `yaml:"ca_key_file"`
	Hosts      []string `yaml:"hosts"` // "*", "*.example.com", a bare host name or an IP literal
}

// MITMConfig configures HTTPS interception. Only CONNECT targets matching a
// group are decrypted; everything else stays an opaque TCP tunnel. Groups are
// evaluated in order and the first match wins, but TunnelConnect entries
// outrank every group.
type MITMConfig struct {
	Enabled bool `yaml:"enabled"`
	// TunnelConnect lists CONNECT authorities that must stay opaque tunnels even
	// when a group covers their host: a host pattern ("*", "*.example.com", a bare
	// host, an IP literal) with an optional ":port". An entry without a port covers
	// every port of that host. Server-speaks-first protocols (SMTP, FTP, IMAP) send
	// no ClientHello to sniff, so their port belongs here rather than in a group.
	TunnelConnect []string `yaml:"tunnel_connect"`
	// MaxCachedHosts bounds the minted leaves kept per CA. A cached leaf costs
	// tens of kilobytes, so without a limit the cache grows with every distinct
	// intercepted host. 0 disables the limit.
	MaxCachedHosts int               `yaml:"max_cached_hosts"`
	Groups         []MITMGroupConfig `yaml:"groups"`
}

// MirrorConfig configures best-effort traffic mirroring: every forwarded proxy
// request is also delivered, asynchronously, to Target. Delivery failures and
// queue overflow never affect the original request.
type MirrorConfig struct {
	Enabled      bool          `yaml:"enabled"`
	Target       string        `yaml:"target"`         // Absolute http(s) base URL; original path and query are appended
	Timeout      time.Duration `yaml:"timeout"`        // Per-delivery timeout
	MaxBodyBytes int64         `yaml:"max_body_bytes"` // Max request body captured; larger bodies are not mirrored
	QueueSize    int           `yaml:"queue_size"`     // Pending mirror jobs; overflow is dropped
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
	UpstreamCAFile        string        `yaml:"upstream_ca_file"` // Extra PEM CAs trusted for upstream HTTPS, in addition to the system trust store
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

// UpstreamTLSConfig builds the client TLS settings for every upstream HTTPS
// connection. Certificates in upstream_ca_file are trusted in addition to the
// system trust store. NextProtos is left nil on purpose: a custom TLS config no
// longer disables HTTP/2 by itself, so force_attempt_http2 stays the only
// switch, and listing h2 here would advertise a protocol the transport may not
// be configured to speak. Returns nil when no file is configured, leaving the
// engine on the system trust store.
func (fc *ForwardConfig) UpstreamTLSConfig() (*tls.Config, error) {
	if fc.UpstreamCAFile == "" {
		return nil, nil
	}
	bundle, err := os.ReadFile(fc.UpstreamCAFile)
	if err != nil {
		return nil, fmt.Errorf("forward.upstream_ca_file: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		// Platforms without a readable system store still honor the configured CA.
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(bundle) {
		return nil, fmt.Errorf("forward.upstream_ca_file %s contains no certificates", fc.UpstreamCAFile)
	}
	return &tls.Config{RootCAs: roots}, nil
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
	if _, err := fc.UpstreamTLSConfig(); err != nil {
		return err
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

// AccessListConfig is the blacklist/whitelist pair for one request attribute.
// A blacklist match rejects the request; when the whitelist is non-empty, only
// matching requests are admitted. Empty lists impose no restriction.
type AccessListConfig struct {
	Blacklist []string `yaml:"blacklist"`
	Whitelist []string `yaml:"whitelist"`
}

// AccessControlConfig gates requests by client IP and target host, each with
// its own blacklist and whitelist.
type AccessControlConfig struct {
	SourceIP     AccessListConfig `yaml:"source_ip"`     // Individual IPs or CIDR ranges
	TargetDomain AccessListConfig `yaml:"target_domain"` // Bare host names, IP literals, *.example.com or *
}

// Config represents the application configuration.
type Config struct {
	Addr            string              `yaml:"addr"`
	Debug           bool                `yaml:"debug"`
	AddProxyHeaders bool                `yaml:"add_proxy_headers"` // Add X-Forwarded-For/Proto/Host when forwarding
	AccessControl   AccessControlConfig `yaml:"access_control"`    // Client IP and target host filtering
	Auth            AuthConfig          `yaml:"auth"`
	Edit            EditConfig          `yaml:"edit"`
	Mirror          MirrorConfig        `yaml:"mirror"`
	MITM            MITMConfig          `yaml:"mitm"`
	Forward         ForwardConfig       `yaml:"forward"`
	Server          ServerConfig        `yaml:"server"`
	Log             LogConfig           `yaml:"log"`
	TLS             TLSConfig           `yaml:"tls"`
}

// defaultForwardConfig returns the built-in forward section defaults. The
// values mirror pkg/forwarder.DefaultConfig; keep the two in sync.
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
		MITM:    MITMConfig{MaxCachedHosts: mitm.DefaultMaxCachedHosts},
		Server: ServerConfig{
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    1 << 20,
			ShutdownTimeout:   30 * time.Second,
		},
		Log: LogConfig{MaxSize: 100, MaxBackups: 10},
		Mirror: MirrorConfig{
			Timeout:      5 * time.Second,
			MaxBodyBytes: 1 << 20,
			QueueSize:    256,
		},
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

	for _, list := range []struct {
		path     string
		entries  []string
		validate func(string) error
	}{
		{"access_control.source_ip.blacklist", cfg.AccessControl.SourceIP.Blacklist, validateIPEntry},
		{"access_control.source_ip.whitelist", cfg.AccessControl.SourceIP.Whitelist, validateIPEntry},
		{"access_control.target_domain.blacklist", cfg.AccessControl.TargetDomain.Blacklist, validateDomainEntry},
		{"access_control.target_domain.whitelist", cfg.AccessControl.TargetDomain.Whitelist, validateDomainEntry},
	} {
		for _, entry := range list.entries {
			if err := list.validate(entry); err != nil {
				return fmt.Errorf("%s: %w", list.path, err)
			}
		}
	}

	if cfg.Auth.Enabled {
		if cfg.Auth.Username == "" {
			return fmt.Errorf("auth.username is required when auth is enabled")
		}
		if strings.Contains(cfg.Auth.Username, ":") {
			return fmt.Errorf("auth.username must not contain ':'")
		}
		if cfg.Auth.Password == "" {
			return fmt.Errorf("auth.password is required when auth is enabled")
		}
		if strings.HasPrefix(cfg.Auth.Password, "$2") {
			if _, err := bcrypt.Cost([]byte(cfg.Auth.Password)); err != nil {
				return fmt.Errorf("auth.password is not a valid bcrypt hash: %w", err)
			}
		}
	}

	if cfg.Edit.Enabled {
		for name, value := range cfg.Edit.AddHeaders {
			if !validHeaderName(name) {
				return fmt.Errorf("edit.add_headers: invalid header name %q", name)
			}
			if strings.ContainsAny(value, "\r\n\x00") {
				return fmt.Errorf("edit.add_headers.%s: value must not contain CR, LF or NUL", name)
			}
		}
	}

	if err := cfg.Mirror.Validate(); err != nil {
		return err
	}

	if err := cfg.MITM.Validate(); err != nil {
		return err
	}

	if cfg.TLS.Enabled {
		if cfg.TLS.CertFile == "" {
			return fmt.Errorf("tls.cert_file is required when tls is enabled")
		}
		if cfg.TLS.KeyFile == "" {
			return fmt.Errorf("tls.key_file is required when tls is enabled")
		}
		if _, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile); err != nil {
			return fmt.Errorf("tls certificate/key pair: %w", err)
		}
	}

	return nil
}

// Validate checks the mirror section for semantic errors.
func (mc *MirrorConfig) Validate() error {
	if !mc.Enabled {
		return nil
	}
	if mc.Target == "" {
		return fmt.Errorf("mirror.target is required when mirror is enabled")
	}
	u, err := url.Parse(mc.Target)
	if err != nil {
		return fmt.Errorf("mirror.target: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("mirror.target %q must use http or https", mc.Target)
	}
	if u.Host == "" {
		return fmt.Errorf("mirror.target %q has no host", mc.Target)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("mirror.target %q must not contain a query or fragment", mc.Target)
	}
	if mc.Timeout <= 0 {
		return fmt.Errorf("mirror.timeout must be positive, got %s", mc.Timeout)
	}
	if mc.MaxBodyBytes < 0 {
		return fmt.Errorf("mirror.max_body_bytes must not be negative, got %d", mc.MaxBodyBytes)
	}
	if mc.QueueSize <= 0 {
		return fmt.Errorf("mirror.queue_size must be positive, got %d", mc.QueueSize)
	}
	return nil
}

// Validate checks the mitm section for semantic errors.
func (mc *MITMConfig) Validate() error {
	if !mc.Enabled {
		return nil
	}
	if mc.MaxCachedHosts < 0 {
		return fmt.Errorf("mitm.max_cached_hosts must not be negative, got %d", mc.MaxCachedHosts)
	}
	for _, entry := range mc.TunnelConnect {
		if err := mitm.ValidateConnectEntry(entry); err != nil {
			return fmt.Errorf("mitm.tunnel_connect: %w", err)
		}
	}
	if len(mc.Groups) == 0 {
		return fmt.Errorf("mitm.groups must contain at least one group when mitm is enabled")
	}
	for i, group := range mc.Groups {
		name := group.Name
		if name == "" {
			name = fmt.Sprintf("group %d", i+1)
		}
		if group.CACertFile == "" {
			return fmt.Errorf("mitm.groups[%d] (%s): ca_cert_file is required", i, name)
		}
		if group.CAKeyFile == "" {
			return fmt.Errorf("mitm.groups[%d] (%s): ca_key_file is required", i, name)
		}
		for _, path := range []string{group.CACertFile, group.CAKeyFile} {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				return fmt.Errorf("mitm.groups[%d] (%s): file not found: %s", i, name, path)
			}
		}
		if len(group.Hosts) == 0 {
			return fmt.Errorf("mitm.groups[%d] (%s): hosts must not be empty", i, name)
		}
		for _, host := range group.Hosts {
			if err := mitm.ValidateHostPattern(host); err != nil {
				return fmt.Errorf("mitm.groups[%d] (%s): %w", i, name, err)
			}
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

// validHeaderName reports whether name is an RFC 7230 token, the character set
// HTTP header field names are restricted to.
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// validateIPEntry checks a single access_control.source_ip entry, which must be
// an individual IP or a CIDR range.
func validateIPEntry(entry string) error {
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

// validateDomainEntry checks a single access_control.target_domain entry: a bare
// host name, an IP literal, "*.example.com" (subdomains only) or "*".
func validateDomainEntry(entry string) error {
	if entry == "" {
		return fmt.Errorf("domain entry must not be empty")
	}
	if strings.ContainsAny(entry, " \t\r\n") {
		return fmt.Errorf("invalid domain %q: must not contain whitespace", entry)
	}
	if entry == "*" {
		return nil
	}
	if rest, ok := strings.CutPrefix(entry, "*."); ok {
		rest = strings.TrimSuffix(rest, ".")
		if rest == "" || strings.ContainsAny(rest, "*:/@?#[]") {
			return fmt.Errorf("invalid domain %q: wildcard must look like *.example.com", entry)
		}
		if net.ParseIP(rest) != nil {
			return fmt.Errorf("invalid domain %q: wildcard cannot cover an IP literal", entry)
		}
		return nil
	}
	if strings.ContainsAny(entry, "*") {
		return fmt.Errorf("invalid domain %q: wildcard is only allowed as a leading * or *.", entry)
	}
	if strings.ContainsAny(entry, "/:@?#[]") {
		return fmt.Errorf("invalid domain %q: must be a bare host name without scheme, port or path", entry)
	}
	return nil
}
