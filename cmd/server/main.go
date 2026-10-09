package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

func main() {
	if err := run(); err != nil {
		// Connection errors can contain credentials; report only a safe message.
		slog.Error("server stopped", "reason", err.Error())
		os.Exit(1)
	}
}

func run() error {
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		return errors.New("could not load .env; check its formatting")
	}
	for _, key := range []string{"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGSSLMODE"} {
		value := os.Getenv(key)
		if value == "" || value == "REPLACE_ME" {
			return fmt.Errorf("set %s in .env or the process environment", key)
		}
	}

	// pgx reads the PG* environment variables, including passwords with special characters.
	config, err := pgxpool.ParseConfig("")
	if err != nil {
		return errors.New("invalid PostgreSQL configuration")
	}
	config.MaxConns = 5
	config.ConnConfig.ConnectTimeout = 5 * time.Second
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return errors.New("could not initialize PostgreSQL connection pool")
	}
	defer pool.Close()
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = pool.Ping(checkCtx)
	cancel()
	if err != nil {
		return errors.New("PostgreSQL connection failed; check credentials, host, port, and server status")
	}
	slog.Info("PostgreSQL connection established")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		respond(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			respond(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		respond(w, http.StatusOK, "ready")
	})
	host, port := os.Getenv("HTTP_HOST"), os.Getenv("HTTP_PORT")
	if host == "" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "8080"
	}
	server := &http.Server{
		Addr: net.JoinHostPort(host, port), Handler: mux,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second,
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return errors.New("could not listen on HTTP address; check host and port availability")
	}
	slog.Info("HTTP server started", "address", server.Addr)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("HTTP server failed")
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return errors.New("HTTP graceful shutdown timed out")
		}
	}
	return nil
}

func respond(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": message})
}
