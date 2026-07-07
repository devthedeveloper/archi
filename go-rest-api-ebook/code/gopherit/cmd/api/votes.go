package main

import (
	"errors"
	"net/http"

	"gopherit/internal/store"
)

// votePostHandler — POST /api/v1/posts/{id}/vote (auth required)
func (app *application) votePostHandler(w http.ResponseWriter, r *http.Request) {
	app.voteHandler(w, r, "post")
}

// voteCommentHandler — POST /api/v1/comments/{id}/vote (auth required)
func (app *application) voteCommentHandler(w http.ResponseWriter, r *http.Request) {
	app.voteHandler(w, r, "comment")
}

// voteHandler does the shared work. Body: {"value": 1 | -1 | 0}
// (0 means "take my vote back").
func (app *application) voteHandler(w http.ResponseWriter, r *http.Request, targetType string) {
	id, err := app.readIDParam(r)
	if err != nil {
		app.notFound(w)
		return
	}

	var input struct {
		Value *int `json:"value"` // pointer so a missing key != a 0 vote
	}
	if err := app.readJSON(w, r, &input); err != nil {
		app.badRequest(w, err)
		return
	}

	p := make(problems)
	p.check(input.Value != nil, "value", "must be provided")
	if input.Value != nil {
		v := *input.Value
		p.check(v == -1 || v == 0 || v == 1, "value", "must be -1, 0 or 1")
	}
	if len(p) > 0 {
		app.failedValidation(w, p)
		return
	}

	user := app.currentUser(r)
	score, err := app.store.Vote(user.ID, targetType, id, *input.Value)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			app.notFound(w)
		default:
			app.serverError(w, r, err)
		}
		return
	}

	app.writeJSON(w, http.StatusOK, envelope{
		"score":     score,
		"your_vote": *input.Value,
	})
}
