// Package autopilottest is an in-memory fake of AutoPilot's admin API
// (/v1/admin). It encodes the API's contract: a bearer admin token, JSON
// objects only, unknown fields refused, records keyed by name, an identical
// create answered with the record, If-Match against lock_version, the
// {error, code, message, field} refusal, and the sender and app rules. The
// provider is exercised end to end through Terraform against it, without a
// live AutoPilot. Anything the fake accepts that the real API would refuse is
// a bug in the fake.
package autopilottest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// DefaultToken is the admin token the fake accepts. It is not a secret.
const DefaultToken = "fake-admin-token-for-tests"

// maxBody is the largest body the API reads.
const maxBody = 64 << 10

// RecordedRequest is one request the fake served, kept for assertions. Body
// is the request body; Response is the body the fake answered with.
type RecordedRequest struct {
	Method   string
	Path     string
	Header   http.Header
	Body     []byte
	Status   int
	Response []byte
}

// Server is the fake. Use the accessor methods to read or change its records
// while test traffic is in flight.
type Server struct {
	*httptest.Server

	mu    sync.Mutex
	token string

	senders map[string]*senderRecord
	apps    map[string]*appRecord
	// reserved are the sender names a create is refused for.
	reserved map[string]bool
	// retiredOweWork leaves a deleted sender retired, still finishing its
	// tasks, until FinishRetiredWork. Otherwise it becomes a tombstone at once.
	retiredOweWork bool

	// Fault injection.
	throttleNext    int
	throttleRetry   time.Duration
	unavailableNext int
	beforeRequest   []requestHook
	dropResponses   []requestHook
	canned          []cannedResponse
	requests        []RecordedRequest
}

// cannedResponse is a raw answer that stands in for AutoPilot's.
type cannedResponse struct {
	method, path string
	status       int
	contentType  string
	body         string
}

// requestHook runs once, just before the first request matching method and
// path is handled: the seam for "someone else wrote in between".
type requestHook struct {
	method, path string
	fn           func()
}

// New starts a fake bound to a random local port and stops it when the test
// ends.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		token:    DefaultToken,
		senders:  map[string]*senderRecord{},
		apps:     map[string]*appRecord{},
		reserved: map[string]bool{"autopilot": true, "autopilot-mcp": true},
	}
	mux := http.NewServeMux()
	s.senderRoutes(mux)
	s.appRoutes(mux)
	// Under /v1/admin, a route that doesn't take the method answers 405 with
	// Allow, and a path with no route answers the not_found refusal. Outside
	// it, a plain 404, as a path that isn't AutoPilot's admin API would get.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/admin" && !strings.HasPrefix(r.URL.Path, "/v1/admin/") {
			http.NotFound(w, r)
			return
		}
		if allow := allowedMethods(r.URL.Path); allow != "" {
			w.Header().Set("Allow", allow)
			writeError(w, refusal{http.StatusMethodNotAllowed, "method_not_allowed", "this route doesn't take " + r.Method, ""})
			return
		}
		writeError(w, refusal{http.StatusNotFound, "not_found", "no such route", ""})
	})
	s.Server = httptest.NewServer(s.middleware(mux))
	t.Cleanup(s.Close)
	return s
}

// Token returns the admin token the fake accepts.
func (s *Server) Token() string { return s.token }

// ReserveName adds a sender name to the environment's fixed list.
func (s *Server) ReserveName(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserved[name] = true
}

// RetiredSendersOweWork makes a deleted sender keep owing work (tasks
// running, events undelivered), so it stays retired, rather than a
// tombstone, until FinishRetiredWork.
func (s *Server) RetiredSendersOweWork(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retiredOweWork = on
}

// ThrottleNext makes the next n requests answer 429 with the given Retry-After.
func (s *Server) ThrottleNext(n int, retryAfter time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.throttleNext = n
	s.throttleRetry = retryAfter
}

// UnavailableNext makes the next n requests answer 503 unavailable, as
// AutoPilot does when it cannot reach its ledger.
func (s *Server) UnavailableNext(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unavailableNext = n
}

// OnNextRequest runs fn once, immediately before the next request with the
// given method and exact path is handled (after fault injection and auth).
func (s *Server) OnNextRequest(method, path string, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beforeRequest = append(s.beforeRequest, requestHook{method: method, path: path, fn: fn})
}

// DropNextResponse makes the next request with the given method whose path
// ends in pathSuffix take effect as usual, and then lose its answer: the fake
// closes the connection instead, the way a timeout or a dropped connection
// loses an answer the server did send.
func (s *Server) DropNextResponse(method, pathSuffix string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropResponses = append(s.dropResponses, requestHook{method: method, path: pathSuffix})
}

// allowedMethods lists the methods an admin route takes, or "" for a path
// with no route.
func allowedMethods(path string) string {
	switch {
	case collectionRoute.MatchString(path):
		return "GET, POST"
	case recordRoute.MatchString(path):
		return "GET, PATCH, DELETE"
	case previousSecretsRoute.MatchString(path):
		return "DELETE"
	}
	return ""
}

var (
	collectionRoute      = regexp.MustCompile(`^/v1/admin/(senders|apps)$`)
	recordRoute          = regexp.MustCompile(`^/v1/admin/(senders|apps)/[^/]+$`)
	previousSecretsRoute = regexp.MustCompile(`^/v1/admin/senders/[^/]+/previous-secrets$`)
)

// RespondNext answers the next request with the given method whose path ends
// in pathSuffix with this raw response, without AutoPilot seeing it, as a
// proxy in front of AutoPilot, or a page that isn't AutoPilot's, would.
func (s *Server) RespondNext(method, pathSuffix string, status int, contentType, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.canned = append(s.canned, cannedResponse{method: method, path: pathSuffix, status: status, contentType: contentType, body: body})
}

// Requests returns every request served so far.
func (s *Server) Requests() []RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// RequestsMatching returns the requests with the given method whose path has
// the given prefix.
func (s *Server) RequestsMatching(method, pathPrefix string) []RecordedRequest {
	var out []RecordedRequest
	for _, r := range s.Requests() {
		if r.Method == method && strings.HasPrefix(r.Path, pathPrefix) {
			out = append(out, r)
		}
	}
	return out
}

// discardWriter is a ResponseWriter that keeps nothing, for an answer the
// fake is about to lose on purpose.
type discardWriter struct{ header http.Header }

func (d discardWriter) Header() http.Header         { return d.header }
func (d discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d discardWriter) WriteHeader(int)             {}

// statusRecorder captures the status code and body for the request log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			s.mu.Lock()
			s.requests = append(s.requests, RecordedRequest{
				Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(),
				Body: body, Status: rec.status, Response: rec.body.Bytes(),
			})
			s.mu.Unlock()
		}()

		// Fault injection runs before auth, as a throttle in front of the
		// service would.
		s.mu.Lock()
		if s.throttleNext > 0 {
			s.throttleNext--
			retry := s.throttleRetry
			s.mu.Unlock()
			if retry > 0 {
				rec.Header().Set("Retry-After", strconv.Itoa(int(retry/time.Second)))
			}
			writeError(rec, refusal{http.StatusTooManyRequests, "rate_limited", "too many requests", ""})
			return
		}
		for i, c := range s.canned {
			if c.method == r.Method && strings.HasSuffix(r.URL.Path, c.path) {
				s.canned = append(s.canned[:i], s.canned[i+1:]...)
				s.mu.Unlock()
				rec.Header().Set("Content-Type", c.contentType)
				rec.WriteHeader(c.status)
				_, _ = io.WriteString(rec, c.body)
				return
			}
		}
		if s.unavailableNext > 0 {
			s.unavailableNext--
			s.mu.Unlock()
			writeError(rec, refusal{http.StatusServiceUnavailable, "unavailable", "AutoPilot couldn't reach its ledger", ""})
			return
		}
		token := s.token
		s.mu.Unlock()

		scheme, given, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "Bearer") || given != token {
			rec.Header().Set("WWW-Authenticate", "Bearer")
			writeError(rec, refusal{http.StatusUnauthorized, "unauthorized", "a missing or wrong admin token", ""})
			return
		}
		if len(body) > maxBody {
			writeError(rec, refusal{http.StatusRequestEntityTooLarge, "too_large", "the body is over 64 KiB", ""})
			return
		}

		s.mu.Lock()
		s.endExpiredOverlaps(time.Now())
		for i, h := range s.beforeRequest {
			if h.method == r.Method && h.path == r.URL.Path {
				s.beforeRequest = append(s.beforeRequest[:i], s.beforeRequest[i+1:]...)
				s.mu.Unlock()
				h.fn()
				s.mu.Lock()
				break
			}
		}
		drop := false
		for i, h := range s.dropResponses {
			if h.method == r.Method && strings.HasSuffix(r.URL.Path, h.path) {
				s.dropResponses = append(s.dropResponses[:i], s.dropResponses[i+1:]...)
				drop = true
				break
			}
		}
		s.mu.Unlock()
		if drop {
			// Serve into a recorder the client never sees, then hang up.
			lost := &statusRecorder{ResponseWriter: discardWriter{header: http.Header{}}, status: http.StatusOK}
			next.ServeHTTP(lost, r)
			rec.status = lost.status
			rec.body.Write(lost.body.Bytes())
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		next.ServeHTTP(rec, r)
	})
}

// --- the envelope ---------------------------------------------------------

// refusal is one of the API's refusals. It never repeats a secret or the body.
type refusal struct {
	status  int
	code    string
	message string
	field   string
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, r refusal) {
	body := map[string]any{"error": r.code, "code": r.code, "message": r.message}
	if r.field != "" {
		body["field"] = r.field
	}
	writeJSON(w, r.status, body)
}

// readObject reads a request body as a JSON object.
func readObject(r *http.Request) (map[string]json.RawMessage, *refusal) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, &refusal{http.StatusBadRequest, "bad_request", "the body could not be read", ""}
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return nil, &refusal{http.StatusBadRequest, "not_json", "the body is not a JSON object", ""}
	}
	return obj, nil
}

// refuseUnknown refuses the first key (in sorted order) that is not allowed.
func refuseUnknown(obj map[string]json.RawMessage, pointer string, allowed ...string) *refusal {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if !slices.Contains(allowed, k) {
			return &refusal{http.StatusUnprocessableEntity, "invalid_attribute", "AutoPilot doesn't know this field", pointer + "/" + k}
		}
	}
	return nil
}

// ifMatch checks a change against a record's lock_version. required says
// whether a missing If-Match is refused (PATCH) or allowed (DELETE).
func ifMatch(r *http.Request, lockVersion int64, required bool) *refusal {
	h := r.Header.Get("If-Match")
	if h == "" {
		if required {
			return &refusal{http.StatusPreconditionRequired, "precondition_required", "a change needs If-Match with the lock_version you read", ""}
		}
		return nil
	}
	if v := ifMatchValue(h); v != strconv.FormatInt(lockVersion, 10) {
		return staleRefusal()
	}
	return nil
}

// changePrecondition checks a PATCH's If-Match against a record's
// lock_version. It names the current version (behind is false), or the one
// just before it (behind is true): the contract's safe repeat of a change
// whose answer was lost, which the caller answers with 200 only when the
// record already holds exactly what the PATCH sends.
func changePrecondition(r *http.Request, lockVersion int64) (behind bool, ref *refusal) {
	if ref := ifMatch(r, lockVersion, true); ref == nil {
		return false, nil
	} else if ref.code != "stale_object" {
		return false, ref
	}
	if lockVersion > 1 && ifMatchValue(r.Header.Get("If-Match")) == strconv.FormatInt(lockVersion-1, 10) {
		return true, nil
	}
	return false, staleRefusal()
}

// ifMatchValue is the version an If-Match names: quoted, or the bare number.
func ifMatchValue(h string) string {
	if v, err := strconv.Unquote(h); err == nil {
		return v
	}
	return h
}

func staleRefusal() *refusal {
	return &refusal{http.StatusConflict, "stale_object", "the record changed since you read it; read it again", ""}
}

// --- helpers ----------------------------------------------------------------

func stringList(raw json.RawMessage, pointer string) ([]string, *refusal) {
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return nil, &refusal{http.StatusUnprocessableEntity, "invalid_attribute", "must be a list of strings", pointer}
	}
	return out, nil
}

func stringValue(raw json.RawMessage, pointer string) (string, *refusal) {
	var out *string
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return "", &refusal{http.StatusUnprocessableEntity, "invalid_attribute", "must be a string", pointer}
	}
	return *out, nil
}

func sortedUnique(values []string) []string {
	out := slices.Clone(values)
	if out == nil {
		out = []string{}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// sortedUniqueFold is sortedUnique for values compared without regard to
// case: of values that differ only in case, the first in sorted order is
// kept, as sent.
func sortedUniqueFold(values []string) []string {
	return slices.CompactFunc(sortedUnique(values), strings.EqualFold)
}

func timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

func invalid(pointer, message string) *refusal {
	return &refusal{http.StatusUnprocessableEntity, "invalid_attribute", message, pointer}
}

func itoa(i int) string { return strconv.Itoa(i) }

func splitDots(s string) []string { return strings.Split(s, ".") }
