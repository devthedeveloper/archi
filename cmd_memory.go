package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
)

func cmdMemory(args []string) {
	root := "."
	if len(args) == 0 {
		memoryUsage()
		return
	}
	switch args[0] {
	case "show":
		memoryShow(root)
	case "edit":
		memoryEdit(root)
	case "compact":
		memoryCompact(root, args[1:])
	default:
		memoryUsage()
	}
}

// memoryShow prints the decision memory to stdout.
func memoryShow(root string) {
	data, err := os.ReadFile(memoryPath(root))
	if err != nil {
		warn("no memory yet — it appears after your first built design")
		return
	}
	os.Stdout.Write(data)
}

// memoryEdit prints the memory file path (scriptable; no editor launch).
func memoryEdit(root string) { fmt.Println(memoryPath(root)) }

// memoryCompact rewrites the memory smaller, backing up first and refusing an
// implausible result.
func memoryCompact(root string, args []string) {
	fs := flag.NewFlagSet("memory compact", flag.ExitOnError)
	provider := fs.String("provider", "", "override provider")
	model := fs.String("model", "", "override model")
	temp := fs.Float64("temp", 0, "sampling temperature")
	fs.Parse(args)

	original := loadMemory(root)
	if strings.TrimSpace(original) == "" {
		warn("no memory to compact yet")
		return
	}
	origN := len(splitMemoryEntries(original))
	pName, pModel := providerResolve(root, *provider, *model)
	mk := func(mt int) Provider { p, _ := newProvider(pName, pModel, *temp, mt); return p }

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	banner()
	info(fmt.Sprintf("Compacting %d entries", origN) + dim("  ·  ~"+humanCount(estimateTokens(original))+" tokens"))

	sp := startSpinner("Compacting memory")
	out, err := runModel(ctx, mk(8000), memoryCompactorPrompt, "=== MEMORY ===\n\n"+original+"\n\nOutput the compacted file now.", nil, sp)
	if err != nil {
		sp.Abort()
		failf("%v", err)
	}
	sp.Stop("Compacted")

	if !validCompactedMemory(original, out) {
		failf("compaction produced an implausible file — original left untouched")
	}
	// Back up the original via the standard backup machinery before overwriting.
	run := newBackupRun(root, time.Now())
	run.save(filepath.ToSlash(filepath.Join(archiDirName, "memory", "decisions.md")))
	run.finish()
	if err := os.WriteFile(memoryPath(root), []byte(strings.TrimSpace(out)+"\n"), 0o644); err != nil {
		failf("%v", err)
	}
	newN := len(splitMemoryEntries(out))
	ok(fmt.Sprintf("Compacted %d → %d entries", origN, newN) + dim("  · original backed up to "+run.dir))
}

func memoryUsage() {
	fmt.Fprint(os.Stderr, `usage: archi memory <show|edit|compact>

  show      print the decision memory
  edit      print the memory file path (open it in your editor)
  compact   merge and shrink the memory (backs up the original first)
`)
}
