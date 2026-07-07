// Chapter 3 checkpoint: the project skeleton, before the database
// (chapter 5), JSON helpers (chapter 4) and graceful shutdown
// (chapter 12) get added.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"
)

const version = "0.1.0"

// config holds everything tweakable from the outside world.
type config struct {
	port int
	env  string
}

// application is the "toolbox" every handler can reach.
// Handlers hang off it as methods, so they can use the logger and
// config without any global variables.
type application struct {
	config config
	logger *slog.Logger
}

func main() {
	var cfg config
	flag.IntVar(&cfg.port, "port", 4000, "API server port")
	flag.StringVar(&cfg.env, "env", "development", "Environment (development|production)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	app := &application{
		config: cfg,
		logger: logger,
	}

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.port),
		Handler:      app.routes(),
		IdleTimeout:  time.Minute,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	logger.Info("starting server", "addr", srv.Addr, "env", cfg.env, "version", version)
	err := srv.ListenAndServe()
	logger.Error("server stopped", "error", err)
	os.Exit(1)
}
