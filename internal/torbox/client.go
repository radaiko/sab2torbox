package torbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// DefaultBaseURL is the production TorBox API v1 base.
const DefaultBaseURL = "https://api.torbox.app/v1/api"

// Client talks to the TorBox Usenet API.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a Client for the production TorBox API.
func New(token string) *Client { return NewWithBaseURL(token, DefaultBaseURL) }

// NewWithBaseURL returns a Client pointed at an arbitrary base URL (for tests).
func NewWithBaseURL(token, baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// CreateRequest describes an NZB to submit. Exactly one of NZBContent or Link
// must be set. NZBName is the multipart filename and optional name override.
type CreateRequest struct {
	NZBContent []byte
	NZBName    string
	Link       string
}

// APIError is returned when TorBox responds with success=false.
type APIError struct {
	Status int
	Detail string
	// RetryAfter is the server's Retry-After hint, parsed from the response
	// header on 429 responses. Zero when absent or unparseable.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("torbox api error (http %d): %s", e.Status, e.Detail)
}

// CreateUsenetDownload submits an NZB and returns the created download.
func (c *Client) CreateUsenetDownload(ctx context.Context, req CreateRequest) (*CreateResult, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if len(req.NZBContent) > 0 {
		name := req.NZBName
		if name == "" {
			name = "upload.nzb"
		}
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			return nil, fmt.Errorf("creating multipart file: %w", err)
		}
		if _, err := fw.Write(req.NZBContent); err != nil {
			return nil, fmt.Errorf("writing nzb content: %w", err)
		}
	}
	if req.Link != "" {
		_ = mw.WriteField("link", req.Link)
	}
	if req.NZBName != "" {
		_ = mw.WriteField("name", req.NZBName)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("closing multipart writer: %w", err)
	}

	env, err := c.do(ctx, http.MethodPost, "/usenet/createusenetdownload",
		mw.FormDataContentType(), &body)
	if err != nil {
		return nil, err
	}
	var res CreateResult
	if err := json.Unmarshal(env.Data, &res); err != nil {
		return nil, fmt.Errorf("decoding create result: %w", err)
	}
	return &res, nil
}

// ListUsenet returns every Usenet download on the account.
func (c *Client) ListUsenet(ctx context.Context) ([]UsenetDownload, error) {
	env, err := c.do(ctx, http.MethodGet, "/usenet/mylist?bypass_cache=true", "", nil)
	if err != nil {
		return nil, err
	}
	var list []UsenetDownload
	if len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, &list); err != nil {
			return nil, fmt.Errorf("decoding usenet list: %w", err)
		}
	}
	return list, nil
}

// ControlUsenet performs an operation (delete, pause, resume, reannounce) on a
// Usenet download.
func (c *Client) ControlUsenet(ctx context.Context, id int64, op string) error {
	payload, err := json.Marshal(map[string]any{"usenet_id": id, "operation": op})
	if err != nil {
		return fmt.Errorf("encoding control request: %w", err)
	}
	_, err = c.do(ctx, http.MethodPost, "/usenet/controlusenetdownload",
		"application/json", bytes.NewReader(payload))
	return err
}

// Ping performs a cheap reachability + token-validity check.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, "/usenet/mylist?limit=1", "", nil)
	return err
}

// do executes one request, decodes the envelope, and surfaces API errors.
func (c *Client) do(ctx context.Context, method, path, contentType string, body io.Reader) (*Envelope, error) {
	u := c.baseURL + path
	if _, err := url.Parse(u); err != nil {
		return nil, fmt.Errorf("bad url %q: %w", u, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
	var env Envelope
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, &APIError{Status: resp.StatusCode, RetryAfter: retryAfter,
				Detail: "non-JSON response: " + truncate(string(raw), 200)}
		}
	}
	if resp.StatusCode >= 400 || !env.Success {
		detail := env.Detail
		if detail == "" {
			detail = "request failed with HTTP " + strconv.Itoa(resp.StatusCode)
		}
		return nil, &APIError{Status: resp.StatusCode, Detail: detail, RetryAfter: retryAfter}
	}
	return &env, nil
}

// parseRetryAfter interprets an HTTP Retry-After header value, which is either
// a non-negative number of seconds or an HTTP date. It returns 0 when the
// header is absent or cannot be parsed.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// RateLimit reports whether err is a 429 rate-limit response. retryAfter is
// the server's Retry-After hint, or 0 when it gave none.
func RateLimit(err error) (retryAfter time.Duration, ok bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == 429 {
		return apiErr.RetryAfter, true
	}
	return 0, false
}

// Retryable reports whether err is a transient failure worth retrying.
func Retryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == 0 || apiErr.Status == 429 || apiErr.Status >= 500
	}
	return true // network/transport errors are retryable
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
