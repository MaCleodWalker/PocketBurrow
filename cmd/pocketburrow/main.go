package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"pocketburrow/internal/burrow"
)

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func run() error {
	registry, err := burrow.OpenRegistry(env("BURROW_DATA_DIR", "./data"))
	if err != nil {
		return err
	}
	defer registry.Close()
	runtime := burrow.NewDocker(env("BURROW_DOCKER_SOCKET", "/var/run/docker.sock"),
		env("BURROW_PB_IMAGE", "pocketburrow/pocketbase:0.40.5"))
	server, err := burrow.NewServer(registry, runtime, env("BURROW_BASE_DOMAIN", "localhost"), os.Getenv("BURROW_ADMIN_TOKEN"))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reconcileCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err = server.Reconcile(reconcileCtx)
	cancel()
	if err != nil {
		return err
	}
	admin := &http.Server{Addr: env("BURROW_ADMIN_ADDR", "127.0.0.1:9090"), Handler: server.Admin(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 75 * time.Second, IdleTimeout: 60 * time.Second}
	gateway := &http.Server{Addr: env("BURROW_GATEWAY_ADDR", "127.0.0.1:8080"), Handler: server.Gateway(),
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	adminListener, err := net.Listen("tcp", admin.Addr)
	if err != nil {
		return err
	}
	defer adminListener.Close()
	gatewayListener, err := net.Listen("tcp", gateway.Addr)
	if err != nil {
		return err
	}
	defer gatewayListener.Close()
	errs := make(chan error, 2)
	go func() { errs <- admin.Serve(adminListener) }()
	go func() { errs <- gateway.Serve(gatewayListener) }()
	slog.Info("PocketBurrow listening", "admin", admin.Addr, "gateway", gateway.Addr)
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errs:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	adminErr := admin.Shutdown(shutdownCtx)
	gatewayErr := gateway.Shutdown(shutdownCtx)
	if adminErr != nil {
		admin.Close()
	}
	if gatewayErr != nil {
		gateway.Close()
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, adminErr, gatewayErr)
}

func main() {
	if err := run(); err != nil {
		slog.Error("PocketBurrow stopped", "error", err)
		fmt.Fprintln(os.Stderr, "See README.md for Docker permissions and configuration.")
		os.Exit(1)
	}
}
