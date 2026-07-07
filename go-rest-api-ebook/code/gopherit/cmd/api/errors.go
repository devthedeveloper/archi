package main

import "net/http"

// errorResponse is the single place that shapes error JSON. Every error
// the API ever returns looks like {"error": ...}.
func (app *application) errorResponse(w http.ResponseWriter, status int, message any) {
	app.writeJSON(w, status, envelope{"error": message})
}

// serverError = 500. Log the gory details, tell the client very little.
func (app *application) serverError(w http.ResponseWriter, r *http.Request, err error) {
	app.logger.Error("server error", "method", r.Method, "url", r.URL.String(), "error", err)
	app.errorResponse(w, http.StatusInternalServerError,
		"the server encountered a problem and could not process your request")
}

// badRequest = 400: the client sent something we couldn't even parse.
func (app *application) badRequest(w http.ResponseWriter, err error) {
	app.errorResponse(w, http.StatusBadRequest, err.Error())
}

// notFound = 404.
func (app *application) notFound(w http.ResponseWriter) {
	app.errorResponse(w, http.StatusNotFound, "the requested resource could not be found")
}

// failedValidation = 422: parseable, but the values were wrong.
func (app *application) failedValidation(w http.ResponseWriter, p problems) {
	app.errorResponse(w, http.StatusUnprocessableEntity, p)
}

// conflict = 409: e.g. username already taken.
func (app *application) conflict(w http.ResponseWriter, message string) {
	app.errorResponse(w, http.StatusConflict, message)
}

// invalidCredentials = 401 for a wrong email/password or a bad token.
func (app *application) invalidCredentials(w http.ResponseWriter) {
	app.errorResponse(w, http.StatusUnauthorized, "invalid credentials")
}

// authRequired = 401 for "you need to log in first".
func (app *application) authRequired(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	app.errorResponse(w, http.StatusUnauthorized, "you must be logged in to do this")
}
