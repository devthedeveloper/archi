package store

import (
	"database/sql"
	"errors"
)

// User is an account on gopherit. PasswordHash is tagged json:"-" so it
// can NEVER leak into an API response, even by accident.
type User struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	Email        string `json:"email"`
	CreatedAt    string `json:"created_at"`
	PasswordHash []byte `json:"-"`
}

// CreateUser inserts a new user and returns the stored row.
// Returns ErrDuplicate when the username or email is already taken.
func (s *Store) CreateUser(username, email string, passwordHash []byte) (*User, error) {
	var u User
	err := s.db.QueryRow(`
        INSERT INTO users (username, email, password_hash)
        VALUES (?, ?, ?)
        RETURNING id, username, email, created_at`,
		username, email, passwordHash,
	).Scan(&u.ID, &u.Username, &u.Email, &u.CreatedAt)

	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	return &u, nil
}

// UserByEmail fetches a user (including the password hash, for login).
// Returns ErrNotFound when no such account exists.
func (s *Store) UserByEmail(email string) (*User, error) {
	var u User
	err := s.db.QueryRow(`
        SELECT id, username, email, created_at, password_hash
        FROM users
        WHERE email = ?`,
		email,
	).Scan(&u.ID, &u.Username, &u.Email, &u.CreatedAt, &u.PasswordHash)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}
