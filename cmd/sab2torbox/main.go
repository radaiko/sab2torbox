// Command sab2torbox bridges Sonarr/Radarr's SABnzbd download client to the
// TorBox Usenet API, exposing completed files via a TorBox WebDAV mount.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/radaiko/sab2torbox/internal/api"
	"github.com/radaiko/sab2torbox/internal/config"
	"github.com/radaiko/sab2torbox/internal/store"
	"github.com/radaiko/sab2torbox/internal/torbox"
	"github.com/radaiko/sab2torbox/internal/worker"
)

// version is the build identifier shown in the startup log. It is "dev" for
// local builds and is overridden at release time via
// -ldflags "-X main.version=<n>", where <n> is the CI build number that
// increments on every published image.
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck())
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

// run loads config, starts workers and the HTTP server, and blocks until a
// shutdown signal arrives.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.SlogLevel()}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := worker.EnsureCategoryDirs(cfg.SymlinkRoot, cfg.Categories); err != nil {
		return fmt.Errorf("preparing symlink category directories: %w", err)
	}

	st, err := store.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer func() { _ = st.Close() }()

	tb := torbox.New(cfg.TorBoxAPIToken)
	workers := worker.New(st, tb, cfg, logger)
	srv := api.NewServer(st, cfg, logger)
	srv.SetHealth(api.NewHealth(st, tb, 5*time.Minute))
	srv.SetHealReporter(workers)

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		workers.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutCtx); err != nil {
			logger.Error("http shutdown", "error", err)
		}
	}()

	logger.Info("sab2torbox started",
		"version", version,
		"listen_addr", cfg.ListenAddr, "usenet_path", cfg.UsenetPath(),
		"poll_interval", cfg.PollInterval.String())

	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		stop()
		wg.Wait()
		return fmt.Errorf("http server: %w", err)
	}
	wg.Wait()
	logger.Info("sab2torbox stopped")
	return nil
}

// runHealthcheck implements the `healthcheck` subcommand: it GETs the
// service's own /healthz so the distroless container HEALTHCHECK works
// without a shell. It returns a process exit code.
func runHealthcheck() int {
	addr := os.Getenv("SAB2TORBOX_LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if addr[0] == ':' {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}
