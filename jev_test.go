package jev

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"localhost:8080":                       "http://localhost:8080/v1/systemone",
		"http://localhost:8080/":               "http://localhost:8080/v1/systemone",
		"https://example.com":                  "https://example.com/v1/systemone",
		"https://example.com/other":            "https://example.com/other",
		"example.com/other":                    "http://example.com/other",
		"https://api.typesafe.ai/v1/systemone": "https://api.typesafe.ai/v1/systemone",
	} {
		if got := Endpoint(in); got != want {
			t.Errorf("Endpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDefaults(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "from-env")
	t.Setenv("JEV_MODEL", "from-env")
	c := NewClient()
	if c.Model() != DefaultModel || c.URL() != DefaultURL || c.apiKey != "" {
		t.Errorf("environment leaked into the client: %+v", c)
	}
}

func TestClient(t *testing.T) {
	var auth, model string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		auth = r.Header.Get("Authorization")
		var req struct {
			Model     string                     `json:"model"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		model = req.Model
		switch {
		case calls == 1:
			w.WriteHeader(429)
		case string(req.Questions["q"]) == `{"type":"noul","instructions":"bad"}`:
			http.Error(w, `{"error":"bad"}`, 422)
		default:
			w.Write([]byte(`{"model":"m","answers":{"q":{"type":"choice","choice":"b","probabilities":{"b":0.9,"a":0.1},"confidence":0.8}},"usage":{"input_tokens":3,"output_tokens":4}}`))
		}
	}))
	defer srv.Close()

	c := NewClient(WithURL(srv.URL), WithAPIKey("k"), WithModel("jev-x"),
		WithHTTPClient(srv.Client()), WithTimeout(5*time.Second), WithMaxRetries(1))
	a, err := c.Ask(context.Background(), "state", Question{Type: "choice", Instructions: "?",
		Criteria: Options{{Name: "b"}, {Name: "a", Desc: "desc"}}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || auth != "Bearer k" || model != "jev-x" {
		t.Errorf("calls=%d auth=%q model=%q", calls, auth, model)
	}
	if a.Choice != "b" || a.Confidence != 0.8 || a.Probabilities["a"] != 0.1 || len(a.Raw) == 0 {
		t.Errorf("answer = %+v", a)
	}

	_, err = c.Ask(context.Background(), "state", Question{Type: "noul", Instructions: "bad"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 422 {
		t.Errorf("err = %v", err)
	}

	// no key, no Authorization header
	calls = 1
	if _, err := NewClient(WithURL(srv.URL)).Ask(context.Background(), "s", Question{Type: "noul", Instructions: "?"}); err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		t.Errorf("Authorization = %q", auth)
	}
}

func TestOptionsOrder(t *testing.T) {
	b, err := json.Marshal(Options{{Name: "z", Desc: "last"}, {Name: "a"}})
	if err != nil || string(b) != `{"z":"last","a":null}` {
		t.Errorf("Options = %s, %v", b, err)
	}
}
