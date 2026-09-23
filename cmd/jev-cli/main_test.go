package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// mock answers like the TypeSafe API: noul is 0.9 when the state contains
// "urgent" and 0.1 otherwise; choice picks the last option; score is 1.5.
type mock struct {
	mu     sync.Mutex
	bodies []string
	calls  int
}

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.bodies = append(m.bodies, string(b))
	m.calls++
	calls := m.calls
	m.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer test-key" {
		http.Error(w, `{"error":"unauthorized"}`, 401)
		return
	}
	var req struct {
		State     any                        `json:"state"`
		Questions map[string]json.RawMessage `json:"questions"`
	}
	json.Unmarshal(b, &req)
	state, _ := json.Marshal(req.State)
	if strings.Contains(string(state), "flaky") && calls%2 == 1 {
		w.WriteHeader(429)
		return
	}
	answers := map[string]any{}
	for k, raw := range req.Questions {
		var q struct {
			Type     string          `json:"type"`
			Criteria json.RawMessage `json:"criteria"`
		}
		json.Unmarshal(raw, &q)
		switch q.Type {
		case "noul":
			p := 0.1
			if strings.Contains(string(state), "urgent") {
				p = 0.9
			}
			answers[k] = map[string]any{"type": "noul", "noul": p}
		case "choice":
			// last key in criteria order
			dec := json.NewDecoder(bytes.NewReader(q.Criteria))
			dec.Token()
			var last string
			for dec.More() {
				t, _ := dec.Token()
				last = t.(string)
				var skip json.RawMessage
				dec.Decode(&skip)
			}
			answers[k] = map[string]any{"type": "choice", "choice": last, "probabilities": map[string]float64{last: 1}, "confidence": 0.8}
		case "score":
			answers[k] = map[string]any{"type": "score", "score": 1.5, "confidence": 0.7}
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"model": "jev-test", "answers": answers,
		"usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})
}

func (m *mock) last() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	var v map[string]any
	json.Unmarshal([]byte(m.bodies[len(m.bodies)-1]), &v)
	return v
}

func setup(t *testing.T) *mock {
	m := &mock{}
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	t.Setenv("JEV_API_URL", srv.URL) // bare host is expanded to /v1/systemone
	t.Setenv("TYPESAFE_API_KEY", "test-key")
	t.Setenv("JEV_MODEL", "")
	return m
}

func runJev(stdin string, args ...string) (string, string, int) {
	var out, errb bytes.Buffer
	code := run(context.Background(), args, strings.NewReader(stdin), &out, &errb)
	return out.String(), errb.String(), code
}

func TestCommands(t *testing.T) {
	m := setup(t)
	tests := []struct {
		stdin string
		args  []string
		out   string
		code  int
	}{
		{"urgent!\n", []string{"noul", "Is it urgent?"}, "0.9\n", 0},
		{"", []string{"noul", "-s", "calm", "Is it urgent?"}, "0.1\n", 0},
		{"urgent", []string{"noul", "-q", "Is it urgent?"}, "", 0},
		{"calm", []string{"noul", "-q", "Is it urgent?"}, "", 1},
		{"calm", []string{"noul", "-q", "-t", "0.05", "Is it urgent?"}, "", 0},
		{"x", []string{"choice", "Which?", "a", "b=desc", "c"}, "c\n", 0},
		{"x", []string{"choice", "-c", `["z","y"]`, "Which?"}, "y\n", 0},
		{"x", []string{"score", "How?", "low", "high"}, "1.5\n", 0},
		{"x", []string{"score", "How?", "-j", "low", "high"}, `{"confidence":0.7,"score":1.5,"type":"score"}` + "\n", 0},
		{"urgent a\ncalm b\n\nurgent c\n", []string{"noul", "-l", "Urgent?"}, "0.9\turgent a\n0.1\tcalm b\n0.9\turgent c\n", 0},
		{"urgent a\ncalm b\nurgent c\n", []string{"grep", "Urgent?"}, "urgent a\nurgent c\n", 0},
		{"urgent a\ncalm b\n", []string{"grep", "-v", "-p", "Urgent?"}, "0.1\tcalm b\n", 0},
		{"calm\n", []string{"grep", "Urgent?"}, "", 1},
		{"flaky", []string{"noul", "Urgent?"}, "0.1\n", 0},
		{"x", []string{"ask", `{"u":{"type":"noul","instructions":"?"}}`}, `{"u":{"noul":0.1,"type":"noul"}}` + "\n", 0},
		{"", []string{"choice", "Which?"}, "", 2},
		{"", []string{"nope"}, "", 2},
	}
	for _, tt := range tests {
		out, errs, code := runJev(tt.stdin, tt.args...)
		if out != tt.out || code != tt.code {
			t.Errorf("jev-cli %q: got %q (exit %d), want %q (exit %d); stderr: %s", tt.args, out, code, tt.out, tt.code, errs)
		}
	}

	// option order and descriptions are kept
	runJev("x", "choice", "Which?", "b=desc", "a")
	m.mu.Lock()
	body := m.bodies[len(m.bodies)-1]
	m.mu.Unlock()
	if !strings.Contains(body, `"criteria":{"b":"desc","a":null}`) {
		t.Errorf("criteria order lost: %s", body)
	}

	// structured state and instructions
	runJev(`{"from":"a","text":"hi"}`, "noul", "-J", "-I", "-true", "yes", `{"q":"spam?"}`)
	req := m.last()
	if s, _ := json.Marshal(req["state"]); string(s) != `{"from":"a","text":"hi"}` {
		t.Errorf("state = %s", s)
	}
	q := req["questions"].(map[string]any)["q"].(map[string]any)
	if s, _ := json.Marshal(q); string(s) != `{"criteria":{"false":"","true":"yes"},"instructions":{"q":"spam?"},"type":"noul"}` {
		t.Errorf("question = %s", s)
	}
	if req["model"] != "jev-latest" {
		t.Errorf("model = %v", req["model"])
	}

	// JSON lines output
	out, _, _ := runJev(`{"t":"urgent"}`+"\n", "noul", "-l", "-J", "-j", "Urgent?")
	if out != `{"state":{"t":"urgent"},"answer":{"noul":0.9,"type":"noul"}}`+"\n" {
		t.Errorf("jsonl = %q", out)
	}

	// failing lines are reported and the rest still printed
	t.Setenv("TYPESAFE_API_KEY", "wrong")
	out, errs, code := runJev("a\nb\n", "noul", "-l", "Urgent?")
	if out != "" || code != 2 || !strings.Contains(errs, "line 1: HTTP 401") {
		t.Errorf("401: out=%q code=%d stderr=%q", out, code, errs)
	}
}
