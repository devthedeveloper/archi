package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Provider turns a system + user prompt into a completed document. If progress
// is non-nil, each streamed chunk is written to it as it arrives.
type Provider interface {
	Name() string  // e.g. "ollama/glm-5.2:cloud", for display
	Model() string // the resolved model id, for config
	Complete(ctx context.Context, system, user string, progress io.Writer) (string, error)
}

// sharedClient is reused across requests; the long timeout covers slow models.
var sharedClient = &http.Client{Timeout: 5 * time.Minute}

// newProvider selects a provider implementation. An empty model falls back to
// the provider's default.
func newProvider(name, model string, temp float64, maxTokens int) (Provider, error) {
	switch name {
	case "ollama", "":
		if model == "" {
			model = defaultOllamaModel
		}
		return &ollamaProvider{model: model, temp: temp, maxTokens: maxTokens}, nil
	case "anthropic":
		if model == "" {
			model = defaultAnthropicModel
		}
		return &anthropicProvider{model: model, temp: temp, maxTokens: maxTokens}, nil
	default:
		return nil, fmt.Errorf("unknown provider %q (valid: ollama, anthropic)", name)
	}
}

// runModel calls the provider, streaming chunks to live (if non-nil) and
// updating sp's token counter (if non-nil). It returns the full document.
func runModel(ctx context.Context, p Provider, system, user string, live io.Writer, sp *Spinner) (string, error) {
	chars := 0
	progress := funcWriter(func(b []byte) (int, error) {
		if live != nil {
			live.Write(b)
		}
		if sp != nil {
			chars += len(b)
			sp.SetSuffix(fmt.Sprintf("~%s tokens", humanCount((chars+3)/4)))
		}
		return len(b), nil
	})
	return p.Complete(ctx, system, user, progress)
}
