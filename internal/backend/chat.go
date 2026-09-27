package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/Andree37/hunch/internal/flow"
)

// Writer generates text, for `llm` action nodes.
type Writer interface {
	Write(ctx context.Context, req WriteRequest) (WriteResponse, error)
}

type WriteRequest struct {
	System    string
	Prompt    string
	MaxTokens int // 0 means the backend's default
}

type WriteResponse struct {
	Text    string
	CostUSD float64
}

// chatModel is one provider's plain chat call: system + user message in,
// text out. Everything else (writing, deciding) is built on it.
type chatModel interface {
	chat(ctx context.Context, system, user string, maxTokens int) (text string, cost float64, err error)
}

// chatBackend makes any chat model both a Writer and a decision Backend.
// Decisions are asked for as JSON probabilities; the model reports its own
// confidence, so answers are not calibrated the way Jev's are.
type chatBackend struct {
	name      string
	model     chatModel
	maxTokens int
}

func newChatBackend(cfg flow.BackendConfig, m chatModel) *chatBackend {
	return &chatBackend{name: cfg.Name, model: m, maxTokens: int(optFloat(cfg.Options, "max_tokens", 4096))}
}

func (c *chatBackend) Name() string { return c.name }

func (c *chatBackend) Caps() Caps { return Caps{MultiQuestion: true, CostReporting: true} }

func (c *chatBackend) Write(ctx context.Context, req WriteRequest) (WriteResponse, error) {
	max := req.MaxTokens
	if max == 0 {
		max = c.maxTokens
	}
	text, cost, err := c.model.chat(ctx, req.System, req.Prompt, max)
	if err != nil {
		return WriteResponse{}, fmt.Errorf("backend %s: %w", c.name, err)
	}
	return WriteResponse{Text: text, CostUSD: cost}, nil
}

const decideSystem = `You make typed decisions about the state you are given.
Answer every question. Reply with one JSON object and nothing else, shaped:
{"answers": {"<question id>": {"probs": {"<answer>": <probability>, ...}}, ...}}
Give a probability for every allowed answer; they should sum to 1 and reflect
how sure you are.`

func (c *chatBackend) Decide(ctx context.Context, state map[string]any, qs []flow.Question) (Response, error) {
	prompt, err := decidePrompt(state, qs)
	if err != nil {
		return Response{}, err
	}
	text, cost, err := c.model.chat(ctx, decideSystem, prompt, 1024)
	if err != nil {
		return Response{}, fmt.Errorf("backend %s: %w", c.name, err)
	}
	decs, err := parseDecisions(text, qs)
	if err != nil {
		return Response{}, fmt.Errorf("backend %s: %w", c.name, err)
	}
	return Response{Decisions: decs, CostUSD: cost}, nil
}

// decidePrompt lays out the state and each question with its allowed answers
// and what they mean.
func decidePrompt(state map[string]any, qs []flow.Question) (string, error) {
	s, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "State:\n%s\n\nQuestions:\n", s)
	for _, q := range qs {
		fmt.Fprintf(&b, "\n- id: %s\n  question: %s\n  allowed answers:\n", q.Name, q.Text)
		for _, a := range allowedAnswers(q) {
			line := "    - " + a.key
			if a.meaning != "" {
				line += ": " + a.meaning
			}
			b.WriteString(line + "\n")
		}
	}
	return b.String(), nil
}

type answerKey struct{ key, meaning string }

func allowedAnswers(q flow.Question) []answerKey {
	var out []answerKey
	switch q.Kind {
	case flow.Bool:
		for _, k := range []string{"yes", "no"} {
			out = append(out, answerKey{k, q.Criteria[k]})
		}
	case flow.Choice:
		for _, o := range q.Options {
			out = append(out, answerKey{o, q.Criteria[o]})
		}
	case flow.Score:
		for l := q.Scale.Min; l <= q.Scale.Max; l++ {
			meaning := scaleLabel(l, q.Scale)
			if i := l - q.Scale.Min; i < len(q.Levels) {
				meaning = q.Levels[i]
			}
			out = append(out, answerKey{strconv.Itoa(l), meaning})
		}
	}
	return out
}

func parseDecisions(text string, qs []flow.Question) ([]Decision, error) {
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return nil, fmt.Errorf("model did not reply with JSON: %.120q", text)
	}
	var reply struct {
		Answers map[string]struct {
			Probs map[string]float64 `json:"probs"`
		} `json:"answers"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &reply); err != nil {
		return nil, fmt.Errorf("model reply is not valid JSON: %v", err)
	}

	out := make([]Decision, len(qs))
	for i, q := range qs {
		a, ok := reply.Answers[q.Name]
		if !ok {
			return nil, fmt.Errorf("model gave no answer for %q", q.Name)
		}
		probs := map[string]float64{}
		var sum float64
		for _, k := range allowedAnswers(q) {
			p := math.Max(a.Probs[k.key], 0)
			probs[k.key] = p
			sum += p
		}
		if sum == 0 {
			return nil, fmt.Errorf("model gave no usable probabilities for %q", q.Name)
		}
		for k := range probs {
			probs[k] /= sum
		}
		out[i] = decisionFromProbs(q, probs)
	}
	return out, nil
}

// decisionFromProbs turns a normalised distribution into a typed decision.
func decisionFromProbs(q flow.Question, probs map[string]float64) Decision {
	switch q.Kind {
	case flow.Bool:
		return boolDecision(q, probs["yes"])
	case flow.Choice:
		ps := make([]float64, len(q.Options))
		for i, o := range q.Options {
			ps[i] = probs[o]
		}
		return choiceDecision(q, ps)
	}
	ps := make([]float64, q.Scale.Max-q.Scale.Min+1)
	for i := range ps {
		ps[i] = probs[strconv.Itoa(q.Scale.Min+i)]
	}
	return scoreDecision(q, ps)
}

// tokenCost prices a call from per-million-token rates given in a backend's
// options (price_in_per_mtok, price_out_per_mtok); unset rates cost nothing.
func tokenCost(opts map[string]any, in, out int64) float64 {
	return (float64(in)*optFloat(opts, "price_in_per_mtok", 0) + float64(out)*optFloat(opts, "price_out_per_mtok", 0)) / 1e6
}
