package store

import (
	"database/sql"
	"errors"
)

// Vote records userID's vote on a post or comment and returns the target's
// new score. value is +1 (upvote), -1 (downvote) or 0 (remove my vote).
// Voting again simply replaces the earlier vote — that is the UPSERT.
//
// Everything runs inside one transaction so the votes table and the
// cached score column can never drift apart.
func (s *Store) Vote(userID int64, targetType string, targetID int64, value int) (int64, error) {
	table := "posts"
	if targetType == "comment" {
		table = "comments"
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // no-op after a successful Commit

	// 1. Does the thing being voted on actually exist?
	var one int
	err = tx.QueryRow(`SELECT 1 FROM `+table+` WHERE id = ?`, targetID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}

	// 2. Record (or remove) the vote.
	if value == 0 {
		_, err = tx.Exec(`
            DELETE FROM votes
            WHERE user_id = ? AND target_type = ? AND target_id = ?`,
			userID, targetType, targetID)
	} else {
		_, err = tx.Exec(`
            INSERT INTO votes (user_id, target_type, target_id, value)
            VALUES (?, ?, ?, ?)
            ON CONFLICT (user_id, target_type, target_id)
            DO UPDATE SET value = excluded.value`,
			userID, targetType, targetID, value)
	}
	if err != nil {
		return 0, err
	}

	// 3. Recompute the score from the source of truth and return it.
	var score int64
	err = tx.QueryRow(`
        UPDATE `+table+` SET score = (
            SELECT COALESCE(SUM(value), 0) FROM votes
            WHERE target_type = ? AND target_id = ?
        )
        WHERE id = ?
        RETURNING score`,
		targetType, targetID, targetID,
	).Scan(&score)
	if err != nil {
		return 0, err
	}

	return score, tx.Commit()
}
