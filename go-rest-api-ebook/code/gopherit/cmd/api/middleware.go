package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gopherit/internal/store"
)

// contextKey is a private type so nothing outside this package can
// collide with our context keys.
type contextKey string

const userContextKey = contextKey("user")

// recoverPanic turns a panicking handler into a clean 500 response
// instead of a dropped connection.
func (app *application) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				w.Header().Set("Connection", "close")
				app.serverError(w, r, fmt.Errorf("panic: %v", err))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// logRequests writes one structured log line per request.
func (app *application) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		app.logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"took", time.Since(start).String())
	})
}

// enableCORS lets browser frontends on other origins call this API —
// that's what makes gopherit "bring your own frontend".
func (app *application) enableCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")

		// Browsers send a "preflight" OPTIONS request before the real
		// one. Answer it directly with 204 No Content.
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate checks the Authorization header. No header = anonymous
// visitor, carry on. A valid "Bearer <token>" = load the user and stash
// them in the request context. A broken/expired token = 401 right here.
func (app *application) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Authorization")

		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			next.ServeHTTP(w, r) // anonymous is fine for public routes
			return
		}

		token, ok := strings.CutPrefix(authHeader, "Bearer ")
		if !ok || token == "" {
			app.invalidCredentials(w)
			return
		}

		user, err := app.store.UserForToken(token)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrNotFound):
				app.invalidCredentials(w)
			default:
				app.serverError(w, r, err)
			}
			return
		}

		ctx := context.WithValue(r.Context(), userContextKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireAuth guards a single route: no logged-in user, no entry.
func (app *application) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if app.currentUser(r) == nil {
			app.authRequired(w)
			return
		}
		next(w, r)
	}
}

// currentUser fishes the logged-in user out of the request context.
// Returns nil for anonymous requests.
func (app *application) currentUser(r *http.Request) *store.User {
	user, _ := r.Context().Value(userContextKey).(*store.User)
	return user
}
