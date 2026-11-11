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
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	iplocationmatchv1 "iplocationmatch/gen/iplocationmatch/v1"
	"iplocationmatch/internal/grpcapi"
	"iplocationmatch/internal/httpapi"
	"iplocationmatch/internal/iplocationmatch"
)

const shutdownTimeout = 10 * time.Second

func main() {
	var (
		dbPath   = flag.String("db", envOr("IPLOCATIONMATCH_DB", "data/GeoLite2-Country.mmdb"), "path to GeoLite2-Country .mmdb file (env IPLOCATIONMATCH_DB)")
		httpAddr = flag.String("http-addr", envOr("IPLOCATIONMATCH_HTTP_ADDR", ":8080"), "HTTP listen address (env IPLOCATIONMATCH_HTTP_ADDR)")
		grpcAddr = flag.String("grpc-addr", envOr("IPLOCATIONMATCH_GRPC_ADDR", ":9090"), "gRPC listen address (env IPLOCATIONMATCH_GRPC_ADDR)")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, log, *dbPath, *httpAddr, *grpcAddr); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, dbPath, httpAddr, grpcAddr string) error {
	checker, err := iplocationmatch.Open(dbPath)
	if err != nil {
		return err
	}
	defer checker.Close()

	httpLis, err := net.Listen("tcp", httpAddr)
	if err != nil {
		return fmt.Errorf("listen http: %w", err)
	}
	grpcLis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		_ = httpLis.Close()
		return fmt.Errorf("listen grpc: %w", err)
	}

	httpSrv := &http.Server{
		Handler:           httpapi.NewHandler(checker, log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	grpcSrv := grpc.NewServer()
	iplocationmatchv1.RegisterIPLocationMatchServiceServer(grpcSrv, grpcapi.NewServer(checker, log))

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
		// Fresh context: gctx is already cancelled, but draining needs time.
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		done := make(chan struct{})
		go func() { grpcSrv.GracefulStop(); close(done) }()
		herr := httpSrv.Shutdown(sctx)
		select {
		case <-done:
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
