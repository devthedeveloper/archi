package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// lastDocPath is where the last-doc stamp lives.
func lastDocPath(root string) string { return filepath.Join(archiDir(root), "last-doc") }

// lastDocStamp reads the last-doc stamp, "" when absent.
func lastDocStamp(root string) string {
	data, err := os.ReadFile(lastDocPath(root))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writeDocStamp records the doc run time; advisory.
func writeDocStamp(root string, t time.Time) error {
	return os.WriteFile(lastDocPath(root), []byte(t.Format(stampLayout)+"\n"), 0o644)
}

// designDocsSince returns design docs newer than since (built first, then by
// stamp), as seedFiles named by their history filename.
func designDocsSince(root, since string) []seedFile {
	recs := listDesigns(root, 100)
	var filtered []designRecord
	for _, r := range recs {
		if since == "" || r.stamp > since {
			filtered = append(filtered, r)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].built != filtered[j].built {
			return filtered[i].built
		}
		return filtered[i].stamp < filtered[j].stamp
	})
	var out []seedFile
	for _, r := range filtered {
		name := r.stamp + "-" + r.slug + ".md"
		if _, body, err := readDesignDoc(filepath.Join(designsDir(root), name)); err == nil {
			out = append(out, seedFile{rel: name, content: body})
		}
	}
	return out
}

// readDocsDir reads docs/*.md files (≤256 KB) as seedFiles.
func readDocsDir(root string) []seedFile {
	entries, err := os.ReadDir(filepath.Join(root, "docs"))
	if err != nil {
		return nil
	}
	var out []seedFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		rel := "docs/" + e.Name()
		if data, ok := readTextFile(root, rel, 256*1024); ok {
			out = append(out, seedFile{rel: rel, content: string(data)})
		}
	}
	return out
}

func cmdDoc(args []string) {
	fs := flag.NewFlagSet("doc", flag.ExitOnError)
	yes := fs.Bool("yes", false, "auto-approve writing docs")
	provider := fs.String("provider", "", "override provider")
	model := fs.String("model", "", "override model")
	temp := fs.Float64("temp", 0.2, "sampling temperature")
	maxTokens := fs.Int("max-tokens", 16000, "max output tokens")
	fs.Usage = docUsage
	fs.Parse(args)

	root := "."
	if !isInitialized(root) {
		failf("no .archi cache here — run %s first", cyan("archi init"))
	}
	since := lastDocStamp(root)
	designs := designDocsSince(root, since)
	if len(designs) == 0 {
		ok("Docs are up to date — no designs since last run")
		return
	}

	pName, pModel := providerResolve(root, *provider, *model)
	mk := func(mt int) Provider { p, _ := newProvider(pName, pModel, *temp, mt); return p }

	profile, _ := os.ReadFile(profilePath(root))
	readme, _ := os.ReadFile(filepath.Join(root, "README.md"))
	docs := readDocsDir(root)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	orch := newOrchestrator(root)
	banner()
	when := "the beginning"
	if since != "" {
		when = since
	}
	info(fmt.Sprintf("%d designs since last doc run", len(designs)) + dim("  ·  since "+when))

	sp := startSpinner("Updating docs")
	agent := Agent{Spec: AgentSpec{Role: RoleDocWriter, System: docWriterPrompt, MaxTokens: *maxTokens, Temp: *temp}, P: mk(*maxTokens)}
	in := TaskInput{Request: "update docs", Extra: map[string]string{
		"user_message": docUserMessage(string(profile), string(readme), docs, designs)}}
	res := orch.runStream(ctx, agent, in, nil, sp)
	if res.Err != nil {
		sp.Abort()
		failf("%v", res.Err)
	}
	sp.Stop("Docs drafted")

	applied, _, err := applyChanges(root, parseFileBlocks(res.Output), *yes)
	if err != nil {
		failf("%v", err)
	}
	if applied {
		if err := writeDocStamp(root, time.Now()); err != nil {
			warn("could not update last-doc stamp: " + err.Error())
		} else {
			ok("docs marked current")
		}
	}
}

func docUsage() {
	fmt.Fprint(os.Stderr, `usage: archi doc [flags]

  Propose README/docs updates from design history since the last doc run.

  -yes          auto-approve writing docs
  -provider string / -model string   override the backend
`)
}
