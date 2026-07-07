package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"time"
)

// CreateToken mints a random bearer token for a user. The client gets the
// plaintext; we keep only a SHA-256 hash, so a stolen database does not
// hand out working logins.
func (s *Store) CreateToken(userID int64, ttl time.Duration) (plaintext, expiresAt string, err error) {
	plaintext = rand.Text() // 26 chars of crypto-random base32 (Go 1.24+)
	hash := sha256.Sum256([]byte(plaintext))
	expiresAt = time.Now().UTC().Add(ttl).Format(time.RFC3339)

	_, err = s.db.Exec(`
        INSERT INTO tokens (hash, user_id, expires_at)
        VALUES (?, ?, ?)`,
		hash[:], userID, expiresAt)
	if err != nil {
		return "", "", err
	}
	return plaintext, expiresAt, nil
}

// UserForToken exchanges a plaintext bearer token for the user it belongs
// to. Returns ErrNotFound for unknown or expired tokens.
func (s *Store) UserForToken(plaintext string) (*User, error) {
	hash := sha256.Sum256([]byte(plaintext))
	now := time.Now().UTC().Format(time.RFC3339)

	var u User
	err := s.db.QueryRow(`
        SELECT u.id, u.username, u.email, u.created_at
        FROM tokens t
        JOIN users  u ON u.id = t.user_id
        WHERE t.hash = ? AND t.expires_at > ?`,
		hash[:], now,
	).Scan(&u.ID, &u.Username, &u.Email, &u.CreatedAt)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}
