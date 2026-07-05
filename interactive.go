package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

var stdin = bufio.NewReader(os.Stdin)

// readLine prompts on stderr and reads one trimmed line from stdin.
func readLine(prompt string) string {
	fmt.Fprint(os.Stderr, "  "+cyan("›")+" "+prompt+" ")
	s, _ := stdin.ReadString('\n')
	return strings.TrimSpace(s)
}

// confirm asks a yes/no question, defaulting to no.
func confirm(prompt string) bool {
	r := strings.ToLower(readLine(prompt + dim(" [y/N]")))
	return r == "y" || r == "yes"
}

// askQuestions presents each clarifying question with its scenario and options,
// and collects the user's answers into a labeled block for the design prompt.
func askQuestions(qs []question) string {
	if len(qs) == 0 {
		return ""
	}
	os.Stderr.WriteString("\n")
	info(bold("A few questions before I design") + dim("  (Enter to skip any)"))
	os.Stderr.WriteString("\n")
	var b strings.Builder
	for i, q := range qs {
		fmt.Fprintf(os.Stderr, "  %s %s\n", cyan(fmt.Sprintf("%d.", i+1)), bold(q.Q))
		if q.Why != "" {
			fmt.Fprintln(os.Stderr, "     "+dim("why: "+q.Why))
		}
		if q.Options != "" && !strings.EqualFold(q.Options, "open") {
			fmt.Fprintln(os.Stderr, "     "+dim("e.g. "+q.Options))
		}
		ans := readLine(dim("your answer:"))
		os.Stderr.WriteString("\n")
		if ans == "" {
			ans = "(no preference — use your best judgment)"
		}
		fmt.Fprintf(&b, "Q: %s\nA: %s\n\n", q.Q, ans)
	}
	return b.String()
}

// reviewChoice presents the post-design menu and returns the chosen key.
func reviewChoice() string {
	os.Stderr.WriteString("\n")
	fmt.Fprintln(os.Stderr, "  "+dim("─────────────────────────────"))
	fmt.Fprintln(os.Stderr, "  "+bold("What next?"))
	fmt.Fprintln(os.Stderr, "    "+cyan("a")+" approve & build the code")
	fmt.Fprintln(os.Stderr, "    "+cyan("m")+" modify the design")
	fmt.Fprintln(os.Stderr, "    "+cyan("q")+" ask a question")
	fmt.Fprintln(os.Stderr, "    "+cyan("w")+" write the design to a file")
	fmt.Fprintln(os.Stderr, "    "+cyan("x")+" exit")
	c := strings.ToLower(readLine("choice:"))
	if c == "" {
		return "x"
	}
	return c[:1]
}
