package edgeworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Errors the API client distinguishes.
var (
	// ErrUnauthorized is a token the server does not accept: unknown, rotated
	// or the worker is disabled.
	ErrUnauthorized = errors.New("the server refused the worker token")
	// ErrJobGone is a result for a job that is no longer this worker's.
	ErrJobGone = errors.New("the job is no longer claimed by this worker")
)

// Job is a scrape job.
type Job struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// Result is what the worker read, as POST /v1/edge/jobs/{id}/result takes it.
type Result struct {
	Markdown   string         `json:"markdown,omitempty"`
	RawHTML    string         `json:"raw_html,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	StatusCode int            `json:"status_code,omitempty"`
	Error      string         `json:"error,omitempty"`
}

// Client talks to the zapply API as an edge worker.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// claimTimeout covers the server's 25 second long poll with room to spare.
const claimTimeout = 40 * time.Second

// NewClient returns a client for the API at baseURL.
func NewClient(baseURL, token string) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{Timeout: claimTimeout}}
}

// Claim waits for a job; it returns nil when there is none within the server's
// wait.
func (c *Client) Claim(ctx context.Context) (*Job, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/edge/jobs/claim", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
		var j Job
		if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&j); err != nil || j.ID == "" || j.URL == "" {
			return nil, fmt.Errorf("claim: undecodable job: %v", err)
		}
		return &j, nil
	case http.StatusNoContent:
		return nil, nil
	case http.StatusUnauthorized:
		return nil, ErrUnauthorized
	}
	return nil, statusError("claim", res)
}

// PostResult sends the outcome of a job.
func (c *Client) PostResult(ctx context.Context, id string, r Result) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/edge/jobs/"+id+"/result", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusNotFound:
		return ErrJobGone
	}
	return statusError("result", res)
}

func statusError(what string, res *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(res.Body, 300))
	return fmt.Errorf("%s: status %d: %s", what, res.StatusCode, strings.TrimSpace(string(b)))
}
