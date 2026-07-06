// Command archi is a software architect that knows your codebase. Run
// `archi init` once to learn a repository, then `archi design "<request>"` to
// get design documents — briefings, Mermaid diagrams, and plans — grounded in
// your real modules, stack, and conventions.
package main

import (
	"fmt"
	"os"
)

// version is the release version; goreleaser overrides it at build time via
// -ldflags "-X main.version=...".
var version = "1.4.0"

func main() {
	if len(os.Args) < 2 {
		printHelp()
		return
	}
	switch os.Args[1] {
	case "init":
		cmdInit(os.Args[2:])
	case "design":
		cmdDesign(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "version", "-version", "--version", "-v":
		fmt.Println("archi", version)
	case "help", "-h", "--help":
		printHelp()
	default:
		fmt.Fprintln(os.Stderr, "  "+red("✗")+" unknown command "+bold(os.Args[1]))
		printHelp()
		os.Exit(2)
	}
}

func printHelp() {
	banner()
	fmt.Fprint(os.Stderr, `
  `+bold("USAGE")+`
    archi <command> [flags]

  `+bold("COMMANDS")+`
    `+cyan("init")+`   [path]        Learn a codebase — scan it and cache an understanding
    `+cyan("design")+` "<request>"   Design a change, grounded in the learned codebase
    `+cyan("status")+`               Show what archi has learned about this repo
    `+cyan("version")+`              Print the version

  `+bold("QUICK START")+`
    `+dim("$")+` cd my-service
    `+dim("$")+` archi init
    `+dim("$")+` archi design "add webhooks with retries and signature verification"

  `+bold("EXAMPLES")+`
    archi init ./api                       `+dim("learn a subdirectory")+`
    archi design "make orders idempotent"  `+dim("design against the whole repo")+`
    archi design -focus 'internal/pay/**' "add refunds"
    archi design "add search" -o design.md `+dim("write to a file")+`
    echo "add rate limiting" | archi design

  `+bold("SETUP")+`
    Ollama Cloud (default):  export OLLAMA_API_KEY=...   `+dim("or OLLAMA_HOST for local")+`
    Anthropic:               export ANTHROPIC_API_KEY=... `+dim("(-provider anthropic)")+`
    OpenAI-compatible:       export OPENAI_API_KEY=...   `+dim("(-provider openai; OPENAI_BASE_URL for OpenRouter/Groq/local)")+`
    Timeout:                 export ARCHI_TIMEOUT=10m    `+dim("overall per-request cap (default: none)")+`
    Context budget:          export ARCHI_CONTEXT_TOKENS=48000 `+dim("grounding cap for design")+`
    Agent concurrency:       export ARCHI_AGENT_CONCURRENCY=3 `+dim("agent worker pool size")+`
    Run token budget:        export ARCHI_RUN_TOKENS=150000 `+dim("output-token budget per run")+`

  `+dim("Docs: https://github.com/devthedeveloper/archi")+`

`)
}

func initUsage() {
	fmt.Fprint(os.Stderr, `usage: archi init [path]

  Scan a repository and cache an understanding in .archi/.

  -provider string   LLM provider: ollama, anthropic, or openai (default "ollama")
  -model string      model id (default: provider default)
  -force             rebuild even if .archi already exists
  -refresh           update an existing cache incrementally from what changed
  -max-file-kb int   skip files larger than this when sampling (default 256)
`)
}

func designUsage() {
	fmt.Fprint(os.Stderr, `usage: archi design [flags] [request]

  In a terminal, archi interviews you, streams a design, then lets you approve &
  build the code, modify, or ask questions. Piped or with -no-interactive it just
  prints the design. Request may be args, -f <file>, or stdin.

  -focus glob        include matching files as extra grounding (repeatable)
  -f string          read the request from this file
  -o string          write the design to this file instead of stdout
  -html string       also render the design to a standalone HTML file (drawn diagrams)
  -no-interactive    skip the interview and review loop
  -build             after designing, generate the code (non-interactive)
  -yes               auto-approve writing generated files
  -provider string   override provider (default: from .archi)
  -model string      override model (default: from .archi)
  -temp float        sampling temperature (default 0.4)
  -max-tokens int    max output tokens for the design (default 8000)
  -no-stream         wait for the full response instead of streaming
  -agents string     ensemble mode: full (explorer + critics), fast (single call), off (reserved)
  -critic-model string  cheaper model for the critic panel (default: the design model)
  -rounds int        designer-critic debate rounds, 1-3 (default 1)
`)
}
