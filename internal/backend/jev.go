package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/Andree37/hunch/internal/flow"
)

// Jev calls a decision model that speaks TypeSafe's Jev API: Jev itself
// (TypeSafe, OpenRouter, Runware...) or Convai's Laya served by laya-serve.
// All take the same request body. Options:
//
//	url:            the endpoint, e.g. https://api.typesafe.ai/v1/systemone
//	model:          sent as is; defaults to Jev's for TypeSafe and OpenRouter
//	api_key:        sent as a bearer token when set
//	price_per_mtok: USD per million input tokens, to estimate cost when the
//	                server doesn't report it (default 0.042 for Jev's hosts, else 0)
//	timeout:        seconds per request (default 30)
//	max_retries:    retries on 429, 529, 5xx and network errors (default 3)
type Jev struct {
	name        string
	url         string
	model       string
	key         string
	pricePerTok float64
	maxRetries  int
	client      *http.Client
	backoff     time.Duration
}

// jevHosts are where Jev itself is served, with its model id there.
var jevHosts = map[string]string{
	"api.typesafe.ai": "jev-latest",
	"openrouter.ai":   "typesafe/jev-1.13",
}

func NewJev(cfg flow.BackendConfig) (*Jev, error) {
	o := cfg.Options
	endpoint := optString(o, "url", "")
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("model %q: url %q is not an endpoint", cfg.Name, endpoint)
	}
	jevModel, isJev := jevHosts[u.Hostname()]
	price := 0.0
	if isJev {
		price = 0.042
	}
	key, err := apiKey(o)
	if err != nil {
		return nil, fmt.Errorf("model %q: %w", cfg.Name, err)
	}
	return &Jev{
		name:        cfg.Name,
		url:         endpoint,
		model:       optString(o, "model", jevModel),
		key:         key,
		pricePerTok: optFloat(o, "price_per_mtok", price) / 1e6,
		maxRetries:  int(optFloat(o, "max_retries", 3)),
		client:      &http.Client{Timeout: optSeconds(o, "timeout", 30)},
		backoff:     500 * time.Millisecond,
	}, nil
}

func (j *Jev) Name() string { return j.name }

func (j *Jev) Caps() Caps { return Caps{MultiQuestion: true, Calibrated: true, CostReporting: true} }

type jevRequest struct {
	Model     string                 `json:"model,omitempty"`
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
	// Laya's confidence on Jev's scale, which the thresholds are set for;
	// its own `confidence` is stricter.
	JevConfidence *float64 `json:"x_jev_confidence"`
}

func (j *Jev) Decide(ctx context.Context, state map[string]any, qs []flow.Question) (Response, error) {

	req := jevRequest{Model: j.model, State: state, Questions: map[string]jevQuestion{}}
	for _, q := range qs {
		req.Questions[q.Name] = toJevQuestion(q)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}

	var res jevResponse
	if err := j.post(ctx, j.key, body, &res); err != nil {
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
	switch {
	case a.JevConfidence != nil:
		d.Confidence = *a.JevConfidence
	case a.Confidence != nil:
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
			ReportStatus(ctx, "%s, retrying in %s (attempt %d/%d)", shortErr(lastErr), delay, attempt+1, j.maxRetries+1)
			if err := sleep(ctx, delay); err != nil {
				return err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
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

// optSeconds reads a duration given in seconds.
func optSeconds(o map[string]any, key string, def float64) time.Duration {
	return time.Duration(optFloat(o, key, def) * float64(time.Second))
}
