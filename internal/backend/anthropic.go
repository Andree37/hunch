package backend

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/Andree37/hunch/internal/flow"
)

// anthropicChat calls a model through the Anthropic API. Options:
//
//	model:       required, the model ID
//	api_key_env: default ANTHROPIC_API_KEY
//	base_url:    override the API endpoint, e.g. for a proxy
//	timeout:     seconds per call, retries included (default 120)
//
// Only these options pick the key and endpoint: ANTHROPIC_BASE_URL,
// ANTHROPIC_AUTH_TOKEN and SDK profiles in the environment are ignored, so
// a flow never sends a key it didn't name to a server it didn't name.
type anthropicChat struct {
	client  anthropic.Client
	model   string
	keyEnv  string
	hasKey  bool
	timeout time.Duration
	opts    map[string]any
}

func newAnthropic(cfg flow.BackendConfig) (*chatBackend, error) {
	o := cfg.Options
	model := optString(o, "model", "")
	if model == "" {
		return nil, fmt.Errorf("backend %q: model is required (the model ID)", cfg.Name)
	}
	keyEnv := optString(o, "api_key_env", "ANTHROPIC_API_KEY")
	key := os.Getenv(keyEnv)
	opts := []option.RequestOption{option.WithoutEnvironmentDefaults()}
	if key != "" {
		opts = append(opts, option.WithAPIKey(key))
	}
	if u := optString(o, "base_url", ""); u != "" {
		opts = append(opts, option.WithBaseURL(u))
	}
	m := &anthropicChat{
		client:  anthropic.NewClient(opts...),
		model:   model,
		keyEnv:  keyEnv,
		hasKey:  key != "" || optString(o, "base_url", "") != "",
		timeout: optSeconds(o, "timeout", 120),
		opts:    o,
	}
	return newChatBackend(cfg, m), nil
}

func (m *anthropicChat) chat(ctx context.Context, r chatRequest) (string, float64, error) {
	if !m.hasKey {
		return "", 0, fmt.Errorf("%s is not set", m.keyEnv)
	}
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(m.model),
		MaxTokens: int64(r.MaxTokens),
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(r.User))},
	}
	if r.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: r.System}}
	}
	resp, err := m.client.Messages.New(ctx, params)
	if err != nil {
		return "", 0, err
	}
	cost := tokenCost(m.opts, resp.Usage.InputTokens, resp.Usage.OutputTokens)
	if resp.StopReason == anthropic.StopReasonRefusal {
		return "", cost, fmt.Errorf("model declined the request (%s)", resp.StopDetails.Category)
	}
	var b strings.Builder
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String(), cost, nil
}
