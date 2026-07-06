package main

import (
	"flag"
	"fmt"
	"os"
)

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, "usage: archi status") }
	fs.Parse(args)

	root := "."
	if !isInitialized(root) {
		banner()
		warn("Not initialized here.")
		fmt.Fprintln(os.Stderr, "  "+dim("Run ")+cyan("archi init")+dim(" to get started."))
		return
	}
	cfg, err := loadConfig(root)
	if err != nil {
		failf("%v", err)
	}

	banner()
	line := func(k, v string) { fmt.Fprintf(os.Stderr, "  %-14s %s\n", dim(k), v) }
	line("provider", cfg.Provider+"/"+cfg.Model)
	line("initialized", cfg.InitializedAt)
	line("files", humanCount(cfg.FileCount)+dim("  ·  ~"+humanCount(cfg.ApproxTokens)+" tokens"))
	if d, fresh := checkFreshness(root); fresh {
		v := green("fresh")
		if !d.empty() {
			v = yellow(d.summary()) + dim("  ·  run ") + cyan("archi init -refresh")
		}
		if age := cacheAge(root); age > 0 {
			v += dim("  ·  cached " + humanAge(age) + " ago")
		}
		line("freshness", v)
	}
	if n, last := memoryStats(root); n > 0 {
		line("memory", humanCount(n)+" decisions"+dim("  ·  last "+last))
	}
	os.Stderr.WriteString("\n")

	// Live language bars from the cached totals.
	stats := make([]langStat, 0, len(cfg.Languages))
	total := 0
	for name, tok := range cfg.Languages {
		stats = append(stats, langStat{name: name, tokens: tok})
		total += tok
	}
	sortLangStats(stats)
	langBars(stats, total, false)

	if recent := listDesigns(root, 5); len(recent) > 0 {
		os.Stderr.WriteString("\n")
		info(bold("Recent designs:"))
		for _, r := range recent {
			state := dim("draft")
			if r.built {
				state = green("built")
			}
			fmt.Fprintf(os.Stderr, "    %s  %-40s %s\n", dim(r.stamp), r.slug, state)
		}
	}

	if backups := listBackupRuns(backupsRoot(root)); len(backups) > 0 {
		os.Stderr.WriteString("\n")
		line("backups", backups[0].stamp+dim(fmt.Sprintf("  ·  %d runs  ·  archi rollback to restore", len(backups))))
	}

	os.Stderr.WriteString("\n")
	info("Design something:  " + cyan(`archi design "add …"`))
}
