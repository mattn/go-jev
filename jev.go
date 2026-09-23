// Package jev is a small client for TypeSafe's Jev (System One) API, a
// decision-only model that returns typed answers (yes/no probability, choice,
// score) instead of text.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultModel   = "jev-latest"
	DefaultURL     = "https://api.typesafe.ai/v1/systemone"
	DefaultTimeout = 60 * time.Second
	Version        = "0.0.1"
)

// Client calls the Jev API. The zero value is not usable; use NewClient.
type Client struct {
	APIKey     string // no Authorization header is sent when empty
	Model      string
	URL        string
	HTTPClient *http.Client
	MaxRetries int // retries on 429 / 529
}

// NewClient returns a client configured from TYPESAFE_API_KEY, JEV_MODEL,
// JEV_API_URL and JEV_TIMEOUT (seconds).
func NewClient() *Client {
	c := &Client{
		APIKey:     os.Getenv("TYPESAFE_API_KEY"),
		Model:      DefaultModel,
		URL:        DefaultURL,
		HTTPClient: &http.Client{Timeout: DefaultTimeout},
		MaxRetries: 3,
	}
	if v := os.Getenv("JEV_MODEL"); v != "" {
		c.Model = v
	}
	if v := os.Getenv("JEV_API_URL"); v != "" {
		c.URL = Endpoint(v)
	}
	if v, err := strconv.Atoi(os.Getenv("JEV_TIMEOUT")); err == nil && v > 0 {
		c.HTTPClient.Timeout = time.Duration(v) * time.Second
	}
	return c
}

// Endpoint normalizes a server address: "localhost:8080" or "http://host/"
// become "http://host:port/v1/systemone", so any Jev-compatible server (such
// as `tensai serve`) can be named by host alone. A URL with a path is used as
// given.
func Endpoint(url string) string {
	scheme, rest := "http://", url
	if i := strings.Index(url, "://"); i >= 0 {
		scheme, rest = url[:i+3], url[i+3:]
	}
	if i := strings.IndexByte(rest, '/'); i < 0 || rest[i:] == "/" {
		return scheme + strings.TrimSuffix(rest, "/") + "/v1/systemone"
	}
	return scheme + rest
}

// Question is one typed question. Instructions and Criteria may be any value
// that marshals to JSON; json.RawMessage is sent verbatim.
type Question struct {
	Type         string `json:"type"` // "noul", "choice" or "score"
	Instructions any    `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Option is one choice option. A nil Desc is sent as null.
type Option struct {
	Name string
	Desc any
}

// Options marshals as a JSON object that keeps the options in order.
type Options []Option

func (o Options) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, opt := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(opt.Name)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(opt.Desc)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Answer is one typed answer. Raw holds the answer as returned by the server.
type Answer struct {
	Type          string             `json:"type"`
	Noul          float64            `json:"noul"`
	Choice        string             `json:"choice"`
	Score         float64            `json:"score"`
	Legend        map[string]string  `json:"legend"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
	Raw           json.RawMessage    `json:"-"`
}

func (a *Answer) UnmarshalJSON(b []byte) error {
	type plain Answer
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*a = Answer(p)
	a.Raw = append(json.RawMessage(nil), b...)
	return nil
}

// Response is the API response. Raw holds the whole body as returned.
type Response struct {
	Model   string             `json:"model"`
	Answers map[string]*Answer `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Raw json.RawMessage `json:"-"`
}

// APIError is a non-2xx response.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	body := e.Body
	if len(body) > 512 {
		body = body[:512]
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, strings.TrimSpace(body))
}

// Evaluate sends state with a map of questions. questions is typically a
// map[string]Question or a json.RawMessage (sent verbatim, keeping key order).
func (c *Client) Evaluate(ctx context.Context, state, questions any) (*Response, error) {
	body, err := json.Marshal(struct {
		State     any    `json:"state"`
		Model     string `json:"model"`
		Questions any    `json:"questions"`
	}{state, c.Model, questions})
	if err != nil {
		return nil, err
	}
	data, err := c.post(ctx, body)
	if err != nil {
		return nil, err
	}
	var resp Response
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unexpected response: %w", err)
	}
	resp.Raw = data
	return &resp, nil
}

// Ask evaluates a single question and returns its answer.
func (c *Client) Ask(ctx context.Context, state any, q Question) (*Answer, error) {
	resp, err := c.Evaluate(ctx, state, map[string]Question{"q": q})
	if err != nil {
		return nil, err
	}
	a := resp.Answers["q"]
	if a == nil {
		return nil, fmt.Errorf("unexpected response: no answer")
	}
	return a, nil
}

func (c *Client) post(ctx context.Context, body []byte) ([]byte, error) {
	hc := c.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "go-jev/"+Version)
		if c.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.APIKey)
		}
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		switch {
		case resp.StatusCode == 429 || resp.StatusCode == 529:
			if attempt >= c.MaxRetries {
				return nil, &APIError{resp.StatusCode, string(data)}
			}
			select {
			case <-time.After((500 * time.Millisecond) << attempt):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case resp.StatusCode < 200 || resp.StatusCode >= 300:
			return nil, &APIError{resp.StatusCode, string(data)}
		default:
			return data, nil
		}
	}
}
