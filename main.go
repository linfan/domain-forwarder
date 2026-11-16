package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"domain-forwarder/pkg/forwarder"
	"domain-forwarder/pkg/mitm"
	"domain-forwarder/pkg/proxy"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Version and BuildTime are injected at build time via -ldflags.
var (
	Version   = "dev"
	BuildTime = "unknown"
)

var (
	configFile  = flag.String("config", "", "Path to config file (YAML); built-in defaults apply when omitted")
	listenAddr  = flag.String("addr", "", "Proxy listen address (overrides config)")
	versionFlag = flag.Bool("version", false, "Print version and exit")
)

func main() {
	flag.StringVar(configFile, "c", "", "Path to config file (YAML) (shorthand)")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("domain-forwarder %s (built %s)\n", Version, BuildTime)
		return
	}

	// Load configuration; without -c the built-in defaults apply.
	cfg := defaultConfig()
	if *configFile != "" {
		loaded, err := LoadConfig(*configFile)
		if err != nil {
			log.Fatalf("Failed to load config: %v", err)
		}
		cfg = loaded
	}

	// Command-line flag overrides the config file
	if *listenAddr != "" {
		cfg.Addr = *listenAddr
		if err := validateAddr(cfg.Addr); err != nil {
			log.Fatalf("Invalid -addr: %v", err)
		}
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// Redirect logs to file if configured
	if cfg.Log.Enabled() {
		logFile := filepath.Join(cfg.Log.Dir, cfg.Log.Filename)
		logger := &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    cfg.Log.MaxSize,
			MaxBackups: cfg.Log.MaxBackups,
		}
		// lumberjack reports open failures through Write and log.Logger discards
		// write errors silently, so probe the target before switching output.
		if _, err := logger.Write(nil); err != nil {
			log.Printf("WARNING: cannot open log file %s: %v; logging to stderr", logFile, err)
		} else {
			log.SetOutput(logger)
			log.Printf("Logging to file: %s (max_size=%dMB, max_backups=%d)",
				logFile, cfg.Log.MaxSize, cfg.Log.MaxBackups)
		}
	}

	log.Printf("Domain Forwarder %s (built %s)", Version, BuildTime)
	log.Printf("Loaded configuration from %s", *configFile)

	// The shared forwarding engine. A forward proxy stays transparent, so the
	// client's Accept-Encoding is kept (stripAcceptEncoding=false).
	upstreamTLS, err := cfg.Forward.UpstreamTLSConfig()
	if err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}
	if upstreamTLS != nil {
		log.Printf("Trusting additional upstream certificate authorities from %s", cfg.Forward.UpstreamCAFile)
	}
	engineConfig := cfg.Forward.EngineConfig(false)
	engineConfig.TLSClientConfig = upstreamTLS
	engine := forwarder.New(engineConfig)

	var auth *proxy.Auth
	if cfg.Auth.Enabled {
		auth = &proxy.Auth{Username: cfg.Auth.Username, Password: cfg.Auth.Password}
	}

	var editHeaders map[string]string
	if cfg.Edit.Enabled {
		editHeaders = cfg.Edit.AddHeaders
	}

	var mirror *proxy.Mirror
	if cfg.Mirror.Enabled {
		target, err := url.Parse(cfg.Mirror.Target)
		if err != nil {
			log.Fatalf("Invalid mirror.target: %v", err)
		}
		mirror = proxy.NewMirror(proxy.MirrorConfig{
			Target:       target,
			Timeout:      cfg.Mirror.Timeout,
			MaxBodyBytes: cfg.Mirror.MaxBodyBytes,
			QueueSize:    cfg.Mirror.QueueSize,
			Debug:        cfg.Debug,
		})
		log.Printf("Mirroring HTTP requests to %s", cfg.Mirror.Target)
	}

	var mitmConfig *mitm.Config
	if cfg.MITM.Enabled {
		mitmConfig = &mitm.Config{}
		for i, groupConfig := range cfg.MITM.Groups {
			name := groupConfig.Name
			if name == "" {
				name = fmt.Sprintf("group-%d", i+1)
			}
			authority, err := mitm.NewAuthority(groupConfig.CACertFile, groupConfig.CAKeyFile)
			if err != nil {
				log.Fatalf("Invalid mitm.groups[%d] (%s): %v", i, name, err)
			}
			group, err := mitm.NewGroup(name, groupConfig.Hosts, authority)
			if err != nil {
				log.Fatalf("Invalid mitm.groups[%d]: %v", i, err)
			}
			mitmConfig.Groups = append(mitmConfig.Groups, group)
		}
		logMITMWarnings(mitmConfig)
		for _, group := range mitmConfig.Groups {
			log.Printf("HTTPS interception group %q: %s", group.Name, strings.Join(group.Hosts(), ", "))
		}
	}

	handler := proxy.New(engine, proxy.Config{
		Debug:           cfg.Debug,
		AddProxyHeaders: cfg.AddProxyHeaders,
		EditHeaders:     editHeaders,
		Mirror:          mirror,
		MITM:            mitmConfig,
		AccessControl: proxy.AccessControl{
			SourceIP: proxy.AccessList{
				Blacklist: cfg.AccessControl.SourceIP.Blacklist,
				Whitelist: cfg.AccessControl.SourceIP.Whitelist,
			},
			TargetDomain: proxy.AccessList{
				Blacklist: cfg.AccessControl.TargetDomain.Blacklist,
				Whitelist: cfg.AccessControl.TargetDomain.Whitelist,
			},
		},
		Auth:                auth,
		DialTimeout:         cfg.Forward.DialTimeout,
		DialKeepAlive:       cfg.Forward.DialKeepAlive,
		TLSHandshakeTimeout: cfg.Forward.TLSHandshakeTimeout,
		ReadHeaderTimeout:   cfg.Server.ReadHeaderTimeout,
		IdleTimeout:         cfg.Server.IdleTimeout,
		MaxHeaderBytes:      cfg.Server.MaxHeaderBytes,
		TLSClientConfig:     upstreamTLS,
	})

	if len(cfg.AccessControl.TargetDomain.Whitelist) > 0 {
		log.Printf("Target domain whitelist enabled: %s", strings.Join(cfg.AccessControl.TargetDomain.Whitelist, ", "))
	}

	server := &http.Server{
		Addr:    cfg.Addr,
		Handler: handler,
		// No Read/Write timeout: CONNECT tunnels and streaming bodies may live
		// as long as the client needs.
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       cfg.Server.IdleTimeout,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		MaxHeaderBytes:    cfg.Server.MaxHeaderBytes,
	}

	// Register the signal handler before starting the listener so a signal
	// arriving during startup still triggers a graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	if cfg.TLS.Enabled {
		log.Printf("Starting HTTPS proxy server on %s (TLS)", cfg.Addr)
	} else {
		log.Printf("Starting HTTP proxy server on %s", cfg.Addr)
	}

	errCh := make(chan error, 1)
	go func() {
		var err error
		if cfg.TLS.Enabled {
			err = server.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		} else {
			err = server.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	exitCode := 0
	select {
	case <-sigCh:
		log.Println("Shutting down proxy server...")
	case serveErr := <-errCh:
		log.Printf("Fatal error: %v", serveErr)
		exitCode = 1
	}

	// Graceful shutdown: drain in-flight requests within the timeout budget.
	// Hijacked HTTP/1.1 CONNECT tunnels are not tracked by Shutdown and end
	// when the process exits; HTTP/2 CONNECT streams are tracked, so shutdown
	// waits for them up to the timeout.
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Error during shutdown: %v", err)
	}

	if exitCode != 0 {
		os.Exit(exitCode)
	}
	log.Println("Server stopped")
}

// logMITMWarnings reports catch-all and duplicated host patterns. Both are
// legal, but a "*" group makes every later group unreachable and a host listed
// twice is only ever matched by the first group.
func logMITMWarnings(config *mitm.Config) {
	owners := make(map[string]string)
	for i, group := range config.Groups {
		for _, host := range group.Hosts() {
			key := strings.ToLower(strings.TrimSuffix(host, "."))
			if previous, ok := owners[key]; ok {
				log.Printf("WARNING: mitm host %q is also configured in group %q; group %q is never matched for it",
					host, previous, group.Name)
			} else {
				owners[key] = group.Name
			}
			if host == "*" {
				log.Printf("WARNING: mitm group %q matches every host; all HTTPS traffic will be decrypted with its CA", group.Name)
				if i+1 < len(config.Groups) {
					log.Printf("WARNING: %d mitm group(s) after %q are unreachable because group %q matches every host",
						len(config.Groups)-i-1, group.Name, group.Name)
				}
			}
		}
	}
}
