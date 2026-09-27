package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Andree37/hunch/internal/backend"
	"github.com/Andree37/hunch/internal/flow"
	"github.com/Andree37/hunch/internal/tmpl"
)

func llmNode(ctx context.Context, f *flow.Flow, n *flow.Node, state map[string]any, opts Options, ev *Event) error {
	a := n.Action
	name := f.BackendFor(n)
	b, ok := opts.Backends[name]
	if !ok {
		return fmt.Errorf("backend %q is not configured", name)
	}
	w, ok := b.(backend.Writer)
	if !ok {
		return fmt.Errorf("backend %q can't write text; use a chat backend (openai, anthropic, bedrock, mock)", name)
	}
	ev.Backend = name
	prompt, err := tmpl.Render(a.Prompt, state)
	if err != nil {
		return err
	}
	system, err := tmpl.Render(a.System, state)
	if err != nil {
		return err
	}
	ev.Asked = []string{prompt}
	resp, err := w.Write(ctx, backend.WriteRequest{System: system, Prompt: prompt, MaxTokens: a.MaxTokens})
	if err != nil {
		return err
	}
	ev.CostUSD = resp.CostUSD
	ev.Output = map[string]any{"text": strings.TrimSpace(resp.Text)}
	return nil
}

// httpNode calls an API. Refs in the URL are escaped for where they sit;
// $VARS in the URL and headers come from the environment (so secrets stay
// out of state and out of every prompt). In a dry run, GETs still happen
// (they only read, and later nodes need the data) while anything that
// writes is recorded instead of sent.
func httpNode(ctx context.Context, n *flow.Node, state map[string]any, opts Options, ev *Event) error {
	a := n.Action
	rawURL, err := tmpl.RenderFunc(a.URL, state, func(v string, at int) string {
		switch {
		case at == 0:
			return v // the ref is the whole base URL
		case strings.Contains(a.URL[:at], "?"):
			return url.QueryEscape(v)
		}
		return url.PathEscape(v)
	})
	if err != nil {
		return err
	}
	rawURL = os.ExpandEnv(rawURL)

	body, contentType, err := renderBody(a.Body, state)
	if err != nil {
		return err
	}
	if opts.DryRun && a.Method != http.MethodGet {
		out := map[string]any{"dry_run": true, "method": a.Method, "url": rawURL}
		if body != nil {
			out["body"] = string(body)
		}
		ev.Output = out
		return nil
	}

	timeout := 30 * time.Second
	if a.Timeout > 0 {
		timeout = time.Duration(a.Timeout * float64(time.Second))
	}
	headers := http.Header{}
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	for _, h := range a.Headers {
		v, err := tmpl.Render(h.Value, state)
		if err != nil {
			return err
		}
		headers.Set(h.Key, os.ExpandEnv(v))
	}
	if a.IdempotencyKey != "" {
		key, err := tmpl.Render(a.IdempotencyKey, state)
		if err != nil {
			return err
		}
		headers.Set("Idempotency-Key", key)
	}
	retries := 2
	if a.Retries != nil {
		retries = *a.Retries
	}
	// Repeating a request is only safe when it can't be applied twice.
	safe := a.Method == http.MethodGet || a.Method == http.MethodPut || a.Method == http.MethodDelete || a.IdempotencyKey != ""

	var status int
	var data []byte
	for attempt := 0; ; attempt++ {
		status, data, err = send(ctx, a.Method, rawURL, headers, body, timeout)
		if attempt >= retries || !retryable(err, status, safe) {
			break
		}
		wait := retryWait << attempt
		backend.ReportStatus(ctx, "%s %s: %s, retrying in %s (attempt %d/%d)",
			a.Method, n.ID, failure(err, status), wait, attempt+2, retries+1)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}
	out := map[string]any{"status": status, "body": string(data)}
	var parsed any
	if json.Unmarshal(data, &parsed) == nil {
		out["body"] = parsed
	}
	ev.Output = out
	if status >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %.200s", a.Method, rawURL, status, data)
	}
	return nil
}

// retryWait is the first retry's delay; each later one doubles it.
var retryWait = 500 * time.Millisecond

func send(ctx context.Context, method, url string, headers http.Header, body []byte, timeout time.Duration) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header = headers.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, data, err
}

// retryable: 429 and 503 mean the request wasn't processed, so any method
// may repeat it. Other 5xx and network errors may have been applied, so only
// requests that are safe to repeat retry on those.
func retryable(err error, status int, safe bool) bool {
	switch {
	case err != nil:
		return safe && !errors.Is(err, context.Canceled)
	case status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable:
		return true
	case status >= 500:
		return safe
	}
	return false
}

func failure(err error, status int) string {
	if err != nil {
		return "network error"
	}
	return fmt.Sprintf("HTTP %d", status)
}

// renderBody renders a template string as text, or a map/list (whose
// strings are templates) as JSON.
func renderBody(body any, state map[string]any) ([]byte, string, error) {
	if body == nil {
		return nil, "", nil
	}
	if s, ok := body.(string); ok {
		out, err := tmpl.Render(s, state)
		return []byte(out), "text/plain; charset=utf-8", err
	}
	rendered, err := renderValue(body, state)
	if err != nil {
		return nil, "", err
	}
	data, err := json.Marshal(rendered)
	return data, "application/json", err
}

func renderValue(v any, state map[string]any) (any, error) {
	switch x := v.(type) {
	case string:
		return renderTyped(x, state)
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			r, err := renderValue(e, state)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			r, err := renderValue(e, state)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

// renderTyped renders a template; a template that is exactly one ref keeps
// the value's type (a number stays a number, a bool a bool).
func renderTyped(s string, state map[string]any) (any, error) {
	if refs := tmpl.Refs(s); len(refs) == 1 && strings.TrimSpace(s) == "{{"+refs[0].Path+"}}" {
		if v, ok := tmpl.Lookup(state, refs[0].Path); ok {
			return v, nil
		}
	}
	return tmpl.Render(s, state)
}

// outputNode records named results of the flow.
func outputNode(n *flow.Node, state map[string]any, ev *Event) error {
	out := map[string]any{}
	for _, kv := range n.Action.Set {
		if kv.Literal != nil {
			out[kv.Key] = kv.Literal
			continue
		}
		v, err := renderTyped(kv.Value, state)
		if err != nil {
			return err
		}
		out[kv.Key] = v
	}
	ev.Output = out
	return nil
}
