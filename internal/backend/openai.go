package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Andree37/hunch/internal/flow"
)

// openAIChat talks to any OpenAI-compatible chat completions endpoint:
// OpenAI itself, Ollama, OpenRouter, vLLM, LM Studio... Options:
//
//	model:       required
//	base_url:    default https://api.openai.com/v1 (Ollama: http://localhost:11434/v1)
//	api_key_env: default OPENAI_API_KEY; may be unset for local servers
type openAIChat struct {
	url, model, keyEnv string
	opts               map[string]any
	client             *http.Client
}

func newOpenAI(cfg flow.BackendConfig) (*chatBackend, error) {
	o := cfg.Options
	model := optString(o, "model", "")
	if model == "" {
		return nil, fmt.Errorf("backend %q: model is required", cfg.Name)
	}
	m := &openAIChat{
		url:    strings.TrimSuffix(optString(o, "base_url", "https://api.openai.com/v1"), "/") + "/chat/completions",
		model:  model,
		keyEnv: optString(o, "api_key_env", "OPENAI_API_KEY"),
		opts:   o,
		client: &http.Client{Timeout: time.Duration(optFloat(o, "timeout", 120) * float64(time.Second))},
	}
	return newChatBackend(cfg, m), nil
}

func (m *openAIChat) chat(ctx context.Context, system, user string, maxTokens int) (string, float64, error) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	body := map[string]any{
		"model":      m.model,
		"max_tokens": maxTokens,
		"messages":   []msg{{"system", system}, {"user", user}},
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
	if key := os.Getenv(m.keyEnv); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
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
