package backend

import (
	"context"
	"fmt"
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
	keyErr  error // no usable key for Anthropic's own API
	timeout time.Duration
	opts    map[string]any
}

func newAnthropic(cfg flow.BackendConfig) (*chatBackend, error) {
	o := cfg.Options
	model := optString(o, "model", "")
	if model == "" {
		return nil, fmt.Errorf("backend %q: model is required (the model ID)", cfg.Name)
	}
	key, keyErr := apiKey(o)
	base := optString(o, "base_url", "")
	// Anthropic's own API needs a key; a proxy may add its own auth.
	if keyErr == nil && key == "" && (base == "" || strings.Contains(base, "api.anthropic.com")) {
		keyErr = fmt.Errorf("no key: add key: to this model in hunch.yaml")
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
		keyErr:  keyErr,
		timeout: optSeconds(o, "timeout", 120),
		opts:    o,
	}
	return newChatBackend(cfg, m), nil
}

func (m *anthropicChat) chat(ctx context.Context, r chatRequest) (string, float64, error) {
	if m.keyErr != nil {
		return "", 0, m.keyErr
	}
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
