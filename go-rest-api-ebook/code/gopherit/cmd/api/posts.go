package main

import (
	"errors"
	"net/http"
	"strings"

	"gopherit/internal/store"
)

// createPostHandler — POST /api/v1/posts  (auth required)
// Body: {"subreddit": "golang", "title": "...", "body": "..."} for a text
// post, or {"subreddit": "...", "title": "...", "url": "https://..."} for
// a link post.
func (app *application) createPostHandler(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Subreddit string `json:"subreddit"`
		Title     string `json:"title"`
		Body      string `json:"body"`
		URL       string `json:"url"`
	}
	if err := app.readJSON(w, r, &input); err != nil {
		app.badRequest(w, err)
		return
	}

	input.Title = strings.TrimSpace(input.Title)
	input.URL = strings.TrimSpace(input.URL)

	p := make(problems)
	p.check(input.Subreddit != "", "subreddit", "must be provided")
	p.check(input.Title != "", "title", "must be provided")
	p.check(len(input.Title) <= 300, "title", "must be at most 300 characters")
	p.check(len(input.Body) <= 40_000, "body", "must be at most 40,000 characters")
	p.check(input.Body == "" || input.URL == "", "url",
		"a post can have a body or a url, not both")
	if input.URL != "" {
		p.check(strings.HasPrefix(input.URL, "http://") || strings.HasPrefix(input.URL, "https://"),
			"url", "must start with http:// or https://")
	}
	if len(p) > 0 {
		app.failedValidation(w, p)
		return
	}

	subreddit, err := app.store.SubredditByName(input.Subreddit)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			app.failedValidation(w, problems{"subreddit": "no such subreddit"})
		default:
			app.serverError(w, r, err)
		}
		return
	}

	user := app.currentUser(r)
	post, err := app.store.CreatePost(subreddit.ID, user.ID, input.Title, input.Body, input.URL)
	if err != nil {
		app.serverError(w, r, err)
		return
	}

	app.writeJSON(w, http.StatusCreated, envelope{"post": post})
}

// getPostHandler — GET /api/v1/posts/{id}
// Returns the post AND its full comment tree in one response.
func (app *application) getPostHandler(w http.ResponseWriter, r *http.Request) {
	id, err := app.readIDParam(r)
	if err != nil {
		app.notFound(w)
		return
	}

	post, err := app.store.PostByID(id)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			app.notFound(w)
		default:
			app.serverError(w, r, err)
		}
		return
	}

	comments, err := app.store.CommentsForPost(id)
	if err != nil {
		app.serverError(w, r, err)
		return
	}

	app.writeJSON(w, http.StatusOK, envelope{"post": post, "comments": comments})
}

// listPostsHandler — GET /api/v1/posts?sort=hot&page=1&page_size=20
// The site-wide front page.
func (app *application) listPostsHandler(w http.ResponseWriter, r *http.Request) {
	filters, err := app.readPostFilters(r)
	if err != nil {
		app.badRequest(w, err)
		return
	}

	posts, meta, err := app.store.ListPosts(filters)
	if err != nil {
		app.serverError(w, r, err)
		return
	}

	app.writeJSON(w, http.StatusOK, envelope{"posts": posts, "meta": meta})
}

// listSubredditPostsHandler — GET /api/v1/subreddits/{name}/posts
// Same as the front page, but scoped to one community.
func (app *application) listSubredditPostsHandler(w http.ResponseWriter, r *http.Request) {
	subreddit, err := app.store.SubredditByName(r.PathValue("name"))
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			app.notFound(w)
		default:
			app.serverError(w, r, err)
		}
		return
	}

	filters, err := app.readPostFilters(r)
	if err != nil {
		app.badRequest(w, err)
		return
	}
	filters.SubredditID = subreddit.ID

	posts, meta, err := app.store.ListPosts(filters)
	if err != nil {
		app.serverError(w, r, err)
		return
	}

	app.writeJSON(w, http.StatusOK, envelope{"posts": posts, "meta": meta})
}
