package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rohankamewar2/global-context-auralink-iil/internal/contextapi"
	"github.com/rohankamewar2/global-context-auralink-iil/internal/database"
	"github.com/rohankamewar2/global-context-auralink-iil/internal/sarvam"
)

func main() {
	if err := run(); err != nil {
		// Connection errors can contain credentials; report only a safe message.
		slog.Error("server stopped", "reason", err.Error())
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := database.Open(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
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
	apiConfig, err := contextapi.LoadConfig()
	if err != nil {
		return err
	}
	if apiConfig != nil {
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("private dataset demo API must use a loopback HTTP_HOST")
		}
		if err := contextapi.Register(mux, *apiConfig, contextapi.PGStore{Pool: pool}); err != nil {
			return err
		}
		if os.Getenv("SARVAM_VOICE_API_KEY") != "" {
			voiceConfig, err := sarvam.LoadConfig()
			if err != nil {
				return err
			}
			if err := contextapi.RegisterBootstrap(mux, *apiConfig, contextapi.PGStore{Pool: pool}, voiceConfig); err != nil {
				return err
			}
			slog.Info("Sarvam variable bootstrap enabled", "sdk_contract_version", sarvam.SDKContractVersion)
		}
		slog.Info("private context API enabled", "mode", "local_demo_allowlist", "subject_roles", len(apiConfig.Subjects))
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
