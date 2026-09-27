package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"

	"github.com/Andree37/hunch/internal/flow"
)

// Mock answers deterministically from a hash of the question and state, so
// the same input always takes the same path and changing any state field can
// flip decisions. Options:
//
//	seed:    any value, changes every answer
//	answers: {question_id: answer} to force specific answers
type Mock struct {
	name    string
	seed    string
	answers map[string]any
}

func NewMock(cfg flow.BackendConfig) (*Mock, error) {
	m := &Mock{name: cfg.Name}
	if s, ok := cfg.Options["seed"]; ok {
		m.seed = fmt.Sprint(s)
	}
	if a, ok := cfg.Options["answers"]; ok {
		answers, ok := a.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("backend %q: answers must be a map", cfg.Name)
		}
		m.answers = answers
	}
	return m, nil
}

func (m *Mock) Name() string { return m.name }

func (m *Mock) Caps() Caps { return Caps{MultiQuestion: true, CostReporting: true} }

func (m *Mock) Decide(_ context.Context, state map[string]any, qs []flow.Question) (Response, error) {
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return Response{}, err
	}
	var resp Response
	for _, q := range qs {
		rng := m.rng(q, stateJSON)
		forced, isForced := m.answers[q.ID]
		var d Decision
		switch q.Kind {
		case flow.Bool:
			p := rng.Float64()
			if isForced {
				yes, err := forcedBool(forced)
				if err != nil {
					return resp, fmt.Errorf("mock answer for %s: %v", q.ID, err)
				}
				p = 0.05
				if yes {
					p = 0.95
				}
			}
			d = boolDecision(q, p)
		case flow.Choice:
			probs := peaky(rng, len(q.Options))
			if isForced {
				i := slices.Index(q.Options, fmt.Sprint(forced))
				if i < 0 {
					return resp, fmt.Errorf("mock answer for %s: %v is not an option", q.ID, forced)
				}
				probs = spike(len(q.Options), i)
			}
			d = choiceDecision(q, probs)
		case flow.Score:
			n := q.Scale.Max - q.Scale.Min + 1
			probs := peaky(rng, n)
			if isForced {
				v, ok := forced.(float64)
				if i, isInt := forced.(int); isInt {
					v, ok = float64(i), true
				}
				level := int(math.Round(v))
				if !ok || level < q.Scale.Min || level > q.Scale.Max {
					return resp, fmt.Errorf("mock answer for %s: want a number in %s", q.ID, q.Scale)
				}
				probs = spike(n, level-q.Scale.Min)
			}
			d = scoreDecision(q, probs)
		default:
			return resp, fmt.Errorf("mock: unsupported question kind %q", q.Kind)
		}
		resp.Decisions = append(resp.Decisions, d)
	}
	return resp, nil
}

func (m *Mock) rng(q flow.Question, state []byte) *rand.Rand {
	h := fnv.New64a()
	for _, part := range [][]byte{[]byte(m.seed), []byte(q.ID), []byte(q.Text), state} {
		h.Write(part)
		h.Write([]byte{0})
	}
	sum := h.Sum64()
	return rand.New(rand.NewPCG(sum, sum>>1|1))
}

func forcedBool(v any) (bool, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		switch x {
		case "yes", "true":
			return true, nil
		case "no", "false":
			return false, nil
		}
	}
	return false, fmt.Errorf("want yes/no, got %v", v)
}

// peaky returns a random distribution that usually has one clear favourite.
func peaky(rng *rand.Rand, n int) []float64 {
	w := make([]float64, n)
	var sum float64
	for i := range w {
		x := rng.Float64()
		w[i] = x * x * x
		sum += w[i]
	}
	for i := range w {
		w[i] /= sum
	}
	return w
}

func spike(n, at int) []float64 {
	w := make([]float64, n)
	for i := range w {
		w[i] = 0.1 / float64(n-1)
	}
	w[at] = 0.9
	return w
}

func boolDecision(q flow.Question, p float64) Decision {
	return Decision{
		QuestionID: q.ID,
		Kind:       flow.Bool,
		Answer:     p >= 0.5,
		Probs:      map[string]float64{"yes": p, "no": 1 - p},
		Confidence: max(p, 1-p),
	}
}

func choiceDecision(q flow.Question, probs []float64) Decision {
	d := Decision{QuestionID: q.ID, Kind: flow.Choice, Probs: map[string]float64{}}
	best := 0
	for i, p := range probs {
		d.Probs[q.Options[i]] = p
		if p > probs[best] {
			best = i
		}
	}
	d.Answer = q.Options[best]
	d.Confidence = probs[best]
	return d
}

func scoreDecision(q flow.Question, probs []float64) Decision {
	d := Decision{QuestionID: q.ID, Kind: flow.Score, Probs: map[string]float64{}}
	var ev, top float64
	for i, p := range probs {
		level := q.Scale.Min + i
		d.Probs[strconv.Itoa(level)] = p
		ev += float64(level) * p
		top = max(top, p)
	}
	d.Answer = ev
	d.Confidence = top
	return d
}

// Write returns a deterministic stand-in, so flows with llm nodes run offline.
func (m *Mock) Write(_ context.Context, req WriteRequest) (WriteResponse, error) {
	h := fnv.New32a()
	h.Write([]byte(m.seed + req.System + req.Prompt))
	first, _, _ := strings.Cut(req.Prompt, "\n")
	if len(first) > 60 {
		first = first[:60] + "…"
	}
	return WriteResponse{Text: fmt.Sprintf("[mock text %04x] %s", h.Sum32()&0xffff, first)}, nil
}
