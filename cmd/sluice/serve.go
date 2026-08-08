package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/sluice-gw/sluice/internal/audit"
	"github.com/sluice-gw/sluice/internal/config"
	"github.com/sluice-gw/sluice/internal/gateway"
	"github.com/sluice-gw/sluice/internal/server"
	"github.com/sluice-gw/sluice/internal/telemetry"
)

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: sluice serve [flags]

Runs the gateway. With no --config it uses the built-in defaults, which are a
complete working deployment against the offline local provider: two upstreams,
three route aliases and one demo key (sk-sluice-local-demo). That is enough to
try the whole pipeline without an API key or a network.

Precedence, highest first: flags, environment, config file, built-in defaults.

flags:
`)
		fs.PrintDefaults()
		fmt.Fprint(fs.Output(), "\nenvironment:\n  "+strings.Join(config.EnvVars(), "\n  ")+"\n")
	}

	var (
		path        = fs.String("config", os.Getenv(config.EnvPrefix+"CONFIG"), "path to a YAML config file")
		addr        = fs.String("addr", "", "listen address, e.g. :8080")
		logLevel    = fs.String("log-level", "", "debug, info, warn or error")
		logFormat   = fs.String("log-format", "", "json or text")
		auditPath   = fs.String("audit", "", "audit log path, or - for stdout")
		dashboard   = fs.Bool("dashboard", true, "serve the operator dashboard at /")
		printConfig = fs.Bool("print-config", false, "print the effective configuration and exit")
		checkOnly   = fs.Bool("check", false, "validate the configuration and exit")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	// Flags last, and only the ones actually present on the command line.
	// Comparing against the zero value instead would make it impossible to set
	// a flag back to its default in order to override a file.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "addr":
			cfg.Server.Addr = *addr
		case "log-level":
			cfg.Telemetry.LogLevel = *logLevel
		case "log-format":
			cfg.Telemetry.LogFormat = *logFormat
		case "audit":
			cfg.Audit.Path, cfg.Audit.Enabled = *auditPath, true
		case "dashboard":
			cfg.Server.Dashboard = *dashboard
		}
	})
	if err := cfg.Validate(); err != nil {
		return err
	}

	if *printConfig {
		redacted := cfg.Redacted()
		b, err := redacted.Marshal()
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(b)
		return err
	}
	if *checkOnly {
		fmt.Println("configuration is valid")
		return nil
	}

	return serve(cfg)
}

// loadConfig applies the layers below the flags: defaults, then file, then
// environment.
func loadConfig(path string) (config.Config, error) {
	cfg := config.Default()
	if path != "" {
		loaded, err := config.Load(path)
		if err != nil {
			return config.Config{}, err
		}
		cfg = loaded
	}
	if err := cfg.ApplyEnv(nil); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func serve(cfg config.Config) error {
	log, err := telemetry.NewLogger(os.Stderr, cfg.Telemetry.LogLevel, cfg.Telemetry.LogFormat)
	if err != nil {
		return err
	}

	// A private registry rather than the default one: prometheus's default
	// registry is package-level mutable state shared with every library in the
	// process, and a duplicate registration in a dependency would panic at
	// startup for reasons unrelated to Sluice.
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	metrics, err := telemetry.NewMetrics(reg)
	if err != nil {
		return err
	}

	var auditor audit.Recorder = audit.Nop{}
	var closeAudit func() error
	if cfg.Audit.Enabled {
		w, openErr := audit.Open(cfg.Audit.Path, cfg.Audit.Sync)
		if openErr != nil {
			return openErr
		}
		auditor, closeAudit = w, w.Close
	}

	gw, err := gateway.New(gateway.Options{
		Config: cfg, Metrics: metrics, Logger: log, Auditor: auditor,
	})
	if err != nil {
		return err
	}

	srv, err := server.New(server.Options{
		Gateway:           gw,
		Logger:            log,
		Registry:          reg,
		Addr:              cfg.Server.Addr,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.D(),
		RequestTimeout:    cfg.Server.RequestTimeout.D(),
		ShutdownGrace:     cfg.Server.ShutdownGrace.D(),
		MaxRequestBytes:   cfg.Server.MaxRequestBytes,
		MetricsPath:       cfg.Telemetry.MetricsPath,
		Dashboard:         cfg.Server.Dashboard,
		Version:           version,
	})
	if err != nil {
		return err
	}

	// SIGINT and SIGTERM start a graceful shutdown; a second signal is taken as
	// "I meant it" and the process exits without waiting for in-flight streams.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sweep := time.NewTicker(sweepInterval(cfg))
	defer sweep.Stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sweep.C:
				gw.Sweep()
			}
		}
	}()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	log.Info("sluice started",
		"version", version, "addr", cfg.Server.Addr,
		"routes", len(cfg.Routes), "providers", len(cfg.Providers), "keys", len(cfg.Keys),
		"redaction", cfg.Redaction.Enabled, "cache", cfg.Cache.Enabled, "semantic_cache", cfg.Cache.Semantic)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down", "grace", cfg.Server.ShutdownGrace.String())
	}

	shutdownErr := srv.Shutdown(context.Background())
	<-done
	if closeAudit != nil {
		if err := closeAudit(); err != nil && shutdownErr == nil {
			shutdownErr = err
		}
	}
	if shutdownErr != nil && !errors.Is(shutdownErr, context.DeadlineExceeded) {
		return shutdownErr
	}
	log.Info("stopped")
	return nil
}

// sweepInterval is how often the bounded-memory structures are swept. It
// follows the cache's configured interval when there is one, because the cache
// is the largest of them; a minute otherwise.
func sweepInterval(cfg config.Config) time.Duration {
	if d := cfg.Cache.SweepInterval.D(); d > 0 {
		return d
	}
	return time.Minute
}
