package store

import (
	"database/sql"
	"errors"
)

// Comment is one comment. ParentID is a *pointer* so a top-level comment
// serialises as null (or disappears, thanks to omitempty) instead of 0.
// Replies makes the type recursive — a comment holds its own children.
type Comment struct {
	ID        int64      `json:"id"`
	PostID    int64      `json:"post_id"`
	ParentID  *int64     `json:"parent_id,omitempty"`
	Author    string     `json:"author"`
	Body      string     `json:"body"`
	Score     int64      `json:"score"`
	CreatedAt string     `json:"created_at"`
	Replies   []*Comment `json:"replies,omitempty"`
}

// CreateComment adds a comment to a post. A nil parentID means a
// top-level comment; otherwise the parent must be a comment on the SAME
// post (ErrInvalidParent if it isn't).
func (s *Store) CreateComment(postID, authorID int64, parentID *int64, body string) (*Comment, error) {
	// Make sure the post exists.
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM posts WHERE id = ?`, postID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	// Make sure the parent comment (if any) belongs to this post.
	if parentID != nil {
		var parentPostID int64
		err := s.db.QueryRow(`SELECT post_id FROM comments WHERE id = ?`, *parentID).Scan(&parentPostID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && parentPostID != postID) {
			return nil, ErrInvalidParent
		}
		if err != nil {
			return nil, err
		}
	}

	var c Comment
	err = s.db.QueryRow(`
        INSERT INTO comments (post_id, author_id, parent_id, body)
        VALUES (?, ?, ?, ?)
        RETURNING id, post_id, parent_id, body, score, created_at`,
		postID, authorID, parentID, body,
	).Scan(&c.ID, &c.PostID, &c.ParentID, &c.Body, &c.Score, &c.CreatedAt)
	if err != nil {
		return nil, err
	}

	err = s.db.QueryRow(`SELECT username FROM users WHERE id = ?`, authorID).Scan(&c.Author)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// CommentsForPost returns the full comment TREE for a post: top-level
// comments with their replies nested inside, oldest first.
func (s *Store) CommentsForPost(postID int64) ([]*Comment, error) {
	rows, err := s.db.Query(`
        SELECT c.id, c.post_id, c.parent_id, u.username, c.body, c.score, c.created_at
        FROM comments c
        JOIN users u ON u.id = c.author_id
        WHERE c.post_id = ?
        ORDER BY c.id ASC`,
		postID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	flat := []*Comment{}
	for rows.Next() {
		var c Comment
		err := rows.Scan(&c.ID, &c.PostID, &c.ParentID, &c.Author, &c.Body, &c.Score, &c.CreatedAt)
		if err != nil {
			return nil, err
		}
		flat = append(flat, &c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return buildTree(flat), nil
}

// buildTree turns a flat list of comments into a nested tree in O(n).
// Trick: because we ordered by id ASC, every parent appears in the map
// before any of its children shows up.
func buildTree(flat []*Comment) []*Comment {
	byID := make(map[int64]*Comment, len(flat))
	for _, c := range flat {
		byID[c.ID] = c
	}

	roots := []*Comment{}
	for _, c := range flat {
		if c.ParentID == nil {
			roots = append(roots, c)
		} else if parent, ok := byID[*c.ParentID]; ok {
			parent.Replies = append(parent.Replies, c)
		} else {
			roots = append(roots, c) // orphan (parent deleted) — show at top level
		}
	}
	return roots
}
