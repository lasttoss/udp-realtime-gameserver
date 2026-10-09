// Command relay runs the UDP realtime tier and the HTTP gateway next to it.
//
//	UDP  :9000   game clients (HELLO / INPUT / PING)
//	HTTP :8080   POST /v1/auth, GET /v1/stats, /v1/rooms, /metrics, / (canvas viewer)
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

	"github.com/lasttoss/udp-realtime-gameserver/internal/gateway"
	"github.com/lasttoss/udp-realtime-gameserver/internal/relay"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(logger)

	udpAddr := env("RELAY_ADDR", ":9000")
	httpAddr := env("HTTP_ADDR", ":8080")
	secret := []byte(env("RELAY_SECRET", "dev-secret-change-me"))

	cfg := relay.DefaultConfig(secret)
	cfg.MaxRooms = envInt("MAX_ROOMS", cfg.MaxRooms)
	cfg.IdleTimeout = envDuration("ROOM_IDLE_TIMEOUT", cfg.IdleTimeout)
	cfg.GhostDropAfter = envDuration("GHOST_DROP_AFTER", cfg.GhostDropAfter)
	cfg.Logger = logger

	r := relay.New(cfg)
	if err := r.Listen(udpAddr); err != nil {
		logger.Error("cannot bind the udp socket", "addr", udpAddr, "err", err)
		os.Exit(1)
	}

	// The address handed to clients cannot be the wildcard the socket is bound to: a client
	// asking "where do I connect" needs something routable, so it is configurable.
	advertise := env("RELAY_ADVERTISE", advertisedAddr(udpAddr, r.LocalAddr().String()))
	gw := gateway.New(gateway.DefaultConfig(secret, advertise), r)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := &http.Server{
		Addr:              httpAddr,
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("http gateway listening", "addr", httpAddr, "relay", r.LocalAddr().String())
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rooms, players := r.Counts()
				logger.Info("relay state", "rooms", rooms, "players", players)
			}
		}
	}()

	if err := r.Serve(ctx); err != nil {
		logger.Error("relay stopped", "err", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	logger.Info("stopped")
}

// advertisedAddr turns a wildcard bind address into something a client can dial.
func advertisedAddr(bind, actual string) string {
	host, port, err := net.SplitHostPort(actual)
	if err != nil {
		return actual
	}
	if host == "" || host == "::" || host == "0.0.0.0" {
		if _, bindPort, err := net.SplitHostPort(bind); err == nil && bindPort != "" {
			port = bindPort
		}
		return "127.0.0.1:" + port
	}
	return actual
}

func logLevel() slog.Level {
	if env("LOG_LEVEL", "info") == "debug" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscan(v, &n); err == nil {
			return n
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
