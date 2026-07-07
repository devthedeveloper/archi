package main

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"gopherit/internal/store"
)

// registerUserHandler — POST /api/v1/users
// Body: {"username": "...", "email": "...", "password": "..."}
func (app *application) registerUserHandler(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := app.readJSON(w, r, &input); err != nil {
		app.badRequest(w, err)
		return
	}

	input.Username = strings.TrimSpace(input.Username)
	input.Email = strings.TrimSpace(strings.ToLower(input.Email))

	p := make(problems)
	p.check(len(input.Username) >= 3, "username", "must be at least 3 characters")
	p.check(len(input.Username) <= 30, "username", "must be at most 30 characters")
	p.check(!strings.ContainsAny(input.Username, " \t\n"), "username", "must not contain spaces")
	p.check(strings.Count(input.Email, "@") == 1, "email", "must be a valid email address")
	p.check(len(input.Password) >= 8, "password", "must be at least 8 characters")
	p.check(len(input.Password) <= 72, "password", "must be at most 72 characters")
	if len(p) > 0 {
		app.failedValidation(w, p)
		return
	}

	// bcrypt: slow on purpose, salted automatically. Cost 12 ≈ 250ms.
	hash, err := bcrypt.GenerateFromPassword([]byte(input.Password), 12)
	if err != nil {
		app.serverError(w, r, err)
		return
	}

	user, err := app.store.CreateUser(input.Username, input.Email, hash)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrDuplicate):
			app.conflict(w, "a user with this username or email already exists")
		default:
			app.serverError(w, r, err)
		}
		return
	}

	app.writeJSON(w, http.StatusCreated, envelope{"user": user})
}

// loginHandler — POST /api/v1/tokens
// Body: {"email": "...", "password": "..."}
// Trades valid credentials for a bearer token that lasts 7 days.
func (app *application) loginHandler(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := app.readJSON(w, r, &input); err != nil {
		app.badRequest(w, err)
		return
	}

	user, err := app.store.UserByEmail(strings.TrimSpace(strings.ToLower(input.Email)))
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			// Same reply as a wrong password — don't reveal which
			// emails have accounts.
			app.invalidCredentials(w)
		default:
			app.serverError(w, r, err)
		}
		return
	}

	err = bcrypt.CompareHashAndPassword(user.PasswordHash, []byte(input.Password))
	if err != nil {
		app.invalidCredentials(w)
		return
	}

	token, expiresAt, err := app.store.CreateToken(user.ID, 7*24*time.Hour)
	if err != nil {
		app.serverError(w, r, err)
		return
	}

	app.writeJSON(w, http.StatusCreated, envelope{
		"token":      token,
		"expires_at": expiresAt,
		"user":       user,
	})
}
