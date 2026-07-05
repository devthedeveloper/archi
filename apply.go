package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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

// applyChanges previews the generated file changes, asks for confirmation
// (unless autoYes), backs up any file it overwrites to <path>.bak, and writes
// them. Paths that escape the repo root are refused.
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
		_, statErr := os.Stat(full)
		exists := statErr == nil
		switch {
		case c.del:
			fmt.Fprintln(os.Stderr, "    "+red("delete")+"    "+rel)
		case exists:
			fmt.Fprintln(os.Stderr, "    "+yellow("overwrite")+" "+rel+dim(fmt.Sprintf("  (%d lines)", strings.Count(c.content, "\n")+1)))
		default:
			fmt.Fprintln(os.Stderr, "    "+green("create")+"    "+rel+dim(fmt.Sprintf("  (%d lines)", strings.Count(c.content, "\n")+1)))
		}
	}
	if len(valid) == 0 {
		return nil
	}

	if !autoYes && !confirm("Write these "+humanCount(len(valid))+" changes?") {
		warn("Aborted — nothing written.")
		return nil
	}

	written := 0
	for _, c := range valid {
		full := filepath.Join(root, filepath.FromSlash(c.path))
		if c.del {
			backup(full)
			if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if _, err := os.Stat(full); err == nil {
			backup(full)
		}
		if err := os.WriteFile(full, []byte(ensureTrailingNewline(c.content)), 0o644); err != nil {
			return err
		}
		written++
	}
	ok(fmt.Sprintf("Wrote %d files", written) + dim("  · overwrites backed up to <file>.bak"))
	return nil
}

// backup copies an existing file to <path>.bak, best-effort.
func backup(full string) {
	data, err := os.ReadFile(full)
	if err != nil {
		return
	}
	_ = os.WriteFile(full+".bak", data, 0o644)
}

func ensureTrailingNewline(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
