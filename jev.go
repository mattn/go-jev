// Package jev is a Go SDK for TypeSafe's Jev (System One) API, a
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
	"strings"
	"time"
)

const (
	DefaultModel   = "jev-latest"
	DefaultURL     = "https://api.typesafe.ai/v1/systemone"
	DefaultTimeout = 60 * time.Second
	Version        = "0.0.2"
)

// Client calls the Jev API. Create one with NewClient.
type Client struct {
	apiKey     string
	model      string
	url        string
	httpClient *http.Client
	timeout    time.Duration
	maxRetries int
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithAPIKey sets the API key. Without one no Authorization header is sent,
// which suits open servers such as `tensai serve`.
func WithAPIKey(key string) ClientOption { return func(c *Client) { c.apiKey = key } }

// WithModel sets the model (default DefaultModel).
func WithModel(model string) ClientOption { return func(c *Client) { c.model = model } }

// WithURL sets the endpoint URL as given (default DefaultURL). Use Endpoint to
// expand a bare address such as "localhost:8080".
func WithURL(url string) ClientOption { return func(c *Client) { c.url = url } }

// WithHTTPClient sets the HTTP client (default http.DefaultClient).
func WithHTTPClient(hc *http.Client) ClientOption { return func(c *Client) { c.httpClient = hc } }

// WithTimeout limits each request, including retries (default DefaultTimeout;
// 0 means no limit).
func WithTimeout(d time.Duration) ClientOption { return func(c *Client) { c.timeout = d } }

// WithMaxRetries sets how many times 429 / 529 responses are retried with
// exponential backoff (default 3).
func WithMaxRetries(n int) ClientOption { return func(c *Client) { c.maxRetries = n } }

// NewClient returns a client for the TypeSafe API, configured by opts.
func NewClient(opts ...ClientOption) *Client {
	c := &Client{
		model:      DefaultModel,
		url:        DefaultURL,
		httpClient: http.DefaultClient,
		timeout:    DefaultTimeout,
		maxRetries: 3,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Model returns the model the client sends.
func (c *Client) Model() string { return c.model }

// URL returns the endpoint the client posts to.
func (c *Client) URL() string { return c.url }

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
	}{state, c.model, questions})
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
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "go-jev/"+Version)
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		resp, err := c.httpClient.Do(req)
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
			if attempt >= c.maxRetries {
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
