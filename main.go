package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"domain-forwarder/pkg/forwarder"
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
	engine := forwarder.New(cfg.Forward.EngineConfig(false))

	handler := proxy.New(engine, proxy.Config{
		Debug:             cfg.Debug,
		AddProxyHeaders:   cfg.AddProxyHeaders,
		SourceIPBlacklist: cfg.SourceIPBlacklist,
		DialTimeout:       cfg.Forward.DialTimeout,
		DialKeepAlive:     cfg.Forward.DialKeepAlive,
	})

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
	// CONNECT tunnels are hijacked and not tracked by Shutdown; they end when
	// the process exits.
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
