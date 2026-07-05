package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const defaultAnthropicModel = "claude-fable-5"

// anthropicProvider talks to the Anthropic Messages API using SSE streaming.
type anthropicProvider struct {
	model     string
	temp      float64
	maxTokens int
}

func (a *anthropicProvider) Name() string  { return "anthropic/" + a.model }
func (a *anthropicProvider) Model() string { return a.model }

func (a *anthropicProvider) Complete(ctx context.Context, system, user string, progress io.Writer) (string, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return "", errors.New("ANTHROPIC_API_KEY is not set")
	}

	body, _ := json.Marshal(map[string]any{
		"model":       a.model,
		"max_tokens":  a.maxTokens,
		"temperature": a.temp,
		"system":      system,
		"stream":      true,
		"messages": []map[string]string{
			{"role": "user", "content": user},
		},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := sharedClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("anthropic %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	var out strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		text, stop := parseAnthropicSSE(sc.Text())
		if text != "" {
			out.WriteString(text)
			if progress != nil {
				progress.Write([]byte(text))
			}
		}
		if stop {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return out.String(), err
	}
	return out.String(), nil
}

// parseAnthropicSSE extracts text from one SSE line, reporting stop at the end
// of the message. Non-data and unrelated event lines yield ("", false).
func parseAnthropicSSE(line string) (text string, stop bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	payload := strings.TrimSpace(line[len("data:"):])
	if payload == "" {
		return "", false
	}
	var ev struct {
		Type  string `json:"type"`
		Delta struct {
			Text string `json:"text"`
		} `json:"delta"`
	}
	if json.Unmarshal([]byte(payload), &ev) != nil {
		return "", false
	}
	switch ev.Type {
	case "content_block_delta":
		return ev.Delta.Text, false
	case "message_stop":
		return "", true
	}
	return "", false
}
