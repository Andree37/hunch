// Package backend defines the decision backend contract. Jev is one
// implementation; local models and mocks are others.
package backend

import (
	"context"
	"fmt"

	"github.com/Andree37/hunch/internal/flow"
)

// Caps describes what a backend supports natively. The runner fills gaps,
// e.g. by fanning out questions for backends without MultiQuestion.
type Caps struct {
	MultiQuestion bool // answers several questions in one call
	Calibrated    bool // probabilities can be trusted as probabilities
	CostReporting bool // reports cost per call
}

type Decision struct {
	QuestionID string             `json:"question"`
	Kind       flow.Kind          `json:"kind"`
	Answer     any                `json:"answer"` // bool, string (choice) or float64 (score)
	Probs      map[string]float64 `json:"probs"`  // bool: yes/no; choice: per option; score: per level
	Confidence float64            `json:"confidence"`
}

type Response struct {
	Decisions []Decision // one per question, in order
	CostUSD   float64
}

type Backend interface {
	Name() string
	Caps() Caps
	Decide(ctx context.Context, state map[string]any, qs []flow.Question) (Response, error)
}

func New(cfg flow.BackendConfig) (Backend, error) {
	switch cfg.Kind {
	case "mock":
		return NewMock(cfg)
	case "jev":
		return NewJev(cfg)
	}
	return nil, fmt.Errorf("backend %q: kind %q is not supported yet", cfg.Name, cfg.Kind)
}

type statusKey struct{}

// WithStatus returns a context that carries a callback for progress notes a
// backend wants to surface while a call is slow, such as retries.
func WithStatus(ctx context.Context, fn func(string)) context.Context {
	return context.WithValue(ctx, statusKey{}, fn)
}

func reportStatus(ctx context.Context, format string, args ...any) {
	if fn, ok := ctx.Value(statusKey{}).(func(string)); ok {
		fn(fmt.Sprintf(format, args...))
	}
}
