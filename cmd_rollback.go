package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// backupInfo summarizes one backup run for listing.
type backupInfo struct {
	stamp    string
	restored int
	created  int
}

// backupsRoot returns the .archi/backups directory for root.
func backupsRoot(root string) string { return filepath.Join(archiDir(root), "backups") }

// parseManifest splits a backup manifest into files to restore and files to
// delete. A "#v2" header enables the tagged format; anything else is treated as
// the v1 bare-path list (every line a restore).
func parseManifest(data []byte) (restore, created []string) {
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "#v2" {
		for _, line := range lines[1:] {
			line = strings.TrimRight(line, "\r")
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, "\t", 2)
			if len(parts) != 2 {
				continue
			}
			switch parts[0] {
			case "B":
				restore = append(restore, parts[1])
			case "C":
				created = append(created, parts[1])
			default:
				warn("rollback: unknown manifest op " + parts[0])
			}
		}
		return restore, created
	}
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			restore = append(restore, line)
		}
	}
	return restore, created
}

// listBackupRuns returns the backup runs under dir, newest first.
func listBackupRuns(dir string) []backupInfo {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	var out []backupInfo
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name, "manifest.txt"))
		if err != nil {
			continue
		}
		r, c := parseManifest(data)
		out = append(out, backupInfo{stamp: name, restored: len(r), created: len(c)})
	}
	return out
}

// restoreRun copies backed-up files back and deletes files the run created.
func restoreRun(root, runDir string, restore, created []string) error {
	for _, rel := range restore {
		if _, ok := safeRel(rel); !ok {
			warn("skipping unsafe path: " + rel)
			continue
		}
		data, err := os.ReadFile(filepath.Join(runDir, filepath.FromSlash(rel)))
		if err != nil {
			continue // backup copy missing; nothing to restore
		}
		dst := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
	}
	for _, rel := range created {
		if _, ok := safeRel(rel); !ok {
			warn("skipping unsafe path: " + rel)
			continue
		}
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			return err
		}
		os.Remove(filepath.Dir(full)) // best-effort: removes the parent only if now empty
	}
	return nil
}

func cmdRollback(args []string) {
	fs := flag.NewFlagSet("rollback", flag.ExitOnError)
	runStamp := fs.String("run", "", "restore this backup run (default: newest)")
	list := fs.Bool("list", false, "list available backup runs and exit")
	yes := fs.Bool("yes", false, "skip confirmation")
	fs.Usage = rollbackUsage
	fs.Parse(args)

	root := "."
	if !isInitialized(root) {
		failf("no .archi here — nothing to roll back")
	}
	dir := backupsRoot(root)
	runs := listBackupRuns(dir)

	if *list {
		banner()
		if len(runs) == 0 {
			warn("no backups found")
			return
		}
		for _, r := range runs {
			fmt.Fprintf(os.Stderr, "  %s  %s\n", cyan(r.stamp), dim(fmt.Sprintf("%d restored · %d created", r.restored, r.created)))
		}
		return
	}
	if len(runs) == 0 {
		warn("no backups found")
		return
	}

	stamp := *runStamp
	if stamp == "" {
		stamp = runs[0].stamp
	}
	runDir := filepath.Join(dir, stamp)
	data, err := os.ReadFile(filepath.Join(runDir, "manifest.txt"))
	if err != nil {
		failf("no such backup run: %s", stamp)
	}
	restore, created := parseManifest(data)

	banner()
	info("Rolling back " + stamp)
	for _, rel := range restore {
		if _, ok := safeRel(rel); ok {
			fmt.Fprintln(os.Stderr, "    "+yellow("restore")+" "+rel)
		}
	}
	for _, rel := range created {
		if _, ok := safeRel(rel); ok {
			fmt.Fprintln(os.Stderr, "    "+red("delete")+"  "+rel+dim("  (created by that run)"))
		}
	}
	if !*yes && !confirm("Restore this backup run?") {
		warn("Aborted — nothing changed.")
		return
	}
	if err := restoreRun(root, runDir, restore, created); err != nil {
		failf("%v", err)
	}
	ok(fmt.Sprintf("Restored %d files, deleted %d created files", len(restore), len(created)) + dim("  · from "+stamp))
}

func rollbackUsage() {
	fmt.Fprint(os.Stderr, `usage: archi rollback [flags]

  Restore the files a build run overwrote or created.

  -run string   restore this backup run under .archi/backups/<stamp> (default: newest)
  -list         list available backup runs and exit
  -yes          skip confirmation
`)
}
