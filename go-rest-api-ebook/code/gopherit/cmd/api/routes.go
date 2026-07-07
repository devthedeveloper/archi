package main

import "net/http"

// routes builds the router. Go 1.22+ ServeMux understands
// "METHOD /path/{param}" patterns, so we need zero external packages.
func (app *application) routes() http.Handler {
	mux := http.NewServeMux()

	// Health
	mux.HandleFunc("GET /api/v1/healthz", app.healthzHandler)

	// Users & auth
	mux.HandleFunc("POST /api/v1/users", app.registerUserHandler)
	mux.HandleFunc("POST /api/v1/tokens", app.loginHandler)

	// Subreddits
	mux.HandleFunc("GET /api/v1/subreddits", app.listSubredditsHandler)
	mux.HandleFunc("POST /api/v1/subreddits", app.requireAuth(app.createSubredditHandler))
	mux.HandleFunc("GET /api/v1/subreddits/{name}", app.getSubredditHandler)
	mux.HandleFunc("GET /api/v1/subreddits/{name}/posts", app.listSubredditPostsHandler)

	// Posts
	mux.HandleFunc("GET /api/v1/posts", app.listPostsHandler)
	mux.HandleFunc("POST /api/v1/posts", app.requireAuth(app.createPostHandler))
	mux.HandleFunc("GET /api/v1/posts/{id}", app.getPostHandler)

	// Comments & votes
	mux.HandleFunc("POST /api/v1/posts/{id}/comments", app.requireAuth(app.createCommentHandler))
	mux.HandleFunc("POST /api/v1/posts/{id}/vote", app.requireAuth(app.votePostHandler))
	mux.HandleFunc("POST /api/v1/comments/{id}/vote", app.requireAuth(app.voteCommentHandler))

	// The middleware onion, outermost first: recover from panics, log
	// every request, add CORS headers, then identify the caller.
	return app.recoverPanic(app.logRequests(app.enableCORS(app.authenticate(mux))))
}
