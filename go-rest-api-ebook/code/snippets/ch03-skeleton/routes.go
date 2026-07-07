package main

import "net/http"

// routes builds the router. It grows a line or two every chapter.
func (app *application) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/healthz", app.healthzHandler)
	return mux
}
