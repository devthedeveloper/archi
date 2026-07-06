package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Board is the multi-agent progress UI: one stderr line per registered agent,
// redrawn in place, extending the Spinner's \r + \x1b[K mechanics to N lines
// via cursor-up. Nil-safe: all methods no-op on a nil receiver.
type Board struct {
	mu    sync.Mutex
	rows  []boardRow // fixed order = registration order
	index map[AgentRole]int
	drawn bool // whether the block has been printed once
	frame int  // spinner animation frame
	stop  chan struct{}
	done  chan struct{}
}

// boardRow is one agent's live state.
type boardRow struct {
	role   AgentRole
	state  string // waiting | running | done | failed
	suffix string
}

// newBoard registers roles (initial state "waiting") and starts the redraw
// ticker. Returns nil when stderr is not a TTY — callers fall back to plain
// step lines. The board owns stderr while active: callers must not print
// through info/warn/ok until Stop.
func newBoard(roles []AgentRole) *Board {
	if !stderrTTY {
		return nil
	}
	b := &Board{index: make(map[AgentRole]int, len(roles)), stop: make(chan struct{}), done: make(chan struct{})}
	for _, r := range roles {
		b.index[r] = len(b.rows)
		b.rows = append(b.rows, boardRow{role: r, state: "waiting"})
	}
	go b.run()
	return b
}

func (b *Board) run() {
	defer close(b.done)
	t := time.NewTicker(90 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-t.C:
			b.mu.Lock()
			running := false
			for _, r := range b.rows {
				if r.state == "running" {
					running = true
					break
				}
			}
			if running {
				b.frame++
				b.redraw()
			}
			b.mu.Unlock()
		}
	}
}

// redraw repaints the whole block. Caller must hold b.mu.
func (b *Board) redraw() {
	if b.drawn {
		fmt.Fprintf(os.Stderr, "\x1b[%dA", len(b.rows))
	}
	for _, r := range b.rows {
		fmt.Fprint(os.Stderr, "\r"+boardLine(r, b.frame)+"\x1b[K\n")
	}
	b.drawn = true
}

// Set updates one row's state and suffix and redraws. Unknown roles are
// ignored. Valid states: waiting, running, done, failed.
func (b *Board) Set(role AgentRole, state string, suffix string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	i, ok := b.index[role]
	if !ok {
		return
	}
	b.rows[i].state = state
	b.rows[i].suffix = suffix
	b.redraw()
}

// Stop halts the animation and leaves a final static render in place (the board
// persists in scrollback; it is not cleared like a Spinner).
func (b *Board) Stop() {
	if b == nil {
		return
	}
	close(b.stop)
	<-b.done
	b.mu.Lock()
	b.redraw()
	b.mu.Unlock()
}

// boardLine renders one row (pure, testable): marker + padded role + state +
// dim suffix. frame indexes spinnerFrames for the running state.
func boardLine(row boardRow, frame int) string {
	var marker string
	switch row.state {
	case "running":
		marker = cyan(spinnerFrames[frame%len(spinnerFrames)])
	case "done":
		marker = green("◆")
	case "failed":
		marker = red("✗")
	default:
		marker = dim("◇")
	}
	line := "  " + marker + " " + fmt.Sprintf("%-18s", string(row.role)) + " " + fmt.Sprintf("%-8s", row.state) + " " + dim(row.suffix)
	return strings.TrimRight(line, " ")
}
