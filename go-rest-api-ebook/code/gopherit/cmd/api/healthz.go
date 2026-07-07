package main

import "net/http"

// healthzHandler answers "is the API alive?" — handy for load balancers,
// uptime monitors, and your own sanity while developing.
func (app *application) healthzHandler(w http.ResponseWriter, r *http.Request) {
	app.writeJSON(w, http.StatusOK, envelope{
		"status":      "available",
		"environment": app.config.env,
		"version":     version,
	})
}
