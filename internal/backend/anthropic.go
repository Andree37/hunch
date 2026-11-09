package backend

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/Andree37/hunch/internal/flow"
)

// anthropicChat calls a model through the Anthropic API. Options:
//
//	model:       required, the model ID
//	api_key:     the API key
//	base_url:    override the API endpoint, e.g. for a proxy
//	timeout:     seconds per call, retries included (default 120; 0 = no limit)
//
// Only these options pick the key and endpoint: ANTHROPIC_BASE_URL,
// ANTHROPIC_AUTH_TOKEN and SDK profiles in the environment are ignored, so
// a flow never sends a key it didn't name to a server it didn't name.
type anthropicChat struct {
	client  anthropic.Client
	model   string
	timeout time.Duration
	opts    map[string]any
}

func newAnthropic(cfg flow.BackendConfig) (*chatBackend, error) {
	o := cfg.Options
	model := optString(o, "model", "")
	if model == "" {
		return nil, fmt.Errorf("backend %q: model is required (the model ID)", cfg.Name)
	}
	key, err := apiKey(o)
	if err != nil {
		return nil, fmt.Errorf("model %q: %w", cfg.Name, err)
	}
	base := optString(o, "base_url", "")
	// Anthropic's own API needs a key; a proxy may add its own auth.
	if key == "" && isAnthropicAPI(base) {
		return nil, fmt.Errorf("model %q: no key: add key: to it in hunch.yaml", cfg.Name)
	}
	opts := []option.RequestOption{option.WithoutEnvironmentDefaults()}
	if key != "" {
		opts = append(opts, option.WithAPIKey(key))
	}
	if base != "" {
		opts = append(opts, option.WithBaseURL(base))
	}
	m := &anthropicChat{
		client:  anthropic.NewClient(opts...),
		model:   model,
		timeout: optSeconds(o, "timeout", 120),
		opts:    o,
	}
	return newChatBackend(cfg, m), nil
}

func (m *anthropicChat) chat(ctx context.Context, r chatRequest) (string, float64, error) {
	if m.timeout > 0 { // 0 means no limit
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, m.timeout)
		defer cancel()
	}
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

// isAnthropicAPI reports whether base is Anthropic's own API (or unset,
// which means it), as opposed to a proxy or gateway.
func isAnthropicAPI(base string) bool {
	if base == "" {
		return true
	}
	u, err := url.Parse(base)
	return err == nil && strings.EqualFold(u.Hostname(), "api.anthropic.com")
}
