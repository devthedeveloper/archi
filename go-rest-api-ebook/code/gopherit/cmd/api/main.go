// gopherit — a Reddit-clone REST API built with Go's standard library.
// This is the companion project for the "Go + REST: Build Your Own Reddit"
// handwritten-notes ebook.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gopherit/internal/store"
)

const version = "1.0.0"

// config holds everything tweakable from the outside world.
type config struct {
	port int
	env  string
	dsn  string
}

// application is the "toolbox" every handler can reach: config, a logger
// and the data layer. Handlers hang off it as methods.
type application struct {
	config config
	logger *slog.Logger
	store  *store.Store
}

func main() {
	var cfg config
	flag.IntVar(&cfg.port, "port", 4000, "API server port")
	flag.StringVar(&cfg.env, "env", "development", "Environment (development|production)")
	flag.StringVar(&cfg.dsn, "dsn", "gopherit.db", "SQLite database file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	st, err := store.Open(cfg.dsn)
	if err != nil {
		logger.Error("cannot open database", "error", err)
		os.Exit(1)
	}
	defer st.Close()
	logger.Info("database ready", "dsn", cfg.dsn)

	app := &application{
		config: cfg,
		logger: logger,
		store:  st,
	}

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.port),
		Handler:      app.routes(),
		IdleTimeout:  time.Minute,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Graceful shutdown: when Ctrl+C (SIGINT) or SIGTERM arrives, stop
	// accepting new requests and give in-flight ones 10s to finish.
	shutdownErr := make(chan error)
	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		sig := <-quit

		logger.Info("shutting down", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownErr <- srv.Shutdown(ctx)
	}()

	logger.Info("starting server", "addr", srv.Addr, "env", cfg.env, "version", version)

	err = srv.ListenAndServe()
	if !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
	if err := <-shutdownErr; err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
	logger.Info("server stopped gracefully")
}
