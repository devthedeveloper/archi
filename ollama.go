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

const defaultOllamaModel = "glm-5.2:cloud"

// ollamaProvider talks to Ollama Cloud (or a local Ollama via OLLAMA_HOST)
// using the /api/chat streaming NDJSON protocol.
type ollamaProvider struct {
	model     string
	temp      float64
	maxTokens int
}

func (o *ollamaProvider) Name() string  { return "ollama/" + o.model }
func (o *ollamaProvider) Model() string { return o.model }

func (o *ollamaProvider) Complete(ctx context.Context, system, user string, progress io.Writer) (string, error) {
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = "https://ollama.com"
	}
	host = strings.TrimRight(host, "/")

	key := os.Getenv("OLLAMA_API_KEY")
	cloud := strings.Contains(host, "ollama.com")
	if cloud && key == "" {
		return "", errors.New("OLLAMA_API_KEY is not set (needed for Ollama Cloud; set OLLAMA_HOST for a local server)")
	}

	ctx, cancel, err := requestContext(ctx)
	if err != nil {
		return "", err
	}
	defer cancel()

	body, _ := json.Marshal(map[string]any{
		"model": o.model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"stream": true,
		"options": map[string]any{
			"temperature": o.temp,
			"num_predict": o.maxTokens,
		},
	})

	resp, err := doWithRetry(ctx, sharedClient, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/api/chat", bytes.NewReader(body))
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
		return "", fmt.Errorf("ollama %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	var out strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		text, done, err := parseOllamaLine(sc.Bytes())
		if err != nil {
			return out.String(), err
		}
		if text != "" {
			out.WriteString(text)
			if progress != nil {
				progress.Write([]byte(text))
			}
		}
		if done {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return out.String(), err
	}
	return out.String(), nil
}

// parseOllamaLine extracts the content chunk and done flag from one NDJSON line.
func parseOllamaLine(line []byte) (text string, done bool, err error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return "", false, nil
	}
	var m struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Done  bool   `json:"done"`
		Error string `json:"error"`
	}
	if e := json.Unmarshal(line, &m); e != nil {
		return "", false, e
	}
	if m.Error != "" {
		return "", false, errors.New(m.Error)
	}
	return m.Message.Content, m.Done, nil
}
