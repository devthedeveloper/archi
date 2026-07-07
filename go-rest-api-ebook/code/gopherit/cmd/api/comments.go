package main

import (
	"errors"
	"net/http"
	"strings"

	"gopherit/internal/store"
)

// createCommentHandler — POST /api/v1/posts/{id}/comments (auth required)
// Body: {"body": "..."} for a top-level comment, or
// {"body": "...", "parent_id": 42} to reply to comment 42.
func (app *application) createCommentHandler(w http.ResponseWriter, r *http.Request) {
	postID, err := app.readIDParam(r)
	if err != nil {
		app.notFound(w)
		return
	}

	var input struct {
		Body     string `json:"body"`
		ParentID *int64 `json:"parent_id"` // pointer: absent JSON key -> nil
	}
	if err := app.readJSON(w, r, &input); err != nil {
		app.badRequest(w, err)
		return
	}

	p := make(problems)
	p.check(strings.TrimSpace(input.Body) != "", "body", "must be provided")
	p.check(len(input.Body) <= 10_000, "body", "must be at most 10,000 characters")
	if len(p) > 0 {
		app.failedValidation(w, p)
		return
	}

	user := app.currentUser(r)
	comment, err := app.store.CreateComment(postID, user.ID, input.ParentID, input.Body)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			app.notFound(w)
		case errors.Is(err, store.ErrInvalidParent):
			app.failedValidation(w, problems{"parent_id": "must be a comment on this post"})
		default:
			app.serverError(w, r, err)
		}
		return
	}

	app.writeJSON(w, http.StatusCreated, envelope{"comment": comment})
}
