package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/Andree37/hunch/internal/flow"
)

// openAIChat talks to any OpenAI-compatible chat completions endpoint:
// OpenAI itself, Ollama, OpenRouter, vLLM, LM Studio... Options:
//
//	endpoint:    the chat completions URL, e.g. http://localhost:11434/v1/chat/completions
//	model:       required
//	api_key:     sent when set (local servers need none)
//	key_header:  "api-key" for Azure OpenAI; otherwise a bearer token
//	structured:  default true; decisions send a JSON schema the server must
//	             follow (response_format). Set false if a server rejects it.
type openAIChat struct {
	url, model, key, keyHeader string
	structured                 bool
	opts                       map[string]any
	client                     *http.Client
}

func newOpenAI(cfg flow.BackendConfig) (*chatBackend, error) {
	o := cfg.Options
	model := optString(o, "model", "")
	if model == "" {
		return nil, fmt.Errorf("model %q: say which model the server should run (model: ...)", cfg.Name)
	}
	m := &openAIChat{
		url:        optString(o, "endpoint", "https://api.openai.com/v1/chat/completions"),
		model:      model,
		key:        optString(o, "api_key", ""),
		keyHeader:  optString(o, "key_header", ""),
		structured: o["structured"] != false,
		opts:       o,
		client:     &http.Client{Timeout: optSeconds(o, "timeout", 120)},
	}
	return newChatBackend(cfg, m), nil
}

func (m *openAIChat) chat(ctx context.Context, r chatRequest) (string, float64, error) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	body := map[string]any{
		"model":      m.model,
		"max_tokens": r.MaxTokens,
		"messages":   []msg{{"system", r.System}, {"user", r.User}},
	}
	if r.Schema != nil && m.structured {
		body["response_format"] = map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": "decision", "strict": true, "schema": r.Schema},
		}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.url, bytes.NewReader(data))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	switch {
	case m.key != "" && m.keyHeader != "":
		req.Header.Set(m.keyHeader, m.key)
	case m.key != "":
		req.Header.Set("Authorization", "Bearer "+m.key)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, (&httpError{status: resp.StatusCode, body: string(raw)})
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", 0, fmt.Errorf("bad response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", 0, fmt.Errorf("response has no choices")
	}
	return out.Choices[0].Message.Content, tokenCost(m.opts, out.Usage.PromptTokens, out.Usage.CompletionTokens), nil
}
