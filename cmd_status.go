package main

import (
	"flag"
	"fmt"
	"os"
	"time"
)

// statusReport is the data behind `archi status`, assembled once so the CLI
// (stderr chrome) and the MCP server (Markdown) can render it independently.
type statusReport struct {
	Cfg    *Config
	Drift  fingerprintDiff
	Fresh  bool          // checkFreshness ok (a fingerprint existed to compare)
	Age    time.Duration // cacheAge
	Stats  []langStat
	Recent []designRecord
}

// statusCore loads the cached config and derives freshness, language, and
// design-history stats for root. It never calls failf.
func statusCore(root string) (*statusReport, error) {
	cfg, err := loadConfig(root)
	if err != nil {
		return nil, err
	}
	rep := &statusReport{Cfg: cfg}
	rep.Drift, rep.Fresh = checkFreshness(root)
	rep.Age = cacheAge(root)
	rep.Stats = make([]langStat, 0, len(cfg.Languages))
	for name, tok := range cfg.Languages {
		rep.Stats = append(rep.Stats, langStat{name: name, tokens: tok})
	}
	sortLangStats(rep.Stats)
	rep.Recent = listDesigns(root, 5)
	return rep, nil
}

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
	rep, err := statusCore(root)
	if err != nil {
		failf("%v", err)
	}
	cfg := rep.Cfg

	banner()
	line := func(k, v string) { fmt.Fprintf(os.Stderr, "  %-14s %s\n", dim(k), v) }
	line("provider", cfg.Provider+"/"+cfg.Model)
	line("initialized", cfg.InitializedAt)
	line("files", humanCount(cfg.FileCount)+dim("  ·  ~"+humanCount(cfg.ApproxTokens)+" tokens"))
	if rep.Fresh {
		v := green("fresh")
		if !rep.Drift.empty() {
			v = yellow(rep.Drift.summary()) + dim("  ·  run ") + cyan("archi init -refresh")
		}
		if rep.Age > 0 {
			v += dim("  ·  cached " + humanAge(rep.Age) + " ago")
		}
		line("freshness", v)
	}
	if n, last := memoryStats(root); n > 0 {
		line("memory", humanCount(n)+" decisions"+dim("  ·  last "+last))
	}
	os.Stderr.WriteString("\n")

	// Live language bars from the cached totals.
	total := 0
	for _, s := range rep.Stats {
		total += s.tokens
	}
	langBars(rep.Stats, total, false)

	if len(rep.Recent) > 0 {
		os.Stderr.WriteString("\n")
		info(bold("Recent designs:"))
		for _, r := range rep.Recent {
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
