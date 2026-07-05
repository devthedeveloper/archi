package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"strings"
)

// stringList collects a repeatable flag (e.g. -focus a -focus b).
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func cmdDesign(args []string) {
	fs := flag.NewFlagSet("design", flag.ExitOnError)
	var focus stringList
	fs.Var(&focus, "focus", "glob of files to include as extra grounding (repeatable)")
	file := fs.String("f", "", "read the request from this file")
	out := fs.String("o", "", "write the design to this file instead of stdout")
	provider := fs.String("provider", "", "override provider (default: from .archi)")
	model := fs.String("model", "", "override model (default: from .archi)")
	temp := fs.Float64("temp", 0.4, "sampling temperature")
	maxTokens := fs.Int("max-tokens", 8000, "max output tokens")
	noStream := fs.Bool("no-stream", false, "wait for the full response instead of streaming")
	fs.Usage = func() { designUsage() }
	fs.Parse(args)

	root := "."
	if !isInitialized(root) {
		failf("no .archi cache here — run %s first", cyan("archi init"))
	}
	cfg, err := loadConfig(root)
	if err != nil {
		failf("reading .archi: %v", err)
	}

	// Resolve the request text: args > -f > stdin.
	var fileData []byte
	if *file != "" {
		fileData, err = os.ReadFile(*file)
		if err != nil {
			failf("%v", err)
		}
	}
	request, err := resolveInput(fs.Args(), fileData, *file != "", os.Stdin, isTTY(os.Stdin))
	if err != nil {
		failf("%v", err)
	}

	// Provider: flags override what init stored.
	pName, pModel := cfg.Provider, cfg.Model
	if *provider != "" {
		pName, pModel = *provider, ""
	}
	if *model != "" {
		pModel = *model
	}
	prov, err := newProvider(pName, pModel, *temp, *maxTokens)
	if err != nil {
		failf("%v", err)
	}

	mapMD, err := os.ReadFile(mapPath(root))
	if err != nil {
		failf("reading map: %v (try %s)", err, cyan("archi init -force"))
	}
	profileMD, err := os.ReadFile(profilePath(root))
	if err != nil {
		failf("reading profile: %v (try %s)", err, cyan("archi init -force"))
	}

	// Optional focus files.
	var focusFiles []seedFile
	if len(focus) > 0 {
		focusFiles = collectFocus(root, focus)
	}

	user := designUserMessage(string(profileMD), string(mapMD), focusFiles, request)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Where the finished document goes.
	var target io.Writer = os.Stdout
	var outFile *os.File
	if *out != "" {
		outFile, err = os.Create(*out)
		if err != nil {
			failf("%v", err)
		}
		defer outFile.Close()
		target = outFile
	}

	// Live view only when the document is going to an interactive stdout: the
	// user watches it appear, and the stream itself is the output — no re-print.
	live := *out == "" && stdoutTTY && !*noStream

	banner()
	info("Designing with " + prov.Name() + dim("  ·  "+humanCount(cfg.FileCount)+" files known"))

	var doc string
	if live {
		info(dim("streaming — Ctrl-C to stop"))
		os.Stderr.WriteString("\n")
		doc, err = runModel(ctx, prov, architectPrompt, user, os.Stdout, nil)
		if err != nil {
			failf("%v", err)
		}
	} else {
		var sp *Spinner
		if !*noStream {
			sp = startSpinner("Designing")
		} else {
			sp = startSpinner("Designing (buffered)")
		}
		doc, err = runModel(ctx, prov, architectPrompt, user, nil, sp)
		if err != nil {
			sp.Abort()
			failf("%v", err)
		}
		sp.Stop("Design ready")
		if _, err := io.WriteString(target, doc); err != nil {
			failf("%v", err)
		}
	}

	if outFile != nil {
		os.Stderr.WriteString("\n")
		ok("Wrote " + humanCount(strings.Count(doc, "\n")+1) + " lines (~" + humanCount(estimateTokens(doc)) + " tokens) → " + *out)
	} else if live {
		os.Stderr.WriteString("\n")
		ok("Done · ~" + humanCount(estimateTokens(doc)) + " tokens")
	}
}

// resolveInput picks the request text by precedence: args, then file, then stdin.
func resolveInput(args []string, fileData []byte, hasFile bool, stdin io.Reader, stdinIsTTY bool) (string, error) {
	if j := strings.TrimSpace(strings.Join(args, " ")); j != "" {
		return j, nil
	}
	if hasFile {
		if s := strings.TrimSpace(string(fileData)); s != "" {
			return s, nil
		}
	}
	if !stdinIsTTY {
		b, _ := io.ReadAll(stdin)
		if s := strings.TrimSpace(string(b)); s != "" {
			return s, nil
		}
	}
	return "", errors.New("no request given — pass it as an argument, with -f <file>, or on stdin")
}

// collectFocus reads files matching the given globs as extra grounding.
func collectFocus(root string, globs []string) []seedFile {
	files, err := scanRepo(root)
	if err != nil {
		return nil
	}
	fg := parseGitignore(strings.NewReader(strings.Join(globs, "\n")))
	var out []seedFile
	used := 0
	for _, f := range files {
		if !fg.Match(f.rel, false) {
			continue
		}
		data, okRead := readTextFile(root, f.rel, 256*1024)
		if !okRead {
			continue
		}
		t := estimateTokens(string(data))
		if used+t > 60000 {
			warn("focus budget reached — some matched files were skipped")
			break
		}
		used += t
		out = append(out, seedFile{rel: f.rel, lang: f.lang, content: string(data)})
	}
	if len(out) > 0 {
		info("Focused on " + humanCount(len(out)) + " files (~" + humanCount(used) + " tokens)")
	}
	return out
}
