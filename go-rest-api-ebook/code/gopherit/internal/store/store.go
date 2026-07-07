// Package store is the data layer of gopherit. Everything that touches
// the database lives here, so the HTTP handlers never write SQL.
package store

import (
	"database/sql"
	"errors"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver with database/sql
)

// Sentinel errors. Handlers compare against these with errors.Is and
// translate them into the right HTTP status codes.
var (
	ErrNotFound      = errors.New("store: not found")
	ErrDuplicate     = errors.New("store: duplicate value")
	ErrInvalidParent = errors.New("store: parent comment does not belong to this post")
)

// Store wraps the database connection pool. Every model gets methods on
// this one struct: s.CreateUser(...), s.ListPosts(...), and so on.
type Store struct {
	db *sql.DB
}

// Open connects to the SQLite file at dsn, applies pragmas and runs the
// schema migration. Call Close when the application shuts down.
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}

	// SQLite allows only one writer at a time. Limiting the pool to a
	// single connection sidesteps "database is locked" errors entirely.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close releases the underlying connection pool.
func (s *Store) Close() error {
	return s.db.Close()
}

// isUniqueViolation reports whether err was caused by a UNIQUE constraint,
// e.g. registering a username that is already taken.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// schema is executed on every start-up. Thanks to IF NOT EXISTS it is a
// no-op when the tables are already there — a poor man's migration that
// is perfect for a learning project.
const schema = `
CREATE TABLE IF NOT EXISTS users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    email         TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    password_hash BLOB    NOT NULL,
    created_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE TABLE IF NOT EXISTS tokens (
    hash       BLOB PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS subreddits (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    title       TEXT    NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    creator_id  INTEGER NOT NULL REFERENCES users(id),
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE TABLE IF NOT EXISTS posts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    subreddit_id INTEGER NOT NULL REFERENCES subreddits(id) ON DELETE CASCADE,
    author_id    INTEGER NOT NULL REFERENCES users(id),
    title        TEXT    NOT NULL,
    body         TEXT    NOT NULL DEFAULT '',
    url          TEXT    NOT NULL DEFAULT '',
    score        INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE TABLE IF NOT EXISTS comments (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    post_id    INTEGER NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    author_id  INTEGER NOT NULL REFERENCES users(id),
    parent_id  INTEGER REFERENCES comments(id) ON DELETE CASCADE,
    body       TEXT    NOT NULL,
    score      INTEGER NOT NULL DEFAULT 0,
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE TABLE IF NOT EXISTS votes (
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    target_type TEXT    NOT NULL CHECK (target_type IN ('post','comment')),
    target_id   INTEGER NOT NULL,
    value       INTEGER NOT NULL CHECK (value IN (-1, 1)),
    PRIMARY KEY (user_id, target_type, target_id)
);

CREATE INDEX IF NOT EXISTS idx_posts_subreddit ON posts(subreddit_id);
CREATE INDEX IF NOT EXISTS idx_comments_post   ON comments(post_id);
`
