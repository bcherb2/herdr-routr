package jev

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Shape of a real response captured from the decisions endpoint.
const realResponse = `{"model":"typesafe/jev-1.13-20260917","answers":{"harness":{"type":"choice","choice":"claude_code","probabilities":{"claude_code":1,"gemini_cli":0,"codex_cli":0},"confidence":1}},"usage":{"input_tokens":530,"output_tokens":86,"cost":0.00002226},"id":"gen-dec-1","provider":"TypeSafe"}`

var twoOptions = map[string]Choice{"harness": {Instructions: "which?", Options: map[string]string{"claude_code": "a", "codex_cli": "b"}}}

func TestDecideSendsChoiceQuestionsAndParses(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(realResponse))
	}))
	defer srv.Close()

	d, err := (&Client{APIKey: "k", URL: srv.URL}).Decide(context.Background(), map[string]string{"user_prompt": "x"}, twoOptions)
	if err != nil {
		t.Fatal(err)
	}
	if got["model"] != DefaultModel || got["state"].(map[string]any)["user_prompt"] != "x" {
		t.Errorf("request = %v", got)
	}
	q := got["questions"].(map[string]any)["harness"].(map[string]any)
	if q["type"] != "choice" || q["criteria"].(map[string]any)["codex_cli"] != "b" {
		t.Errorf("question = %v", q)
	}
	a := d.Answers["harness"]
	if a.Choice != "claude_code" || a.Confidence != 1 || d.Usage.InputTokens != 530 {
		t.Errorf("decision = %+v", d)
	}
}

func TestDecideRetriesOverloaded(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(529)
			return
		}
		w.Write([]byte(realResponse))
	}))
	defer srv.Close()
	d, err := (&Client{APIKey: "k", URL: srv.URL, Backoff: time.Millisecond}).Decide(context.Background(), nil, twoOptions)
	if err != nil || d.Answers["harness"].Choice != "claude_code" || n.Load() != 3 {
		t.Fatalf("d=%+v err=%v attempts=%d", d, err, n.Load())
	}
}

func TestDecideDoesNotRetryClientErrors(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"message":"expected array"}}`))
	}))
	defer srv.Close()
	_, err := (&Client{APIKey: "k", URL: srv.URL, Backoff: time.Millisecond}).Decide(context.Background(), nil, twoOptions)
	var je *Error
	if !errors.As(err, &je) || je.Status != 400 || n.Load() != 1 {
		t.Fatalf("err=%v attempts=%d", err, n.Load())
	}
}

func TestDecideGivesUpAfterRetries(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(503)
	}))
	defer srv.Close()
	_, err := (&Client{APIKey: "k", URL: srv.URL, Retries: 2, Backoff: time.Millisecond}).Decide(context.Background(), nil, twoOptions)
	if err == nil || n.Load() != 3 {
		t.Fatalf("err=%v attempts=%d", err, n.Load())
	}
}

func TestDecideValidatesInput(t *testing.T) {
	if _, err := (&Client{}).Decide(context.Background(), nil, twoOptions); err == nil {
		t.Error("want missing-key error")
	}
	one := map[string]Choice{"h": {Options: map[string]string{"only": "x"}}}
	if _, err := (&Client{APIKey: "k"}).Decide(context.Background(), nil, one); err == nil {
		t.Error("want single-option error")
	}
}
