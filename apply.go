package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxBackupRuns is how many timestamped backup run directories are retained
// under .archi/backups/; older ones are pruned best-effort.
const maxBackupRuns = 10

// maxDiffDisplayLines caps how much of each file's diff the preview prints.
const maxDiffDisplayLines = 200

// safeRel validates that a model-supplied path stays inside the repo root.
// It returns the cleaned slash path, or ("", false) if it escapes.
func safeRel(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" || filepath.IsAbs(p) {
		return "", false
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}

// changePreview is one proposed change, render-ready.
type changePreview struct {
	Path  string
	Kind  string // "create" | "overwrite" | "delete"
	Lines int
	Diff  string // unifiedDiff output; "" for create/delete
}

// previewChanges validates paths via safeRel (unsafe ones dropped, reported
// in skipped) and computes diffs against on-disk content. Read-only: it
// writes nothing and prints nothing. previews aligns with valid.
func previewChanges(root string, changes []fileChange) (valid []fileChange, previews []changePreview, skipped []string) {
	for _, c := range changes {
		rel, okPath := safeRel(c.path)
		if !okPath {
			skipped = append(skipped, c.path)
			continue
		}
		c.path = rel
		valid = append(valid, c)
		p := changePreview{Path: rel}
		full := filepath.Join(root, filepath.FromSlash(rel))
		old, readErr := os.ReadFile(full)
		exists := readErr == nil
		switch {
		case c.del:
			p.Kind = "delete"
		case exists:
			p.Kind = "overwrite"
			p.Lines = strings.Count(c.content, "\n") + 1
			p.Diff = unifiedDiff(string(old), ensureTrailingNewline(c.content), rel)
		default:
			p.Kind = "create"
			p.Lines = strings.Count(c.content, "\n") + 1
		}
		previews = append(previews, p)
	}
	return valid, previews, skipped
}

// writeChanges backs up and writes valid changes with no prompting. backupDir
// is "" when no originals were backed up (a creates-only run still records a
// manifest for rollback, but has nothing to back up).
func writeChanges(root string, valid []fileChange, now time.Time) (written int, backupDir string, err error) {
	written, run, err := writeChangesRun(root, valid, now)
	if err != nil {
		return written, "", err
	}
	if len(run.files) > 0 {
		backupDir = run.dir
	}
	return written, backupDir, nil
}

// writeChangesRun is writeChanges returning the full backup run, which
// applyChanges needs for its stamp and message.
func writeChangesRun(root string, valid []fileChange, now time.Time) (int, *backupRun, error) {
	run := newBackupRun(root, now)
	written := 0
	for _, c := range valid {
		full := filepath.Join(root, filepath.FromSlash(c.path))
		if c.del {
			run.save(c.path)
			if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
				return written, run, err
			}
			written++
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return written, run, err
		}
		if _, statErr := os.Stat(full); statErr == nil {
			run.save(c.path)
		} else {
			run.markCreated(c.path)
		}
		if err := os.WriteFile(full, []byte(ensureTrailingNewline(c.content)), 0o644); err != nil {
			return written, run, err
		}
		written++
	}
	run.finish()
	return written, run, nil
}

// applyChanges previews the generated file changes — including a unified diff
// for every overwrite — asks for confirmation (unless autoYes), copies each
// file it overwrites or deletes into a timestamped run directory under
// .archi/backups/, and writes them. Paths that escape the repo root are
// refused. It composes previewChanges and writeChangesRun with the original
// interactive chrome in between.
func applyChanges(root string, changes []fileChange, autoYes bool) (applied bool, stamp string, err error) {
	if len(changes) == 0 {
		warn("The model returned no file changes.")
		return false, "", nil
	}

	os.Stderr.WriteString("\n")
	info(bold("Proposed changes:"))
	valid, previews, _ := previewChanges(root, changes)
	vi := 0
	for _, c := range changes {
		if _, okPath := safeRel(c.path); !okPath {
			warn("refusing unsafe path: " + c.path)
			continue
		}
		p := previews[vi]
		vi++
		switch p.Kind {
		case "delete":
			fmt.Fprintln(os.Stderr, "    "+red("delete")+"    "+p.Path)
		case "overwrite":
			fmt.Fprintln(os.Stderr, "    "+yellow("overwrite")+" "+p.Path+dim(fmt.Sprintf("  (%d lines)", p.Lines)))
			printDiff(p.Diff, maxDiffDisplayLines)
		default:
			fmt.Fprintln(os.Stderr, "    "+green("create")+"    "+p.Path+dim(fmt.Sprintf("  (%d lines)", p.Lines)))
		}
	}
	if len(valid) == 0 {
		return false, "", nil
	}

	if gitDirty(root) {
		warn("This repo has uncommitted changes — consider committing or stashing before applying.")
	}
	if !autoYes && !confirm("Write these "+humanCount(len(valid))+" changes?") {
		warn("Aborted — nothing written.")
		return false, "", nil
	}

	written, run, err := writeChangesRun(root, valid, time.Now())
	if err != nil {
		return false, "", err
	}
	if len(run.files)+len(run.created) > 0 {
		stamp = filepath.Base(run.dir)
	}
	msg := fmt.Sprintf("Wrote %d files", written)
	if len(run.files) > 0 {
		msg += dim("  · originals backed up to " + run.dir)
	}
	ok(msg)
	return true, stamp, nil
}

// printDiff writes a unified diff to stderr, indented and colored (additions
// green, removals red, everything else dim), showing at most maxShown lines
// with a trailer counting the rest. Empty diffs print nothing.
func printDiff(diff string, maxShown int) {
	if diff == "" {
		return
	}
	lines := strings.Split(strings.TrimSuffix(diff, "\n"), "\n")
	shown := lines
	if len(shown) > maxShown {
		shown = shown[:maxShown]
	}
	for _, l := range shown {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
			l = dim(l)
		case strings.HasPrefix(l, "@@"):
			l = cyan(l)
		case strings.HasPrefix(l, "+"):
			l = green(l)
		case strings.HasPrefix(l, "-"):
			l = red(l)
		default:
			l = dim(l)
		}
		fmt.Fprintln(os.Stderr, "      "+l)
	}
	if n := len(lines) - len(shown); n > 0 {
		fmt.Fprintln(os.Stderr, "      "+dim(fmt.Sprintf("… %d more lines", n)))
	}
}

// gitDirty reports whether root has uncommitted changes per
// `git status --porcelain`. Git being absent, or root not being a repository,
// reads as clean — this only feeds an advisory warning.
func gitDirty(root string) bool {
	out, err := exec.Command("git", "-C", root, "status", "--porcelain").Output()
	if err != nil {
		return false
	}
	return len(bytes.TrimSpace(out)) > 0
}

// backupRun collects the originals touched by one apply run under
// .archi/backups/<timestamp>/, preserving each file's relative path. All of
// it is best-effort: backups never block the apply itself.
type backupRun struct {
	root    string
	dir     string
	files   []string // backed-up originals (overwritten or deleted)
	created []string // files this run created (no backup; rollback deletes them)
}

// markCreated records that rel was created by this run (it did not exist before).
func (b *backupRun) markCreated(rel string) { b.created = append(b.created, rel) }

// newBackupRun names (but does not yet create) the backup directory for an
// apply run starting at t.
func newBackupRun(root string, t time.Time) *backupRun {
	return &backupRun{root: root, dir: backupRunDir(root, t)}
}

// backupRunDir builds the backup directory path for a run starting at t.
func backupRunDir(root string, t time.Time) string {
	return filepath.Join(archiDir(root), "backups", t.Format(stampLayout))
}

// save copies root/rel into the run directory, creating parents on first use.
func (b *backupRun) save(rel string) {
	data, err := os.ReadFile(filepath.Join(b.root, filepath.FromSlash(rel)))
	if err != nil {
		return
	}
	dst := filepath.Join(b.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return
	}
	b.files = append(b.files, rel)
}

// finish writes the run's v2 manifest and prunes old backup runs. It is a
// no-op when the run neither backed up nor created anything.
func (b *backupRun) finish() {
	if len(b.files)+len(b.created) == 0 {
		return
	}
	var sb strings.Builder
	sb.WriteString("#v2\n")
	for _, rel := range b.files {
		sb.WriteString("B\t" + rel + "\n")
	}
	for _, rel := range b.created {
		sb.WriteString("C\t" + rel + "\n")
	}
	if err := os.MkdirAll(b.dir, 0o755); err == nil {
		_ = os.WriteFile(filepath.Join(b.dir, "manifest.txt"), []byte(sb.String()), 0o644)
	}
	pruneBackups(filepath.Dir(b.dir), maxBackupRuns)
}

// pruneBackups removes all but the keep newest run directories under dir,
// best-effort.
func pruneBackups(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var runs []string
	for _, e := range entries {
		if e.IsDir() {
			runs = append(runs, e.Name())
		}
	}
	for _, name := range backupsToPrune(runs, keep) {
		_ = os.RemoveAll(filepath.Join(dir, name))
	}
}

// backupsToPrune picks which run names should be removed to keep only the
// keep newest. Names are timestamps, so lexical order is age order.
func backupsToPrune(names []string, keep int) []string {
	if len(names) <= keep {
		return nil
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	return sorted[:len(sorted)-keep]
}

func ensureTrailingNewline(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
