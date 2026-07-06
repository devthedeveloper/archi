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

// applyChanges previews the generated file changes — including a unified diff
// for every overwrite — asks for confirmation (unless autoYes), copies each
// file it overwrites or deletes into a timestamped run directory under
// .archi/backups/, and writes them. Paths that escape the repo root are
// refused.
func applyChanges(root string, changes []fileChange, autoYes bool) error {
	if len(changes) == 0 {
		warn("The model returned no file changes.")
		return nil
	}

	os.Stderr.WriteString("\n")
	info(bold("Proposed changes:"))
	valid := changes[:0:0]
	for _, c := range changes {
		rel, okPath := safeRel(c.path)
		if !okPath {
			warn("refusing unsafe path: " + c.path)
			continue
		}
		c.path = rel
		valid = append(valid, c)
		full := filepath.Join(root, filepath.FromSlash(rel))
		old, readErr := os.ReadFile(full)
		exists := readErr == nil
		switch {
		case c.del:
			fmt.Fprintln(os.Stderr, "    "+red("delete")+"    "+rel)
		case exists:
			fmt.Fprintln(os.Stderr, "    "+yellow("overwrite")+" "+rel+dim(fmt.Sprintf("  (%d lines)", strings.Count(c.content, "\n")+1)))
			printDiff(unifiedDiff(string(old), ensureTrailingNewline(c.content), rel), maxDiffDisplayLines)
		default:
			fmt.Fprintln(os.Stderr, "    "+green("create")+"    "+rel+dim(fmt.Sprintf("  (%d lines)", strings.Count(c.content, "\n")+1)))
		}
	}
	if len(valid) == 0 {
		return nil
	}

	if gitDirty(root) {
		warn("This repo has uncommitted changes — consider committing or stashing before applying.")
	}
	if !autoYes && !confirm("Write these "+humanCount(len(valid))+" changes?") {
		warn("Aborted — nothing written.")
		return nil
	}

	run := newBackupRun(root, time.Now())
	written := 0
	for _, c := range valid {
		full := filepath.Join(root, filepath.FromSlash(c.path))
		if c.del {
			run.save(c.path)
			if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if _, err := os.Stat(full); err == nil {
			run.save(c.path)
		}
		if err := os.WriteFile(full, []byte(ensureTrailingNewline(c.content)), 0o644); err != nil {
			return err
		}
		written++
	}
	run.finish()
	msg := fmt.Sprintf("Wrote %d files", written)
	if len(run.files) > 0 {
		msg += dim("  · originals backed up to " + run.dir)
	}
	ok(msg)
	return nil
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
	root  string
	dir   string
	files []string
}

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

// finish writes the run's manifest.txt and prunes old backup runs. It is a
// no-op when nothing was backed up.
func (b *backupRun) finish() {
	if len(b.files) == 0 {
		return
	}
	_ = os.WriteFile(filepath.Join(b.dir, "manifest.txt"), []byte(strings.Join(b.files, "\n")+"\n"), 0o644)
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
