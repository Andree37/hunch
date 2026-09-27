package backend

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/Andree37/hunch/internal/flow"
)

// bedrockChat calls any Amazon Bedrock model through the Converse API, so
// Claude, Llama, Nova, Mistral and the rest all work the same way. AWS
// credentials come from the usual chain (env, profile, SSO, role). Options:
//
//	model:   required, a Bedrock model or inference profile ID
//	region:  default from the AWS config
//	profile: AWS shared-config profile to use
type bedrockChat struct {
	client *bedrockruntime.Client
	model  string
	opts   map[string]any
}

func newBedrock(cfg flow.BackendConfig) (*chatBackend, error) {
	o := cfg.Options
	model := optString(o, "model", "")
	if model == "" {
		return nil, fmt.Errorf("backend %q: model is required (a Bedrock model or inference profile ID)", cfg.Name)
	}
	var loads []func(*config.LoadOptions) error
	if r := optString(o, "region", ""); r != "" {
		loads = append(loads, config.WithRegion(r))
	}
	if p := optString(o, "profile", ""); p != "" {
		loads = append(loads, config.WithSharedConfigProfile(p))
	}
	awsCfg, err := config.LoadDefaultConfig(context.Background(), loads...)
	if err != nil {
		return nil, fmt.Errorf("backend %q: AWS config: %w", cfg.Name, err)
	}
	m := &bedrockChat{client: bedrockruntime.NewFromConfig(awsCfg), model: model, opts: o}
	return newChatBackend(cfg, m), nil
}

func (m *bedrockChat) chat(ctx context.Context, system, user string, maxTokens int) (string, float64, error) {
	in := &bedrockruntime.ConverseInput{
		ModelId: aws.String(m.model),
		Messages: []types.Message{{
			Role:    types.ConversationRoleUser,
			Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: user}},
		}},
		InferenceConfig: &types.InferenceConfiguration{MaxTokens: aws.Int32(int32(maxTokens))},
	}
	if system != "" {
		in.System = []types.SystemContentBlock{&types.SystemContentBlockMemberText{Value: system}}
	}
	out, err := m.client.Converse(ctx, in)
	if err != nil {
		return "", 0, err
	}
	var cost float64
	if u := out.Usage; u != nil {
		cost = tokenCost(m.opts, int64(aws.ToInt32(u.InputTokens)), int64(aws.ToInt32(u.OutputTokens)))
	}
	msg, ok := out.Output.(*types.ConverseOutputMemberMessage)
	if !ok {
		return "", cost, fmt.Errorf("unexpected Converse output %T", out.Output)
	}
	var b strings.Builder
	for _, c := range msg.Value.Content {
		if t, ok := c.(*types.ContentBlockMemberText); ok {
			b.WriteString(t.Value)
		}
	}
	return b.String(), cost, nil
}
