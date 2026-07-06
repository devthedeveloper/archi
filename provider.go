package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// Provider turns a system + user prompt into a completed document. If progress
// is non-nil, each streamed chunk is written to it as it arrives.
type Provider interface {
	Name() string  // e.g. "ollama/glm-5.2:cloud", for display
	Model() string // the resolved model id, for config
	Complete(ctx context.Context, system, user string, progress io.Writer) (string, error)
}

// sharedClient is reused across requests. It has no overall timeout — a
// streamed design can legitimately run for a long time — but connecting and
// waiting for the response headers are bounded so a dead server fails fast.
// Set ARCHI_TIMEOUT for an overall per-request cap (see requestContext).
var sharedClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	},
}

// requestContext applies the optional ARCHI_TIMEOUT overall deadline (a Go
// duration string such as "10m") to ctx. Unset means no overall deadline. The
// returned cancel must be called once the request — streaming included — is done.
func requestContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	v := os.Getenv("ARCHI_TIMEOUT")
	if v == "" {
		return ctx, func() {}, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return nil, nil, fmt.Errorf("invalid ARCHI_TIMEOUT %q (want a Go duration such as \"10m\")", v)
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	return ctx, cancel, nil
}

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
	case "openai":
		if model == "" {
			model = defaultOpenAIModel
		}
		return &openaiProvider{model: model, temp: temp, maxTokens: maxTokens}, nil
	default:
		return nil, fmt.Errorf("unknown provider %q (valid: ollama, anthropic, openai)", name)
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
