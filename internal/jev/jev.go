// Package jev calls TypeSafe's Jev decision model through OpenRouter's
// decisions endpoint (not chat/completions, which Jev does not serve).
//
// Jev scores caller-defined options against a JSON state and returns a choice
// with probabilities and confidence per question. It produces no prose.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	DefaultURL   = "https://openrouter.ai/api/alpha/decisions"
	DefaultModel = "typesafe/jev-1.13"
)

// Choice is a "choice" question: pick one of Options (id → description).
type Choice struct {
	Instructions string
	Options      map[string]string
}

// Answer is Jev's verdict on one choice question.
type Answer struct {
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

// Decision is a full response.
type Decision struct {
	ID      string            `json:"id"`
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   struct {
		InputTokens int     `json:"input_tokens"`
		Cost        float64 `json:"cost"`
	} `json:"usage"`
}

// Client calls the decisions endpoint.
type Client struct {
	APIKey  string
	URL     string        // default DefaultURL
	Model   string        // default DefaultModel
	HTTP    *http.Client  // default: 30s timeout
	Retries int           // extra attempts on 429/5xx; default 3
	Backoff time.Duration // first retry delay, doubled each time; default 500ms
}

// Error is a non-2xx response.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string { return fmt.Sprintf("jev: HTTP %d: %s", e.Status, e.Body) }

func retryable(status int) bool {
	// The alpha endpoint answers 529 (overloaded) at times.
	return status == http.StatusTooManyRequests || status >= 500
}

// Decide asks every question against state in one call.
func (c *Client) Decide(ctx context.Context, state any, questions map[string]Choice) (Decision, error) {
	if c.APIKey == "" {
		return Decision{}, fmt.Errorf("jev: no API key (set OPENROUTER_API_KEY)")
	}
	qs := map[string]any{}
	for id, q := range questions {
		if len(q.Options) < 2 {
			return Decision{}, fmt.Errorf("jev: question %q needs at least two options", id)
		}
		qs[id] = map[string]any{"type": "choice", "instructions": q.Instructions, "criteria": q.Options}
	}
	body, err := json.Marshal(map[string]any{"model": or(c.Model, DefaultModel), "state": state, "questions": qs})
	if err != nil {
		return Decision{}, fmt.Errorf("jev: encode request: %w", err)
	}

	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	retries, backoff := c.Retries, c.Backoff
	if retries == 0 {
		retries = 3
	}
	if backoff == 0 {
		backoff = 500 * time.Millisecond
	}

	for attempt := 0; ; attempt++ {
		d, err := c.post(ctx, hc, body)
		var he *Error
		if err == nil || attempt >= retries || !(errors.As(err, &he) && retryable(he.Status)) {
			return d, err
		}
		select {
		case <-ctx.Done():
			return Decision{}, ctx.Err()
		case <-time.After(backoff << attempt):
		}
	}
}

func (c *Client) post(ctx context.Context, hc *http.Client, body []byte) (Decision, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, or(c.URL, DefaultURL), bytes.NewReader(body))
	if err != nil {
		return Decision{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return Decision{}, fmt.Errorf("jev: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Decision{}, fmt.Errorf("jev: read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return Decision{}, &Error{Status: resp.StatusCode, Body: string(b)}
	}
	var d Decision
	if err := json.Unmarshal(b, &d); err != nil {
		return Decision{}, fmt.Errorf("jev: decode response: %w", err)
	}
	return d, nil
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
