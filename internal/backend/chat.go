package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
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
// text out. Everything else (writing, deciding) is built on it. A provider
// that can enforce a JSON schema on the reply uses schema when given.
type chatModel interface {
	chat(ctx context.Context, req chatRequest) (text string, cost float64, err error)
}

type chatRequest struct {
	System, User string
	MaxTokens    int
	Schema       map[string]any // optional: the reply must match this JSON schema
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
	text, cost, err := c.model.chat(ctx, chatRequest{System: req.System, User: req.Prompt, MaxTokens: max})
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
	req := chatRequest{System: decideSystem, User: prompt, MaxTokens: 1024, Schema: decideSchema(qs)}
	var total float64
	// Small models sometimes wander off the format; one more try usually
	// lands it.
	for attempt := 1; ; attempt++ {
		text, cost, err := c.model.chat(ctx, req)
		total += cost
		if err != nil {
			return Response{}, fmt.Errorf("backend %s: %w", c.name, err)
		}
		decs, perr := parseDecisions(text, qs)
		if perr == nil {
			return Response{Decisions: decs, CostUSD: total}, nil
		}
		if attempt == 2 {
			return Response{}, fmt.Errorf("backend %s: %w", c.name, perr)
		}
		ReportStatus(ctx, "%s: reply didn't fit the format (%v), asking again", c.name, perr)
	}
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

// decideSchema is the exact reply shape, for providers that enforce it:
// one probability per allowed answer of every question.
func decideSchema(qs []flow.Question) map[string]any {
	obj := func(props map[string]any) map[string]any {
		req := make([]string, 0, len(props))
		for k := range props {
			req = append(req, k)
		}
		slices.Sort(req)
		return map[string]any{"type": "object", "properties": props, "required": req, "additionalProperties": false}
	}
	answers := map[string]any{}
	for _, q := range qs {
		probs := map[string]any{}
		for _, a := range allowedAnswers(q) {
			probs[a.key] = map[string]any{"type": "number"}
		}
		answers[q.Name] = obj(map[string]any{"probs": obj(probs)})
	}
	return obj(map[string]any{"answers": obj(answers)})
}

// parseDecisions reads the first JSON object in a reply. It is lenient
// about what small models do: numbers written as strings, text around the
// JSON, and a single {"answer", "confidence"} instead of probabilities.
func parseDecisions(text string, qs []flow.Question) ([]Decision, error) {
	start := strings.Index(text, "{")
	if start < 0 {
		return nil, fmt.Errorf("model did not reply with JSON: %.120q", text)
	}
	var reply struct {
		Answers map[string]struct {
			Probs      map[string]any `json:"probs"`
			Answer     any            `json:"answer"`
			Confidence any            `json:"confidence"`
		} `json:"answers"`
	}
	if err := json.NewDecoder(strings.NewReader(text[start:])).Decode(&reply); err != nil {
		return nil, fmt.Errorf("model reply is not valid JSON: %v", err)
	}

	out := make([]Decision, len(qs))
	for i, q := range qs {
		a, ok := reply.Answers[q.Name]
		if !ok {
			return nil, fmt.Errorf("model gave no answer for %q", q.Name)
		}
		raw := map[string]float64{}
		for k, v := range a.Probs {
			if p, ok := number(v); ok {
				raw[k] = p
			}
		}
		// {"answer": "yes", "confidence": 0.8}: the rest shares what's left.
		if len(raw) == 0 && a.Answer != nil {
			keys := allowedAnswers(q)
			chosen := strings.ToLower(fmt.Sprint(a.Answer))
			conf, ok := number(a.Confidence)
			if !ok {
				conf = 1
			}
			for _, k := range keys {
				if k.key == chosen {
					raw[k.key] = conf
				} else if len(keys) > 1 {
					raw[k.key] = (1 - conf) / float64(len(keys)-1)
				}
			}
		}
		probs := map[string]float64{}
		var sum float64
		for _, k := range allowedAnswers(q) {
			p := math.Max(raw[k.key], 0)
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

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
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
