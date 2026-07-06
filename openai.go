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

const defaultOpenAIModel = "gpt-5.2"

// defaultOpenAIBaseURL is used when OPENAI_BASE_URL is unset.
const defaultOpenAIBaseURL = "https://api.openai.com/v1"

// openaiProvider talks to the OpenAI Chat Completions API — or any
// OpenAI-compatible server (OpenRouter, Groq, vLLM, …) via OPENAI_BASE_URL —
// using SSE streaming.
type openaiProvider struct {
	model     string
	temp      float64
	maxTokens int
}

func (o *openaiProvider) Name() string  { return "openai/" + o.model }
func (o *openaiProvider) Model() string { return o.model }

func (o *openaiProvider) Complete(ctx context.Context, system, user string, progress io.Writer) (string, error) {
	base := os.Getenv("OPENAI_BASE_URL")
	if base == "" {
		base = defaultOpenAIBaseURL
	}
	base = strings.TrimRight(base, "/")

	key := os.Getenv("OPENAI_API_KEY")
	if key == "" && strings.Contains(base, "api.openai.com") {
		return "", errors.New("OPENAI_API_KEY is not set (also set OPENAI_BASE_URL for OpenRouter/Groq/local servers)")
	}

	ctx, cancel, err := requestContext(ctx)
	if err != nil {
		return "", err
	}
	defer cancel()

	body, _ := json.Marshal(map[string]any{
		"model":       o.model,
		"temperature": o.temp,
		"max_tokens":  o.maxTokens,
		"stream":      true,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	})

	resp, err := doWithRetry(ctx, sharedClient, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		return req, nil
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("openai %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	var out strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		text, stop := parseOpenAISSE(sc.Text())
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

// parseOpenAISSE extracts the content delta from one Chat Completions SSE
// line, reporting stop at the "data: [DONE]" terminator. Non-data lines,
// keep-alives, and unrelated events yield ("", false).
func parseOpenAISSE(line string) (text string, stop bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	payload := strings.TrimSpace(line[len("data:"):])
	if payload == "" {
		return "", false
	}
	if payload == "[DONE]" {
		return "", true
	}
	var ev struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(payload), &ev) != nil || len(ev.Choices) == 0 {
		return "", false
	}
	return ev.Choices[0].Delta.Content, false
}
