// Command excalidraw-wopi runs the WOPI editor service for excalidraw.
package main

import (
	"context"
	"errors"
	"log/slog"
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

const shutdownTimeout = 10 * time.Second

// goreleaser injects these values through ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := run(); err != nil {
		slog.Error("service stopped", "error", err)
		os.Exit(1)
	}
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
