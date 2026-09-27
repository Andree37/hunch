package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/Andree37/hunch/internal/flow"
)

// Jev calls TypeSafe's Jev decision model, either through OpenRouter's
// Decisions API or TypeSafe's own API. Both take the same request body.
// Options:
//
//	provider:       openrouter (default) or typesafe
//	model:          defaults per provider
//	api_key_env:    env var holding the key (OPENROUTER_API_KEY / TYPESAFE_API_KEY)
//	url:            override the endpoint
//	price_per_mtok: USD per million input tokens, used to estimate cost when
//	                the provider doesn't report it (default 0.042)
//	timeout:        seconds per request (default 30)
//	max_retries:    retries on 429, 529, 5xx and network errors (default 3)
type Jev struct {
	name        string
	url         string
	model       string
	keyEnv      string
	pricePerTok float64
	maxRetries  int
	client      *http.Client
	backoff     time.Duration
}

var jevProviders = map[string]struct{ url, model, keyEnv string }{
	"openrouter": {"https://openrouter.ai/api/alpha/decisions", "typesafe/jev-1.13", "OPENROUTER_API_KEY"},
	"typesafe":   {"https://api.typesafe.ai/v1/systemone", "jev-latest", "TYPESAFE_API_KEY"},
}

func NewJev(cfg flow.BackendConfig) (*Jev, error) {
	o := cfg.Options
	provider := optString(o, "provider", "openrouter")
	p, ok := jevProviders[provider]
	if !ok {
		return nil, fmt.Errorf("backend %q: unknown provider %q (want openrouter or typesafe)", cfg.Name, provider)
	}
	return &Jev{
		name:        cfg.Name,
		url:         optString(o, "url", p.url),
		model:       optString(o, "model", p.model),
		keyEnv:      optString(o, "api_key_env", p.keyEnv),
		pricePerTok: optFloat(o, "price_per_mtok", 0.042) / 1e6,
		maxRetries:  int(optFloat(o, "max_retries", 3)),
		client:      &http.Client{Timeout: time.Duration(optFloat(o, "timeout", 30) * float64(time.Second))},
		backoff:     500 * time.Millisecond,
	}, nil
}

func (j *Jev) Name() string { return j.name }

func (j *Jev) Caps() Caps { return Caps{MultiQuestion: true, Calibrated: true, CostReporting: true} }

type jevRequest struct {
	Model     string                 `json:"model"`
	State     any                    `json:"state"`
	Questions map[string]jevQuestion `json:"questions"`
}

type jevQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type jevResponse struct {
	Model   string               `json:"model"`
	Answers map[string]jevAnswer `json:"answers"`
	Usage   struct {
		InputTokens int      `json:"input_tokens"`
		Cost        *float64 `json:"cost"`
	} `json:"usage"`
}

type jevAnswer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul"`
	Choice        string             `json:"choice"`
	Score         *float64           `json:"score"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    *float64           `json:"confidence"`
}

func (j *Jev) Decide(ctx context.Context, state map[string]any, qs []flow.Question) (Response, error) {
	key := os.Getenv(j.keyEnv)
	if key == "" {
		return Response{}, fmt.Errorf("backend %s: $%s is not set", j.name, j.keyEnv)
	}

	req := jevRequest{Model: j.model, State: state, Questions: map[string]jevQuestion{}}
	for _, q := range qs {
		req.Questions[q.Name] = toJevQuestion(q)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}

	var res jevResponse
	if err := j.post(ctx, key, body, &res); err != nil {
		return Response{}, fmt.Errorf("backend %s: %w", j.name, err)
	}

	out := Response{CostUSD: float64(res.Usage.InputTokens) * j.pricePerTok}
	if res.Usage.Cost != nil {
		out.CostUSD = *res.Usage.Cost
	}
	for _, q := range qs {
		a, ok := res.Answers[q.Name]
		if !ok {
			return out, fmt.Errorf("backend %s: no answer for question %q", j.name, q.Name)
		}
		d, err := fromJevAnswer(q, a)
		if err != nil {
			return out, fmt.Errorf("backend %s: question %q: %w", j.name, q.Name, err)
		}
		out.Decisions = append(out.Decisions, d)
	}
	return out, nil
}

func toJevQuestion(q flow.Question) jevQuestion {
	jq := jevQuestion{Instructions: q.Text}
	switch q.Kind {
	case flow.Bool:
		jq.Type = "noul"
		if len(q.Criteria) > 0 {
			c := map[string]string{}
			if y, ok := q.Criteria["yes"]; ok {
				c["true"] = y
			}
			if n, ok := q.Criteria["no"]; ok {
				c["false"] = n
			}
			jq.Criteria = c
		}
	case flow.Choice:
		jq.Type = "choice"
		c := map[string]string{}
		for _, o := range q.Options {
			c[o] = o
			if d, ok := q.Criteria[o]; ok && d != "" {
				c[o] = d
			}
		}
		jq.Criteria = c
	case flow.Score:
		jq.Type = "score"
		levels := q.Levels
		if len(levels) == 0 {
			for l := q.Scale.Min; l <= q.Scale.Max; l++ {
				levels = append(levels, scaleLabel(l, q.Scale))
			}
		}
		jq.Criteria = levels
	}
	return jq
}

func scaleLabel(l int, s flow.Scale) string {
	switch l {
	case s.Min:
		return fmt.Sprintf("%d (lowest)", l)
	case s.Max:
		return fmt.Sprintf("%d (highest)", l)
	}
	return strconv.Itoa(l)
}

func fromJevAnswer(q flow.Question, a jevAnswer) (Decision, error) {
	d := Decision{QuestionID: q.ID, Kind: q.Kind, Probs: map[string]float64{}}
	switch q.Kind {
	case flow.Bool:
		if a.Noul == nil {
			return d, fmt.Errorf("missing noul probability")
		}
		p := *a.Noul
		d.Answer = p >= 0.5
		d.Probs["yes"], d.Probs["no"] = p, 1-p
		d.Confidence = max(p, 1-p)
	case flow.Choice:
		if !slices.Contains(q.Options, a.Choice) {
			return d, fmt.Errorf("answer %q is not an option", a.Choice)
		}
		d.Answer = a.Choice
		d.Probs = a.Probabilities
		d.Confidence = a.Probabilities[a.Choice]
	case flow.Score:
		if a.Score == nil {
			return d, fmt.Errorf("missing score")
		}
		// Jev numbers levels from 0; ours start at the scale's min.
		d.Answer = *a.Score + float64(q.Scale.Min)
		for k, p := range a.Probabilities {
			i, err := strconv.Atoi(k)
			if err != nil {
				return d, fmt.Errorf("bad level %q", k)
			}
			d.Probs[strconv.Itoa(i+q.Scale.Min)] = p
			d.Confidence = max(d.Confidence, p)
		}
	}
	if a.Confidence != nil {
		d.Confidence = *a.Confidence
	}
	return d, nil
}

// post sends body and decodes the JSON response into out, retrying
// rate limits, overloads, server errors and network failures.
func (j *Jev) post(ctx context.Context, key string, body []byte, out any) error {
	var lastErr error
	for attempt := 0; attempt <= j.maxRetries; attempt++ {
		if attempt > 0 {
			delay := j.retryDelay(attempt, lastErr)
			reportStatus(ctx, "%s, retrying in %s (attempt %d/%d)", shortErr(lastErr), delay, attempt+1, j.maxRetries+1)
			if err := sleep(ctx, delay); err != nil {
				return err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Title", "hunch")

		resp, err := j.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = err
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusOK {
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("bad response: %w", err)
			}
			return nil
		}
		lastErr = &httpError{status: resp.StatusCode, body: string(data), retryAfter: resp.Header.Get("Retry-After")}
		if !retryable(resp.StatusCode) {
			return lastErr
		}
	}
	return fmt.Errorf("giving up after %d attempts: %w", j.maxRetries+1, lastErr)
}

type httpError struct {
	status     int
	body       string
	retryAfter string
}

func (e *httpError) Error() string {
	body := e.body
	if len(body) > 300 {
		body = body[:300] + "…"
	}
	return fmt.Sprintf("HTTP %d: %s", e.status, body)
}

func shortErr(err error) string {
	if he, ok := err.(*httpError); ok {
		switch he.status {
		case http.StatusTooManyRequests:
			return "rate limited (429)"
		case 529:
			return "Jev overloaded (529)"
		}
		return fmt.Sprintf("HTTP %d", he.status)
	}
	return "network error"
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

func (j *Jev) retryDelay(attempt int, lastErr error) time.Duration {
	if he, ok := lastErr.(*httpError); ok && he.retryAfter != "" {
		if s, err := strconv.Atoi(he.retryAfter); err == nil {
			return time.Duration(s) * time.Second
		}
	}
	return j.backoff << (attempt - 1)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func optString(o map[string]any, key, def string) string {
	if v, ok := o[key]; ok {
		return fmt.Sprint(v)
	}
	return def
}

func optFloat(o map[string]any, key string, def float64) float64 {
	switch v := o[key].(type) {
	case int:
		return float64(v)
	case float64:
		return v
	}
	return def
}
