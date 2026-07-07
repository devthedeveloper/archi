package main

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"gopherit/internal/store"
)

// Subreddit names look like reddit's: lowercase letters, digits and
// underscores, 3–21 characters.
var subredditNameRX = regexp.MustCompile(`^[a-z0-9_]{3,21}$`)

// createSubredditHandler — POST /api/v1/subreddits  (auth required)
// Body: {"name": "golang", "title": "The Go...", "description": "..."}
func (app *application) createSubredditHandler(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name        string `json:"name"`
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	if err := app.readJSON(w, r, &input); err != nil {
		app.badRequest(w, err)
		return
	}

	input.Name = strings.ToLower(strings.TrimSpace(input.Name))
	input.Title = strings.TrimSpace(input.Title)

	p := make(problems)
	p.check(subredditNameRX.MatchString(input.Name), "name",
		"must be 3-21 characters of lowercase letters, digits or underscores")
	p.check(input.Title != "", "title", "must be provided")
	p.check(len(input.Title) <= 100, "title", "must be at most 100 characters")
	p.check(len(input.Description) <= 500, "description", "must be at most 500 characters")
	if len(p) > 0 {
		app.failedValidation(w, p)
		return
	}

	user := app.currentUser(r)
	subreddit, err := app.store.CreateSubreddit(input.Name, input.Title, input.Description, user.ID)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrDuplicate):
			app.conflict(w, "a subreddit with this name already exists")
		default:
			app.serverError(w, r, err)
		}
		return
	}

	w.Header().Set("Location", "/api/v1/subreddits/"+subreddit.Name)
	app.writeJSON(w, http.StatusCreated, envelope{"subreddit": subreddit})
}

// getSubredditHandler — GET /api/v1/subreddits/{name}
func (app *application) getSubredditHandler(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	subreddit, err := app.store.SubredditByName(name)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			app.notFound(w)
		default:
			app.serverError(w, r, err)
		}
		return
	}

	app.writeJSON(w, http.StatusOK, envelope{"subreddit": subreddit})
}

// listSubredditsHandler — GET /api/v1/subreddits
func (app *application) listSubredditsHandler(w http.ResponseWriter, r *http.Request) {
	subreddits, err := app.store.ListSubreddits()
	if err != nil {
		app.serverError(w, r, err)
		return
	}
	app.writeJSON(w, http.StatusOK, envelope{"subreddits": subreddits})
}
