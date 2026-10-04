// Package client is a small, hand-written REST client for AutoPilot's admin
// API (/v1/admin). It keeps the API's cross-cutting rules in one place so
// every resource behaves the same way:
//
//   - a bearer admin token, JSON in and out;
//   - records keyed by name. A create is safe to send again: AutoPilot
//     answers a create for a name that already holds the same fields with
//     that record (200) instead of making another, so no Idempotency-Key is
//     sent;
//   - `If-Match: "<lock_version>"` on every change and delete;
//   - the `{"error", "code", "message", "field"}` refusal, surfaced as *Error
//     so callers branch on the code, never on the prose;
//   - client-side backoff on 429 (honouring Retry-After), on 503 and the other
//     gateway answers, and on dropped connections. Every request this API
//     takes is safe to send twice (see request.replayable), so each of them
//     is retried.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// APIPath is the base path every admin route hangs off.
const APIPath = "/v1/admin"

// DefaultUserAgent identifies the provider. AutoPilot records the User-Agent
// of every admin write in its ledger.
const DefaultUserAgent = "terraform-provider-autopilot"

// Defaults for the retry policy: generous enough to ride out a deploy or a
// short outage without failing the apply, and bounded so a misbehaving server
// cannot hang a plan forever.
const (
	DefaultMaxRetries     = 6
	DefaultInitialBackoff = 500 * time.Millisecond
	DefaultMaxBackoff     = 30 * time.Second
	DefaultTimeout        = 60 * time.Second
)

// maxErrorBody bounds how much of a non-JSON error body is kept in Error.Message.
const maxErrorBody = 512

// Client talks to one AutoPilot as its admin token.
type Client struct {
	baseURL        *url.URL
	token          string
	httpClient     *http.Client
	userAgent      string
	maxRetries     int
	initialBackoff time.Duration
	maxBackoff     time.Duration

	// sleep is swapped out by tests so retry timing is deterministic.
	sleep func(context.Context, time.Duration) error
}

// Option customises a Client at construction.
type Option func(*Client)

// WithHTTPClient replaces the underlying *http.Client (timeouts, TLS, proxies).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.httpClient = h }
}

// WithUserAgent overrides the User-Agent header.
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.userAgent = ua }
}

// WithMaxRetries bounds how many times a retryable failure is retried.
func WithMaxRetries(n int) Option {
	return func(c *Client) { c.maxRetries = n }
}

// WithBackoff sets the exponential backoff bounds used when the server does
// not supply a Retry-After.
func WithBackoff(initial, maxBackoff time.Duration) Option {
	return func(c *Client) {
		c.initialBackoff = initial
		c.maxBackoff = maxBackoff
	}
}

// ValidateEndpoint reports what is wrong with an endpoint, or nil. It is the
// check New makes, exposed so the provider can make it at configure time.
func ValidateEndpoint(endpoint string) error {
	_, err := parseEndpoint(endpoint)
	return err
}

// ValidateToken reports what is wrong with an admin token, or nil.
func ValidateToken(token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("token must not be empty")
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return errors.New("token must not contain whitespace")
	}
	return nil
}

func parseEndpoint(endpoint string) (*url.URL, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, errors.New("endpoint must not be empty")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint %q: %w", endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("invalid endpoint %q: scheme must be http or https", endpoint)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid endpoint %q: missing host", endpoint)
	}
	if u.User != nil {
		return nil, fmt.Errorf("invalid endpoint %q: credentials belong in the token, not the URL", u.Redacted())
	}
	u.RawQuery = ""
	u.Fragment = ""
	// Tolerate an endpoint that already carries /v1/admin (or a trailing
	// slash), so a URL copied out of the API's docs works either way.
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), APIPath) + APIPath
	return u, nil
}

// New builds a client for the AutoPilot at endpoint (scheme and host,
// optionally a path prefix; /v1/admin is appended here) using the given admin
// token.
func New(endpoint, token string, opts ...Option) (*Client, error) {
	u, err := parseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	// Tokens arrive from environment variables and secret stores, which often
	// carry a trailing newline. The API would refuse it verbatim.
	token = strings.TrimSpace(token)
	if err := ValidateToken(token); err != nil {
		return nil, err
	}

	c := &Client{
		baseURL:        u,
		token:          token,
		httpClient:     &http.Client{Timeout: DefaultTimeout},
		userAgent:      DefaultUserAgent,
		maxRetries:     DefaultMaxRetries,
		initialBackoff: DefaultInitialBackoff,
		maxBackoff:     DefaultMaxBackoff,
		sleep:          sleepCtx,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// BaseURL returns the resolved API base (endpoint + /v1/admin).
func (c *Client) BaseURL() string { return c.baseURL.String() }

// Fields is a write body: only the keys present are sent. On a PATCH an
// omitted key leaves that field as it is.
type Fields map[string]any

// RequestOption tunes a single request.
type RequestOption func(*request)

type request struct {
	ifMatch *int64
	// replayable marks a request the server can safely see twice. Only those
	// are retried after a dropped connection or a gateway 5xx, where the
	// first attempt may have been processed. For this API that is every
	// request: GET and DELETE always, POST because a create is keyed by name
	// and an identical one is answered with the record, and PATCH because it
	// always carries If-Match (the API refuses one without), so a second copy
	// of an applied change meets stale_object instead of applying twice.
	replayable bool
	// trace, when set, is told how the request's attempts went (see sendTrace).
	trace *sendTrace
}

// sendTrace reports how a request's attempts went, for a caller that has to
// act on a SUCCESSFUL answer differently depending on them. A failed request
// carries the same facts on its *Error.
type sendTrace struct {
	// status is the status code of the answer that ended the request.
	status int
	// earlierSendUnanswered: an attempt before the one that answered may have
	// reached the server and got no usable response (Error.EarlierSendUnanswered).
	earlierSendUnanswered bool
}

// withSendTrace asks do to fill t once the request is answered.
func withSendTrace(t *sendTrace) RequestOption {
	return func(r *request) { r.trace = t }
}

// WithIfMatch sends the record's lock_version as an If-Match precondition.
// The server answers 409 stale_object if the record moved on.
func WithIfMatch(lockVersion int64) RequestOption {
	return func(r *request) {
		v := lockVersion
		r.ifMatch = &v
	}
}

// Get performs a GET and decodes the JSON body into out (which may be nil).
func (c *Client) Get(ctx context.Context, path string, out any, opts ...RequestOption) error {
	return c.do(ctx, http.MethodGet, path, nil, out, opts...)
}

// Post performs a POST with a JSON body and decodes the response into out.
func (c *Client) Post(ctx context.Context, path string, body, out any, opts ...RequestOption) error {
	return c.do(ctx, http.MethodPost, path, body, out, opts...)
}

// Patch performs a PATCH with a JSON body and decodes the response into out.
func (c *Client) Patch(ctx context.Context, path string, body, out any, opts ...RequestOption) error {
	return c.do(ctx, http.MethodPatch, path, body, out, opts...)
}

// Delete performs a DELETE. Any 2xx is success.
func (c *Client) Delete(ctx context.Context, path string, out any, opts ...RequestOption) error {
	return c.do(ctx, http.MethodDelete, path, nil, out, opts...)
}

// shapeOf describes a JSON body for a diagnostic without reproducing its
// contents: the top-level keys of an object, or the JSON kind otherwise.
func shapeOf(raw []byte) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "empty body"
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(trimmed, &top) == nil {
		keys := make([]string, 0, len(top))
		for k := range top {
			keys = append(keys, k)
		}
		sortStrings(keys)
		return "object with keys [" + strings.Join(keys, ", ") + "]"
	}
	switch trimmed[0] {
	case '[':
		return "JSON array"
	case '"':
		return "JSON string"
	default:
		return "JSON " + string(trimmed[:min(len(trimmed), 20)])
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any, opts ...RequestOption) error {
	req := &request{}
	for _, opt := range opts {
		opt(req)
	}
	switch method {
	case http.MethodGet, http.MethodDelete, http.MethodPost:
		req.replayable = true
	case http.MethodPatch:
		req.replayable = req.ifMatch != nil
	}

	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding %s %s body: %w", method, path, err)
		}
	}

	u := *c.baseURL
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + strings.TrimPrefix(path, "/")

	// unanswered: an attempt so far may have reached the server and got no
	// usable response. It only ever turns on, and it describes the attempts
	// BEFORE the one being handled, which is what a caller asks about.
	unanswered := false
	for attempt := 0; ; attempt++ {
		var sent attemptTrace
		resp, err := c.send(httptrace.WithClientTrace(ctx, sent.hooks()), method, u.String(), payload, req)
		// net/http retries a request by itself when a reused connection dies
		// after the request was written, and only says so through the trace:
		// two complete writes in one attempt mean the first went unanswered.
		if sent.writes.Load() > 1 {
			unanswered = true
		}
		if err != nil {
			if !req.replayable || !isTransient(err) || attempt >= c.maxRetries {
				return &Error{Method: method, Path: path, Message: err.Error(), Err: err,
					Preconditioned: req.ifMatch != nil, EarlierSendUnanswered: unanswered}
			}
			if sent.mayHaveReachedServer(err) {
				unanswered = true
			}
			if werr := c.sleep(ctx, c.backoff(attempt, 0)); werr != nil {
				return werr
			}
			continue
		}

		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return &Error{Method: method, Path: path, Status: resp.StatusCode, Message: readErr.Error(), Err: readErr,
				Preconditioned: req.ifMatch != nil, EarlierSendUnanswered: unanswered}
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if req.trace != nil {
				req.trace.status = resp.StatusCode
				req.trace.earlierSendUnanswered = unanswered
			}
			if out == nil || len(bytes.TrimSpace(respBody)) == 0 {
				return nil
			}
			if err := json.Unmarshal(respBody, out); err != nil {
				return &Error{Method: method, Path: path, Status: resp.StatusCode,
					Message: fmt.Sprintf("decoding response (%s): %s", shapeOf(respBody), err), Err: err}
			}
			return nil
		}

		apiErr := newError(method, path, resp, respBody)
		apiErr.Preconditioned = req.ifMatch != nil
		apiErr.EarlierSendUnanswered = unanswered

		retry := false
		switch {
		case apiErr.Status == http.StatusTooManyRequests:
			// The request was not processed; always safe to repeat.
			retry = true
		case isGatewayStatus(apiErr.Status):
			// 503 is also AutoPilot's own `unavailable` (no ledger, or no
			// SECRETS_KEY to seal a sender's secrets), which it asks callers to
			// retry with backoff.
			retry = req.replayable
		}
		if !retry || attempt >= c.maxRetries {
			return apiErr
		}
		// A gateway answering for the server says nothing about whether the
		// server itself got the request, so treat it as unanswered.
		if isGatewayStatus(apiErr.Status) && apiErr.Code != CodeUnavailable {
			unanswered = true
		}
		if werr := c.sleep(ctx, c.backoff(attempt, apiErr.RetryAfter)); werr != nil {
			return werr
		}
	}
}

func (c *Client) send(ctx context.Context, method, rawURL string, payload []byte, req *request) (*http.Response, error) {
	var bodyReader io.Reader
	if payload != nil {
		bodyReader = bytes.NewReader(payload)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, rawURL, bodyReader)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", c.userAgent)
	if payload != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if req.ifMatch != nil {
		httpReq.Header.Set("If-Match", strconv.Quote(strconv.FormatInt(*req.ifMatch, 10)))
	}
	return c.httpClient.Do(httpReq)
}

// backoff returns how long to wait before the next attempt. A server-supplied
// Retry-After wins outright; otherwise exponential backoff with full jitter,
// capped at maxBackoff.
func (c *Client) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > c.maxBackoff {
			return c.maxBackoff
		}
		return retryAfter
	}
	d := c.initialBackoff << uint(attempt) //nolint:gosec // attempt is bounded by maxRetries
	if d > c.maxBackoff || d <= 0 {
		d = c.maxBackoff
	}
	// Full jitter: anywhere in [d/2, d].
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1)) //nolint:gosec // jitter, not security
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// attemptTrace watches one attempt through net/http's client trace: whether
// the transport reported on it at all, and how many times it finished writing
// the request. The hooks can run on the transport's goroutines, hence atomics.
type attemptTrace struct {
	traced atomic.Bool
	writes atomic.Int32
}

func (t *attemptTrace) hooks() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(string) { t.traced.Store(true) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				t.writes.Add(1)
			}
		},
	}
}

// mayHaveReachedServer reports whether a failed attempt could have been
// received by the server. When the transport traced the attempt, that is
// exactly whether the request was written in full. A transport that reports
// nothing (a custom RoundTripper) falls back to reading the error.
//
// A caller treats "an earlier send went unanswered" as "the change the server
// now shows may be my own", so a doubtful case counts as never sent.
func (t *attemptTrace) mayHaveReachedServer(err error) bool {
	if t.traced.Load() {
		return t.writes.Load() > 0
	}
	return mayHaveBeenSent(err)
}

// mayHaveBeenSent reads a transport failure for whether it could have come
// after the server received the request: a timeout waiting for the answer, a
// reset or closed connection, a torn-down response. A failure to connect at
// all (a refused connection, a DNS failure, any error while dialling, a TLS
// handshake that timed out) means the request never left.
func mayHaveBeenSent(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return false
	}
	if strings.Contains(err.Error(), "TLS handshake timeout") {
		return false
	}
	return true
}

func isGatewayStatus(status int) bool {
	return status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

// isTransient reports whether a transport-level error is worth retrying: a
// timeout, a refused or reset connection, or a torn-down response. Context
// cancellation, TLS failures, DNS failures that are not timeouts, and malformed
// requests are permanent and are never retried.
func isTransient(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsTimeout || dnsErr.IsTemporary
	}
	return false
}
