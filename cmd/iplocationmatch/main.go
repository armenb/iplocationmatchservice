// Command iplocationmatch serves the IP-in-countries check over HTTP and gRPC, backed
// by a read-only MaxMind GeoLite2-Country database.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	iplocationmatchv1 "iplocationmatchservice/gen/iplocationmatch/v1"
	"iplocationmatchservice/internal/grpcapi"
	"iplocationmatchservice/internal/httpapi"
	"iplocationmatchservice/internal/iplocationmatch"
	"iplocationmatchservice/internal/statsd"
	"iplocationmatchservice/internal/telemetry"
)

const shutdownTimeout = 10 * time.Second

// config is the service's runtime configuration.
type config struct {
	dbPath, httpAddr, grpcAddr, statsdAddr string
	maxAllowedCountries                    int
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	maxCountries, err := envInt("IPLOCATIONMATCH_MAX_ALLOWED_COUNTRIES", iplocationmatch.DefaultMaxCountries)
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}

	var cfg config
	flag.StringVar(&cfg.dbPath, "db", envOr("IPLOCATIONMATCH_DB", "data/GeoLite2-Country.mmdb"), "path to GeoLite2-Country .mmdb file (env IPLOCATIONMATCH_DB)")
	flag.StringVar(&cfg.httpAddr, "http-addr", envOr("IPLOCATIONMATCH_HTTP_ADDR", ":8080"), "HTTP listen address (env IPLOCATIONMATCH_HTTP_ADDR)")
	flag.StringVar(&cfg.grpcAddr, "grpc-addr", envOr("IPLOCATIONMATCH_GRPC_ADDR", ":9090"), "gRPC listen address (env IPLOCATIONMATCH_GRPC_ADDR)")
	flag.StringVar(&cfg.statsdAddr, "statsd-addr", envOr("IPLOCATIONMATCH_STATSD_ADDR", ""), "DogStatsD UDP address host:port; empty disables metrics (env IPLOCATIONMATCH_STATSD_ADDR)")
	flag.IntVar(&cfg.maxAllowedCountries, "max-allowed-countries", maxCountries, "longest allowed_countries list accepted; longer lists are rejected (env IPLOCATIONMATCH_MAX_ALLOWED_COUNTRIES)")
	flag.Parse()

	if err := run(log, cfg); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, cfg config) error {
	if cfg.maxAllowedCountries < 1 {
		return fmt.Errorf("max-allowed-countries must be at least 1, got %d", cfg.maxAllowedCountries)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	checker, err := iplocationmatch.Open(cfg.dbPath)
	if err != nil {
		return err
	}

	// Close the database only after a clean drain. After a forced stop a handler
	// may still be mid-lookup and unmapping under it would fault; the process is
	// about to exit, so the OS reclaims the mapping instead. drained is written
	// by the shutdown goroutine and read after g.Wait(), which orders the two.
	drained := false
	defer func() {
		if drained {
			_ = checker.Close()
		}
	}()

	// The database build date is on every request log line and every metric, so
	// operators (and a canary rollout) can tell which data a pod is serving.
	dbVersion := checker.DBVersion()
	log.Info("database loaded", "path", cfg.dbPath, "db_version", dbVersion)
	log.Info("limits", "max_allowed_countries", cfg.maxAllowedCountries)

	var sink telemetry.Sink // nil: metrics disabled
	if cfg.statsdAddr != "" {
		sc, err := statsd.New(cfg.statsdAddr, "db_version:"+dbVersion)
		if err != nil {
			return fmt.Errorf("statsd: %w", err)
		}
		// Flushes queued metrics; runs before the database is closed. A failed close at
		// exit is not actionable.
		defer func() { _ = sc.Close() }()
		sink = sc
		log.Info("metrics enabled", "statsd_addr", cfg.statsdAddr)
	}
	obs := telemetry.NewObserver(log, sink, dbVersion)

	httpLis, err := net.Listen("tcp", cfg.httpAddr)
	if err != nil {
		return fmt.Errorf("listen http: %w", err)
	}
	grpcLis, err := net.Listen("tcp", cfg.grpcAddr)
	if err != nil {
		_ = httpLis.Close()
		return fmt.Errorf("listen grpc: %w", err)
	}

	httpSrv := &http.Server{
		Handler:           httpapi.NewHandler(checker, obs, cfg.maxAllowedCountries),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	grpcSrv := grpc.NewServer()
	iplocationmatchv1.RegisterIPLocationMatchServiceServer(grpcSrv, grpcapi.NewServer(checker, obs, cfg.maxAllowedCountries))

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		log.Info("http listening", "addr", httpLis.Addr().String())
		if err := httpSrv.Serve(httpLis); !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http serve: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		log.Info("grpc listening", "addr", grpcLis.Addr().String())
		if err := grpcSrv.Serve(grpcLis); err != nil {
			return fmt.Errorf("grpc serve: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done() // signal, or one of the servers failed
		log.Info("shutting down")
		// Restore default signal handling: while draining, a second SIGINT/SIGTERM
		// should kill the process rather than be swallowed by NotifyContext.
		stop()
		// Fresh context: gctx is already cancelled, but draining needs time.
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		done := make(chan struct{})
		go func() { grpcSrv.GracefulStop(); close(done) }()
		herr := httpSrv.Shutdown(sctx)
		if herr != nil {
			// The graceful drain failed or timed out: force-close what is left,
			// mirroring the grpcSrv.Stop() fallback below.
			herr = errors.Join(herr, httpSrv.Close())
		}
		select {
		case <-done:
			drained = herr == nil
		case <-sctx.Done():
			grpcSrv.Stop()
		}
		return herr
	})
	return g.Wait()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt reads an integer from the environment, returning def if it is unset.
// A set but malformed value is an error, not a silent default.
func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}
