package store

import (
	"database/sql"
	"errors"
)

// Subreddit is a community, like r/golang.
type Subreddit struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	CreatorID   int64  `json:"creator_id"`
	CreatedAt   string `json:"created_at"`
}

// CreateSubreddit inserts a community. Returns ErrDuplicate when the name
// is already taken.
func (s *Store) CreateSubreddit(name, title, description string, creatorID int64) (*Subreddit, error) {
	var sr Subreddit
	err := s.db.QueryRow(`
        INSERT INTO subreddits (name, title, description, creator_id)
        VALUES (?, ?, ?, ?)
        RETURNING id, name, title, description, creator_id, created_at`,
		name, title, description, creatorID,
	).Scan(&sr.ID, &sr.Name, &sr.Title, &sr.Description, &sr.CreatorID, &sr.CreatedAt)

	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	return &sr, nil
}

// SubredditByName fetches one community. Returns ErrNotFound if missing.
func (s *Store) SubredditByName(name string) (*Subreddit, error) {
	var sr Subreddit
	err := s.db.QueryRow(`
        SELECT id, name, title, description, creator_id, created_at
        FROM subreddits
        WHERE name = ?`,
		name,
	).Scan(&sr.ID, &sr.Name, &sr.Title, &sr.Description, &sr.CreatorID, &sr.CreatedAt)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sr, nil
}

// ListSubreddits returns every community, newest first.
func (s *Store) ListSubreddits() ([]Subreddit, error) {
	rows, err := s.db.Query(`
        SELECT id, name, title, description, creator_id, created_at
        FROM subreddits
        ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	subreddits := []Subreddit{}
	for rows.Next() {
		var sr Subreddit
		err := rows.Scan(&sr.ID, &sr.Name, &sr.Title, &sr.Description, &sr.CreatorID, &sr.CreatedAt)
		if err != nil {
			return nil, err
		}
		subreddits = append(subreddits, sr)
	}
	return subreddits, rows.Err()
}
