// Package runners talks to the trading-bot services ("runners") over the HTTP API described in
// docs/runner_api.md: typed client, a registry that polls every runner, and the alert rules.
package runners

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const requestTimeout = 10 * time.Second

// Client drives one runner.
type Client struct {
	Name  string
	URL   string // without a trailing slash
	token string
	hc    *http.Client
}

func NewClient(name, baseURL, token string) *Client {
	return &Client{Name: name, URL: strings.TrimRight(baseURL, "/"), token: token, hc: &http.Client{}}
}

// HTTPError is a non-2xx answer from a runner; Msg is the runner's {"error": ...} text.
type HTTPError struct {
	Status int
	Msg    string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Msg) }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-Control-Token", c.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(raw))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		} else if len(msg) > 200 {
			msg = msg[:200]
		}
		return &HTTPError{Status: resp.StatusCode, Msg: msg}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func sPath(id, tail string) string { return "/v1/strategies/" + url.PathEscape(id) + tail }

// Health calls GET /v1/health.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	err := c.do(ctx, http.MethodGet, "/v1/health", nil, &h)
	return h, err
}

func (c *Client) Strategies(ctx context.Context) ([]Strategy, error) {
	var s []Strategy
	err := c.do(ctx, http.MethodGet, "/v1/strategies", nil, &s)
	return s, err
}

// Pause stops new positions; by is the Telegram user id.
func (c *Client) Pause(ctx context.Context, id string, by int64) (Strategy, error) {
	return c.setDesired(ctx, id, "pause", by)
}

func (c *Client) Resume(ctx context.Context, id string, by int64) (Strategy, error) {
	return c.setDesired(ctx, id, "resume", by)
}

func (c *Client) setDesired(ctx context.Context, id, action string, by int64) (Strategy, error) {
	var r struct {
		Strategy Strategy `json:"strategy"`
	}
	err := c.do(ctx, http.MethodPost, sPath(id, "/"+action), map[string]any{"by": by, "reason": "tg"}, &r)
	return r.Strategy, err
}

// Run starts the daily cycle now (a 409 "busy" comes back as an *HTTPError).
func (c *Client) Run(ctx context.Context, id string) (RunResult, error) {
	var r RunResult
	err := c.do(ctx, http.MethodPost, sPath(id, "/run"), map[string]any{}, &r)
	return r, err
}

func (c *Client) Plan(ctx context.Context, id string) (Plan, error) {
	var p Plan
	err := c.do(ctx, http.MethodGet, sPath(id, "/plan"), nil, &p)
	return p, err
}

func (c *Client) Positions(ctx context.Context, id string) ([]Position, error) {
	var p []Position
	err := c.do(ctx, http.MethodGet, sPath(id, "/positions"), nil, &p)
	return p, err
}

func (c *Client) Report(ctx context.Context, id string) (Report, error) {
	var r Report
	err := c.do(ctx, http.MethodGet, sPath(id, "/report"), nil, &r)
	return r, err
}

// Events returns events with seq > since (oldest first); since < 0 means "the last n events".
// n <= 0 uses the runner's default.
func (c *Client) Events(ctx context.Context, id string, since int64, n int) ([]Event, error) {
	q := url.Values{}
	if since >= 0 {
		q.Set("since", strconv.FormatInt(since, 10))
	}
	if n > 0 {
		q.Set("n", strconv.Itoa(n))
	}
	path := sPath(id, "/events")
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var ev []Event
	err := c.do(ctx, http.MethodGet, path, nil, &ev)
	return ev, err
}
