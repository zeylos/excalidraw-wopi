// Command excalidraw-wopi runs the WOPI editor service for excalidraw.
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

	"github.com/zeylos/excalidraw-wopi/internal/app"
	"github.com/zeylos/excalidraw-wopi/internal/config"
	"github.com/zeylos/excalidraw-wopi/internal/proof"
	"github.com/zeylos/excalidraw-wopi/web"
)

const (
	shutdownTimeout    = 10 * time.Second
	healthcheckTimeout = 3 * time.Second
)

// goreleaser injects these values through ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	// The distroless image has no shell; this flag lets the container
	// runtime run a healthcheck with the binary itself as the only tool.
	healthcheck := flag.Bool("healthcheck", false, "check GET /healthz on the local server and exit")
	flag.Parse()

	if *healthcheck {
		if err := runHealthcheck(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	if err := run(); err != nil {
		slog.Error("service stopped", "error", err)
		os.Exit(1)
	}
}

func runHealthcheck() error {
	addr := os.Getenv("EXCALIDRAW_WOPI_LISTEN_ADDR")
	if addr == "" {
		addr = config.DefaultListenAddr
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("healthcheck: bad listen addr %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}

	url := fmt.Sprintf("http://%s/healthz", net.JoinHostPort(host, port))
	client := &http.Client{Timeout: healthcheckTimeout}

	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: status %d", resp.StatusCode)
	}
	return nil
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	proofKeys, err := proof.Load(cfg)
	if err != nil {
		return err
	}

	srv, err := app.NewServer(cfg, web.DistFS(), proofKeys)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("listening",
			"addr", cfg.ListenAddr, "public_url", cfg.PublicURL,
			"version", version, "commit", commit, "build_date", date)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return nil
}
