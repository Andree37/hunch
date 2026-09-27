package backend

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/Andree37/hunch/internal/flow"
)

// anthropicChat calls Claude through the Anthropic API. Options:
//
//	model:       required, e.g. claude-opus-5
//	api_key_env: default ANTHROPIC_API_KEY
//	base_url:    override the API endpoint, e.g. for a proxy
type anthropicChat struct {
	client anthropic.Client
	model  string
	opts   map[string]any
}

func newAnthropic(cfg flow.BackendConfig) (*chatBackend, error) {
	o := cfg.Options
	model := optString(o, "model", "")
	if model == "" {
		return nil, fmt.Errorf("backend %q: model is required (e.g. claude-opus-5)", cfg.Name)
	}
	var opts []option.RequestOption
	if key := os.Getenv(optString(o, "api_key_env", "ANTHROPIC_API_KEY")); key != "" {
		opts = append(opts, option.WithAPIKey(key))
	}
	if u := optString(o, "base_url", ""); u != "" {
		opts = append(opts, option.WithBaseURL(u))
	}
	m := &anthropicChat{client: anthropic.NewClient(opts...), model: model, opts: o}
	return newChatBackend(cfg, m), nil
}

func (m *anthropicChat) chat(ctx context.Context, r chatRequest) (string, float64, error) {
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
