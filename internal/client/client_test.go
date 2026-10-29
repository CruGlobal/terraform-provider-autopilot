package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "fake-admin-token"

func newTestClient(t *testing.T, handler http.Handler, opts ...Option) (*Client, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, testToken, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var slept []time.Duration
	var mu sync.Mutex
	c.sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		slept = append(slept, d)
		return nil
	}
	return c, &slept
}

func writeRecord(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRefusal(w http.ResponseWriter, status int, code, message, field string) {
	writeRecord(w, status, map[string]any{"error": code, "code": code, "message": message, "field": field})
}

// hangUp closes the connection without an answer, as a lost response does.
func hangUp(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Fatal("cannot hijack")
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}

func TestNew_NormalisesEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://autopilot.example.com":           "https://autopilot.example.com/v1/admin",
		"https://autopilot.example.com/":          "https://autopilot.example.com/v1/admin",
		"https://autopilot.example.com/v1/admin":  "https://autopilot.example.com/v1/admin",
		"https://autopilot.example.com/v1/admin/": "https://autopilot.example.com/v1/admin",
		"http://localhost:8080?x=1#frag":          "http://localhost:8080/v1/admin",
		"http://127.0.0.1:8080":                   "http://127.0.0.1:8080/v1/admin",
		"http://[::1]:8080":                       "http://[::1]:8080/v1/admin",
	}
	for in, want := range cases {
		c, err := New(in, "tok")
		if err != nil {
			t.Fatalf("New(%q): %v", in, err)
		}
		if got := c.BaseURL(); got != want {
			t.Errorf("New(%q).BaseURL() = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "ftp://x", "autopilot.example.com", "https://", "https://u:p@autopilot.example.com",
		// The token travels in every request: plain http only to this machine.
		"http://autopilot.example.com", "http://10.0.0.5",
		// A route after /v1/admin: AutoPilot would answer every request
		// not_found, which reads as every record gone.
		"https://autopilot.example.com/v1/admin/senders", "https://autopilot.example.com/v1/admin/senders/",
		"https://autopilot.example.com/proxy/v1/admin/apps/billing", "https://autopilot.example.com/v1/admin/v1/admin"} {
		if _, err := New(bad, "tok"); err == nil {
			t.Errorf("New(%q) succeeded, want error", bad)
		}
	}
	// A path prefix is kept, and a name that only starts like /v1/admin is no
	// route after it.
	for in, want := range map[string]string{
		"https://autopilot.example.com/proxy":             "https://autopilot.example.com/proxy/v1/admin",
		"https://autopilot.example.com/proxy/v1/admin":    "https://autopilot.example.com/proxy/v1/admin",
		"https://autopilot.example.com/v1/administration": "https://autopilot.example.com/v1/administration/v1/admin",
	} {
		c, err := New(in, "tok")
		if err != nil {
			t.Fatalf("New(%q): %v", in, err)
		}
		if got := c.BaseURL(); got != want {
			t.Errorf("New(%q).BaseURL() = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "  ", "two words"} {
		if _, err := New("https://autopilot.example.com", bad); err == nil {
			t.Errorf("New with token %q succeeded, want error", bad)
		}
	}
}

// An endpoint error gives the reason only. A token can end up where the
// endpoint should be (the two swapped, or the wrong output wired in), and the
// error is printed in plan output.
func TestValidateEndpoint_NeverRepeatsTheValue(t *testing.T) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	secret := "tfacc-" + hex.EncodeToString(b[:])
	for _, bad := range []string{
		secret,                              // no scheme: the token itself
		"ftp://" + secret,                   // not http or https
		"https:///" + secret,                // no host
		"http://" + secret + ".example.com", // plain http to another host
		"https://" + secret + "@autopilot.example.com",      // as a user name
		"https://user:" + secret + "@autopilot.example.com", // as a password
		"https://autopilot.example.com:" + secret,           // not a valid URL (the port)
		"https://autopilot.example.com/\x7f" + secret,       // not a valid URL (a control character)
		"https://autopilot.example.com/v1/admin/" + secret,  // a path after /v1/admin
	} {
		err := ValidateEndpoint(bad)
		if err == nil {
			t.Errorf("an endpoint was accepted: %s", strings.ReplaceAll(bad, secret, "<secret>"))
			continue
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), secret[len("tfacc-"):len("tfacc-")+12]) {
			t.Errorf("the error repeats the value: %q", strings.ReplaceAll(err.Error(), secret, "<secret>"))
		}
		if _, err := New(bad, "tok"); err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("New: %v", err)
		}
	}
}

func TestFingerprint(t *testing.T) {
	// Worked out independently: printf '%s' 'autopilot-fingerprint:<secret>' | shasum -a 256 | cut -c1-16
	cases := map[string]string{
		"not-a-real-secret-just-a-test-vector": "5133b2c576a155be",
		"":                                     "df46012db7a7e802",
	}
	for secret, want := range cases {
		if got := Fingerprint(secret); got != want {
			t.Errorf("Fingerprint(%q) = %q, want %q", secret, got, want)
		}
	}
}

func TestDo_SendsHeaders(t *testing.T) {
	var got *http.Request
	var body []byte
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		body, _ = io.ReadAll(r.Body)
		writeRecord(w, http.StatusOK, map[string]any{"name": "tracker", "lock_version": 3})
	}))
	s, err := c.UpdateSender(context.Background(), "tracker", SenderSpec{Kinds: []string{"research", "fix-error", "research"}}, 2)
	if err != nil {
		t.Fatalf("UpdateSender: %v", err)
	}
	if s.LockVersion != 3 {
		t.Errorf("decoded %+v", s)
	}
	if got.Method != http.MethodPatch || got.URL.Path != "/v1/admin/senders/tracker" {
		t.Errorf("request = %s %s", got.Method, got.URL.Path)
	}
	if h := got.Header.Get("Authorization"); h != "Bearer "+testToken {
		t.Errorf("Authorization = %q", h)
	}
	if h := got.Header.Get("If-Match"); h != `"2"` {
		t.Errorf("If-Match = %q, want the quoted lock_version", h)
	}
	if h := got.Header.Get("Idempotency-Key"); h != "" {
		t.Errorf("Idempotency-Key = %q; this API keys creates by name", h)
	}
	if h := got.Header.Get("Content-Type"); h != "application/json" {
		t.Errorf("Content-Type = %q", h)
	}
	if h := got.Header.Get("User-Agent"); h != DefaultUserAgent {
		t.Errorf("User-Agent = %q", h)
	}
	// Lists go out sorted and without repeats, the form the API keeps them in.
	if string(body) != `{"kinds":["fix-error","research"]}` {
		t.Errorf("body = %s", body)
	}
}

func TestDo_RefusalEnvelope(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/admin/senders/invalid":
			writeRefusal(w, http.StatusUnprocessableEntity, "invalid_attribute", "a branch prefix is a lowercase word and a /", "/branch_prefix")
		case "/v1/admin/senders/slug-only":
			writeRecord(w, http.StatusConflict, map[string]any{"error": "name_taken"})
		case "/v1/admin/senders/html":
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprint(w, "<html>bad gateway</html>")
		}
	}), WithMaxRetries(0))

	_, err := c.GetSender(context.Background(), "invalid")
	e, ok := AsError(err)
	if !ok || e.Status != 422 || e.Code != CodeInvalidAttribute || e.Field != "/branch_prefix" ||
		e.Message != "a branch prefix is a lowercase word and a /" {
		t.Errorf("422 parsed as %+v", e)
	}
	if !IsValidation(err) || !HasCode(err, CodeInvalidAttribute) {
		t.Error("IsValidation / HasCode")
	}
	if !strings.Contains(err.Error(), "at /branch_prefix") {
		t.Errorf("Error() = %q, want the field", err.Error())
	}

	_, err = c.GetSender(context.Background(), "slug-only")
	if !HasCode(err, CodeNameTaken) {
		t.Errorf("a refusal with only `error` should still carry the code: %v", err)
	}

	_, err = c.GetSender(context.Background(), "html")
	if e, ok := AsError(err); !ok || e.Status != 502 || !strings.Contains(e.Message, "bad gateway") {
		t.Errorf("non-JSON body parsed as %+v", e)
	}
}

func TestDo_RetriesThrottleHonouringRetryAfter(t *testing.T) {
	calls := 0
	c, slept := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "7")
			writeRefusal(w, http.StatusTooManyRequests, "rate_limited", "slow down", "")
			return
		}
		writeRecord(w, http.StatusOK, map[string]any{"name": "tracker", "lock_version": 1})
	}))
	if _, err := c.GetSender(context.Background(), "tracker"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(*slept) != 1 || (*slept)[0] != 7*time.Second {
		t.Errorf("calls = %d, slept = %v; want one retry after the server's 7s", calls, *slept)
	}
}

func TestDo_RetriesUnavailable(t *testing.T) {
	calls := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls <= 2 {
			writeRefusal(w, http.StatusServiceUnavailable, "unavailable", "AutoPilot couldn't reach its ledger", "")
			return
		}
		writeRecord(w, http.StatusCreated, map[string]any{"name": "tracker", "lock_version": 1})
	}))
	_, outcome, err := c.CreateSender(context.Background(), "tracker", SenderSpec{Secrets: &SenderSecrets{Request: "r", Callback: "c"}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || outcome != Created {
		t.Errorf("calls = %d, outcome = %v", calls, outcome)
	}
}

func TestDo_DoesNotRetryARefusal(t *testing.T) {
	calls := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		writeRefusal(w, http.StatusConflict, "name_taken", "a sender with this name exists with other fields", "/name")
	}))
	_, _, err := c.CreateSender(context.Background(), "tracker", SenderSpec{Secrets: &SenderSecrets{Request: "r", Callback: "c"}})
	if !HasCode(err, CodeNameTaken) || calls != 1 {
		t.Errorf("err = %v after %d calls; a refusal is not retried", err, calls)
	}
}

// A create whose answer was lost is sent again, and the 200 for the
// identical create is this call's own record, not someone else's.
func TestCreate_LostAnswerIsSentAgain(t *testing.T) {
	calls := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			hangUp(t, w)
			return
		}
		writeRecord(w, http.StatusOK, map[string]any{"name": "tracker", "lock_version": 1})
	}))
	s, outcome, err := c.CreateSender(context.Background(), "tracker", SenderSpec{Secrets: &SenderSecrets{Request: "r", Callback: "c"}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || s.Name != "tracker" || outcome != Created {
		t.Errorf("calls = %d, outcome = %v; want a second send whose 200 counts as this create", calls, outcome)
	}
}

func TestCreate_AnsweredWith200IsAdopted(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeRecord(w, http.StatusOK, map[string]any{"name": "billing", "lock_version": 4})
	}))
	_, outcome, err := c.CreateApp(context.Background(), "billing", AppSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != Adopted {
		t.Errorf("outcome = %v, want Adopted", outcome)
	}
}

func TestCreate_RefusesAnUnusableAnswer(t *testing.T) {
	for _, answer := range []string{`null`, `{}`, `{"name": "other", "lock_version": 1}`, `{"name": "billing"}`} {
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, answer)
		}))
		if _, _, err := c.CreateApp(context.Background(), "billing", AppSpec{}); err == nil {
			t.Errorf("answer %s was accepted", answer)
		}
	}
}

func TestCreateApp_SendsOnlyWhatIsSet(t *testing.T) {
	var bodies []string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		writeRecord(w, http.StatusCreated, map[string]any{"name": "billing", "lock_version": 1})
	}))
	ctx := context.Background()
	if _, _, err := c.CreateApp(ctx, "billing", AppSpec{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.CreateApp(ctx, "billing", AppSpec{
		Repos: []string{},
		Accepts: []Accept{
			{Sender: "tracker", Kinds: []string{"review-pr", "implement-work-item"}, CanDecide: true},
			{Sender: "monitor", Kinds: []string{"fix-error"}},
			{Sender: "planner", Kinds: []string{"implement-work-item"}, CanAutoMerge: true},
			{Sender: "autopilot-mcp", Kinds: []string{"review-pr"}, CanDecide: true, CanDecideOwnPR: true},
			{Sender: "autopilot-github", Kinds: []string{"review-pr"}, CanDecide: true, ApproveUpTo: "low"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`{"name":"billing"}`,
		`{"accepts":[{"approve_up_to":"low","can_decide":true,"kinds":["review-pr"],"sender":"autopilot-github"},{"can_decide":true,"can_decide_own_pr":true,"kinds":["review-pr"],"sender":"autopilot-mcp"},{"kinds":["fix-error"],"sender":"monitor"},{"can_auto_merge":true,"kinds":["implement-work-item"],"sender":"planner"},{"can_decide":true,"kinds":["implement-work-item","review-pr"],"sender":"tracker"}],"name":"billing","repos":[]}`,
	}
	for i := range want {
		if bodies[i] != want[i] {
			t.Errorf("body %d = %s\nwant      %s", i, bodies[i], want[i])
		}
	}
}

// A change applied by AutoPilot whose answer was lost: the client sends it
// again. AutoPilot answers a repeat of the change it just made with 200, so
// that is all there is to it.
func TestUpdate_LostAnswerRetryAnswered200(t *testing.T) {
	var mu sync.Mutex
	stored := map[string]any{"name": "tracker", "kinds": []string{"implement-work-item"}, "lock_version": 1}
	patches, gets := 0, 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			gets++
			writeRecord(w, http.StatusOK, stored)
		case http.MethodPatch:
			patches++
			if patches == 1 {
				stored = map[string]any{"name": "tracker", "kinds": []string{"research"}, "lock_version": 2}
				hangUp(t, w)
				return
			}
			// One version behind, and the record already holds the body.
			writeRecord(w, http.StatusOK, stored)
		}
	}))
	s, err := c.UpdateSender(context.Background(), "tracker", SenderSpec{Kinds: []string{"research"}}, 1)
	if err != nil {
		t.Fatalf("UpdateSender: %v", err)
	}
	if patches != 2 || gets != 0 || s.LockVersion != 2 {
		t.Errorf("patches = %d, reads = %d, lock_version = %d", patches, gets, s.LockVersion)
	}
}

// The fallback for an AutoPilot that answers the repeat with stale_object
// instead: the client reads the record and finds its own change there.
func TestUpdate_LostAnswerIsRecognisedAsOwnChange(t *testing.T) {
	var mu sync.Mutex
	stored := map[string]any{"name": "tracker", "kinds": []string{"implement-work-item"}, "lock_version": 1}
	patches := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			writeRecord(w, http.StatusOK, stored)
		case http.MethodPatch:
			patches++
			if r.Header.Get("If-Match") != fmt.Sprintf("%q", fmt.Sprint(stored["lock_version"])) {
				writeRefusal(w, http.StatusConflict, "stale_object", "read it again", "")
				return
			}
			stored = map[string]any{"name": "tracker", "kinds": []string{"research"}, "lock_version": 2}
			hangUp(t, w)
		}
	}))
	s, err := c.UpdateSender(context.Background(), "tracker", SenderSpec{Kinds: []string{"research"}}, 1)
	if err != nil {
		t.Fatalf("UpdateSender: %v", err)
	}
	if patches != 2 || s.LockVersion != 2 {
		t.Errorf("patches = %d, lock_version = %d", patches, s.LockVersion)
	}
}

// The same stale_object after a lost answer, but the record holds something
// else: somebody else's change, which is reported, never taken as ours.
func TestUpdate_LostAnswerThenSomebodyElsesChangeIsStale(t *testing.T) {
	patches := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeRecord(w, http.StatusOK, map[string]any{"name": "tracker", "kinds": []string{"fix-error"}, "lock_version": 2})
		case http.MethodPatch:
			patches++
			if patches == 1 {
				hangUp(t, w)
				return
			}
			writeRefusal(w, http.StatusConflict, "stale_object", "read it again", "")
		}
	}))
	_, err := c.UpdateSender(context.Background(), "tracker", SenderSpec{Kinds: []string{"research"}}, 1)
	if !IsStale(err) {
		t.Errorf("err = %v, want stale_object", err)
	}
}

func TestUpdate_StaleWithoutLostAnswerIsReturned(t *testing.T) {
	gets := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets++
		}
		writeRefusal(w, http.StatusConflict, "stale_object", "read it again", "")
	}))
	_, err := c.UpdateApp(context.Background(), "billing", AppSpec{Repos: []string{"example-org/billing"}}, 1)
	if !IsStale(err) || gets != 0 {
		t.Errorf("err = %v, reads = %d; a plain stale_object is returned without a read", err, gets)
	}
}

func TestSecretsMatchByFingerprint(t *testing.T) {
	request, callback := randomValue(t), randomValue(t)
	spec := SenderSpec{Secrets: &SenderSecrets{Request: request, Callback: callback}}
	s := &Sender{
		RequestSecretFingerprint:  Fingerprint(request),
		CallbackSecretFingerprint: Fingerprint(callback),
	}
	if !spec.reflectedIn(s) {
		t.Error("matching fingerprints should count as the secrets being held")
	}
	s.CallbackSecretFingerprint = Fingerprint(randomValue(t))
	if spec.reflectedIn(s) {
		t.Error("a different fingerprint should not")
	}
}

// A timeout is one attempt failing, not the caller giving up: it is retried,
// and the create that timed out (but was made) is this call's own.
func TestDo_RetriesATimedOutAttempt(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			time.Sleep(400 * time.Millisecond)
			writeRecord(w, http.StatusCreated, map[string]any{"name": "billing", "lock_version": 1})
			return
		}
		writeRecord(w, http.StatusOK, map[string]any{"name": "billing", "lock_version": 1})
	}), WithHTTPClient(&http.Client{Timeout: 100 * time.Millisecond}))
	_, outcome, err := c.CreateApp(context.Background(), "billing", AppSpec{})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if calls != 2 || outcome != Created {
		t.Errorf("calls = %d, outcome = %v; want a retry whose 200 is this create's own", calls, outcome)
	}
}

// The caller's own deadline is not retried.
func TestDo_CallerCancellationIsNotRetried(t *testing.T) {
	calls := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		time.Sleep(200 * time.Millisecond)
		writeRecord(w, http.StatusOK, map[string]any{"name": "billing", "lock_version": 1})
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.GetApp(ctx, "billing"); err == nil {
		t.Fatal("want an error once the caller's deadline passes")
	}
	if calls != 1 {
		t.Errorf("calls = %d; the caller's own deadline must not be retried", calls)
	}
}

// An answer whose body is cut off was still an answer: the request was
// handled. It is sent again, and counts as unanswered.
func TestDo_RetriesALostBody(t *testing.T) {
	calls := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "200")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"name": "bill`)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			hangUp(t, w)
			return
		}
		writeRecord(w, http.StatusOK, map[string]any{"name": "billing", "lock_version": 1})
	}))
	_, outcome, err := c.CreateApp(context.Background(), "billing", AppSpec{})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if calls != 2 || outcome != Created {
		t.Errorf("calls = %d, outcome = %v", calls, outcome)
	}
}

// A gateway's 502 or 504 says nothing about whether AutoPilot made the
// record, so a 200 after it is this create's own, not someone else's.
func TestCreate_GatewayErrorThen200IsCreated(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusGatewayTimeout} {
		calls := 0
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			if calls == 1 {
				w.WriteHeader(status)
				return
			}
			writeRecord(w, http.StatusOK, map[string]any{"name": "billing", "lock_version": 1})
		}))
		_, outcome, err := c.CreateApp(context.Background(), "billing", AppSpec{})
		if err != nil {
			t.Fatalf("%d: %v", status, err)
		}
		if outcome != Created {
			t.Errorf("after a %d, outcome = %v, want Created", status, outcome)
		}
	}
}

// Only AutoPilot's not_found refusal means a record is gone. A 404 from
// anything else is an error, for reads and deletes alike.
func TestNotFound_OnlyTheRefusalMeansGone(t *testing.T) {
	plain := true
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if plain {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, "<html>Not Found</html>")
			return
		}
		writeRefusal(w, http.StatusNotFound, "not_found", "no such sender", "")
	}))
	ctx := context.Background()
	_, err := c.GetSender(ctx, "tracker")
	if err == nil || IsNotFound(err) {
		t.Errorf("a plain 404 read as gone: %v", err)
	}
	if err := c.DeleteSender(ctx, "tracker", 1); err == nil {
		t.Error("a delete answered with a plain 404 was taken as done")
	}
	plain = false
	if _, err := c.GetSender(ctx, "tracker"); !IsNotFound(err) {
		t.Errorf("the not_found refusal should read as gone: %v", err)
	}
}

func TestDelete_GoneIsSuccess(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeRefusal(w, http.StatusNotFound, "not_found", "no such sender", "")
	}))
	if err := c.DeleteSender(context.Background(), "tracker", 1); err != nil {
		t.Errorf("DeleteSender of a missing sender: %v", err)
	}
}

// AutoPilot never redirects, so the client never follows a redirect: a 3xx is
// an error, a delete answered with one is not done, and wherever it points
// never sees a request, or the token.
func TestDo_NeverFollowsARedirect(t *testing.T) {
	var followed sync.Map
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Store(r.Method+" "+r.URL.Path, r.Header.Get("Authorization"))
		writeRecord(w, http.StatusOK, map[string]any{"name": "tracker", "lock_version": 1})
	}))
	t.Cleanup(target.Close)
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/moved"+r.URL.Path, status)
		}))
		ctx := context.Background()
		if _, err := c.GetSender(ctx, "tracker"); err == nil || !hasStatus(err, status) {
			t.Errorf("%d: a read was answered %v, want an HTTP %d error", status, err, status)
		}
		if err := c.DeleteSender(ctx, "tracker", 1); err == nil || !hasStatus(err, status) {
			t.Errorf("%d: a delete was answered %v, want an HTTP %d error", status, err, status)
		}
	}
	followed.Range(func(k, _ any) bool {
		t.Errorf("a redirect was followed: %v", k)
		return true
	})
}

// A client passed in keeps its own redirect policy; New works on a copy.
func TestNew_LeavesAGivenHTTPClientAsItIs(t *testing.T) {
	given := &http.Client{}
	if _, err := New("https://autopilot.example.com", "tok", WithHTTPClient(given)); err != nil {
		t.Fatal(err)
	}
	if given.CheckRedirect != nil {
		t.Error("New changed the http.Client it was given")
	}
}

// randomValue is a fresh random string, so no test holds a fixed secret.
func randomValue(t *testing.T) string {
	t.Helper()
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

func TestSortedUniqueKeepsNilApartFromEmpty(t *testing.T) {
	if sortedUnique(nil) != nil {
		t.Error("nil must stay nil (a field not sent)")
	}
	if got := sortedUnique([]string{}); got == nil || len(got) != 0 {
		t.Error("an empty list must stay an empty, non-nil list (a field sent as [])")
	}
}

// A sender's branch prefix never changes, so a change can't send one.
func TestUpdateSender_RefusesABranchPrefix(t *testing.T) {
	calls := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++ }))
	prefix := "bots/"
	if _, err := c.UpdateSender(context.Background(), "tracker", SenderSpec{BranchPrefix: &prefix}, 1); err == nil || calls != 0 {
		t.Errorf("err = %v after %d calls", err, calls)
	}
}

// The credential policy is sent as one: credential always, as null to take
// the app's own away, and when_seats_full only when set.
func TestUpdateApp_SendsTheCredentialPolicyAsOne(t *testing.T) {
	var bodies []string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		writeRecord(w, http.StatusOK, map[string]any{"name": "billing", "lock_version": 2})
	}))
	ctx := context.Background()
	for _, policy := range []*CredentialPolicy{
		{Credential: "seat_first", WhenSeatsFull: "wait"},
		{Credential: "bedrock"},
		{},
	} {
		if _, err := c.UpdateApp(ctx, "billing", AppSpec{Policy: policy}, 1); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		`{"credential":"seat_first","when_seats_full":"wait"}`,
		`{"credential":"bedrock"}`,
		`{"credential":null}`,
	}
	for i := range want {
		if bodies[i] != want[i] {
			t.Errorf("body %d = %s\nwant      %s", i, bodies[i], want[i])
		}
	}
	if !(AppSpec{}).Empty() {
		t.Error("a spec without a policy sends nothing")
	}
}

// An app read back holds a change's credential policy only when both of
// its fields match.
func TestAppSpec_PolicyReflectedIn(t *testing.T) {
	str := func(s string) *string { return &s }
	seatFirst := &App{Credential: str("seat_first"), WhenSeatsFull: str("wait")}
	cases := []struct {
		spec AppSpec
		app  *App
		want bool
	}{
		{AppSpec{Policy: &CredentialPolicy{Credential: "seat_first", WhenSeatsFull: "wait"}}, seatFirst, true},
		{AppSpec{Policy: &CredentialPolicy{Credential: "seat_first", WhenSeatsFull: "fall_back"}}, seatFirst, false},
		{AppSpec{Policy: &CredentialPolicy{}}, seatFirst, false},
		{AppSpec{Policy: &CredentialPolicy{}}, &App{}, true},
		{AppSpec{}, seatFirst, true},
	}
	for i, c := range cases {
		if got := c.spec.reflectedIn(c.app); got != c.want {
			t.Errorf("case %d: reflectedIn = %v, want %v", i, got, c.want)
		}
	}
}

// An AutoPilot that predates the credential policy answers without it: the
// app reads as having none.
func TestGetApp_WithoutTheCredentialPolicy(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeRecord(w, http.StatusOK, map[string]any{"name": "billing", "lock_version": 1})
	}))
	a, err := c.GetApp(context.Background(), "billing")
	if err != nil {
		t.Fatal(err)
	}
	if a.Credential != nil || a.WhenSeatsFull != nil {
		t.Errorf("credential = %v, when_seats_full = %v", a.Credential, a.WhenSeatsFull)
	}
}

func TestDo_MethodNotAllowedRefusal(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", "GET, POST")
		writeRefusal(w, http.StatusMethodNotAllowed, "method_not_allowed", "this route doesn't take PATCH", "")
	}))
	_, err := c.GetApp(context.Background(), "billing")
	if !HasCode(err, CodeMethodNotAllowed) || IsNotFound(err) {
		t.Errorf("err = %v", err)
	}
}

// The worker image is sent when set, as null to take the app back to node,
// and the size as one: both fields always, as null to take it away.
func TestUpdateApp_SendsTheWorker(t *testing.T) {
	var bodies []string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		writeRecord(w, http.StatusOK, map[string]any{"name": "billing", "lock_version": 2})
	}))
	ctx := context.Background()
	str := func(s string) *string { return &s }
	for _, spec := range []AppSpec{
		{WorkerImage: str("ruby")},
		{WorkerImage: str("")},
		{Size: &WorkerSize{CPU: 4096, Memory: 16384}},
		{Size: &WorkerSize{}},
		{WorkerImage: str("go"), Size: &WorkerSize{CPU: 1024, Memory: 2048}},
	} {
		if _, err := c.UpdateApp(ctx, "billing", spec, 1); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		`{"worker_image":"ruby"}`,
		`{"worker_image":null}`,
		`{"worker_cpu":4096,"worker_memory":16384}`,
		`{"worker_cpu":null,"worker_memory":null}`,
		`{"worker_cpu":1024,"worker_image":"go","worker_memory":2048}`,
	}
	for i := range want {
		if bodies[i] != want[i] {
			t.Errorf("body %d = %s\nwant      %s", i, bodies[i], want[i])
		}
	}
}

// An app read back holds a change's worker only when what it sends matches.
func TestAppSpec_WorkerReflectedIn(t *testing.T) {
	str := func(s string) *string { return &s }
	num := func(n int64) *int64 { return &n }
	ruby := &App{WorkerImage: str("ruby"), WorkerCPU: num(4096), WorkerMemory: num(16384)}
	cases := []struct {
		spec AppSpec
		app  *App
		want bool
	}{
		{AppSpec{WorkerImage: str("ruby")}, ruby, true},
		{AppSpec{WorkerImage: str("go")}, ruby, false},
		{AppSpec{WorkerImage: str("")}, ruby, false},
		{AppSpec{WorkerImage: str("")}, &App{}, true},
		{AppSpec{Size: &WorkerSize{CPU: 4096, Memory: 16384}}, ruby, true},
		{AppSpec{Size: &WorkerSize{CPU: 4096, Memory: 8192}}, ruby, false},
		{AppSpec{Size: &WorkerSize{}}, ruby, false},
		{AppSpec{Size: &WorkerSize{}}, &App{}, true},
		{AppSpec{}, ruby, true},
	}
	for i, c := range cases {
		if got := c.spec.reflectedIn(c.app); got != c.want {
			t.Errorf("case %d: reflectedIn = %v, want %v", i, got, c.want)
		}
	}
}

// An AutoPilot that predates the worker answers without it: the app reads
// as running node at its worker's own size.
func TestGetApp_WithoutTheWorker(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeRecord(w, http.StatusOK, map[string]any{"name": "billing", "lock_version": 1})
	}))
	a, err := c.GetApp(context.Background(), "billing")
	if err != nil {
		t.Fatal(err)
	}
	if a.WorkerImage != nil || a.WorkerCPU != nil || a.WorkerMemory != nil {
		t.Errorf("worker_image = %v, worker_cpu = %v, worker_memory = %v", a.WorkerImage, a.WorkerCPU, a.WorkerMemory)
	}
}

// GitHub logins are sent only when set, as [] when empty, sorted by login,
// with an entry listed twice exactly sent once.
func TestAppSpec_SendsGitHubLogins(t *testing.T) {
	var bodies []string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		writeRecord(w, http.StatusOK, map[string]any{"name": "billing", "lock_version": 2})
	}))
	ctx := context.Background()
	for _, spec := range []AppSpec{
		{},
		{GitHubLogins: []GitHubLogin{}},
		{GitHubLogins: []GitHubLogin{
			{Login: "zed", Developer: "zed@example.org"},
			{Login: "alice-gh", Developer: "alice@example.org"},
			{Login: "zed", Developer: "zed@example.org"},
			{Login: "alice_work", Developer: "alice@example.org"},
		}},
	} {
		if _, err := c.UpdateApp(ctx, "billing", spec, 1); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		`{}`,
		`{"github_logins":[]}`,
		`{"github_logins":[{"login":"alice-gh","developer":"alice@example.org"},{"login":"alice_work","developer":"alice@example.org"},{"login":"zed","developer":"zed@example.org"}]}`,
	}
	for i := range want {
		if bodies[i] != want[i] {
			t.Errorf("body %d = %s\nwant      %s", i, bodies[i], want[i])
		}
	}
	if !(AppSpec{}).Empty() || (AppSpec{GitHubLogins: []GitHubLogin{}}).Empty() {
		t.Error("a nil list sends nothing, and an empty one sends []")
	}
}

// An app read back holds a change's GitHub logins only when the pairs match,
// in any order.
func TestAppSpec_GitHubLoginsReflectedIn(t *testing.T) {
	alice := GitHubLogin{Login: "alice-gh", Developer: "alice@example.org"}
	bob := GitHubLogin{Login: "bob-gh", Developer: "bob@example.org"}
	held := &App{GitHubLogins: []GitHubLogin{alice, bob}}
	cases := []struct {
		spec AppSpec
		app  *App
		want bool
	}{
		{AppSpec{GitHubLogins: []GitHubLogin{bob, alice}}, held, true},
		{AppSpec{GitHubLogins: []GitHubLogin{alice}}, held, false},
		{AppSpec{GitHubLogins: []GitHubLogin{alice, {Login: "bob-gh", Developer: "alice@example.org"}}}, held, false},
		{AppSpec{GitHubLogins: []GitHubLogin{}}, held, false},
		{AppSpec{GitHubLogins: []GitHubLogin{}}, &App{}, true},
		{AppSpec{}, held, true},
	}
	for i, c := range cases {
		if got := c.spec.reflectedIn(c.app); got != c.want {
			t.Errorf("case %d: reflectedIn = %v, want %v", i, got, c.want)
		}
	}
}

// An app read back holds a change's accepts only when each entry's flags
// match, can_decide_own_pr among them.
func TestAppSpec_AcceptsReflectedIn(t *testing.T) {
	mcp := func(ownPR bool) Accept {
		return Accept{Sender: "autopilot-mcp", Kinds: []string{"review-pr"}, CanDecide: true, CanDecideOwnPR: ownPR}
	}
	held := &App{Accepts: []Accept{mcp(true)}}
	cases := []struct {
		spec AppSpec
		app  *App
		want bool
	}{
		{AppSpec{Accepts: []Accept{mcp(true)}}, held, true},
		{AppSpec{Accepts: []Accept{mcp(false)}}, held, false},
		{AppSpec{Accepts: []Accept{mcp(true)}}, &App{Accepts: []Accept{mcp(false)}}, false},
		{AppSpec{}, held, true},
	}
	for i, c := range cases {
		if got := c.spec.reflectedIn(c.app); got != c.want {
			t.Errorf("case %d: reflectedIn = %v, want %v", i, got, c.want)
		}
	}
}

// An app read back holds a change's accepts only when each entry's ceiling
// matches too.
func TestAppSpec_ApproveUpToReflectedIn(t *testing.T) {
	github := func(ceiling string) Accept {
		return Accept{Sender: "autopilot-github", Kinds: []string{"review-pr"}, CanDecide: true, ApproveUpTo: ceiling}
	}
	cases := []struct {
		spec AppSpec
		app  *App
		want bool
	}{
		{AppSpec{Accepts: []Accept{github("low")}}, &App{Accepts: []Accept{github("low")}}, true},
		{AppSpec{Accepts: []Accept{github("low")}}, &App{Accepts: []Accept{github("medium")}}, false},
		{AppSpec{Accepts: []Accept{github("low")}}, &App{Accepts: []Accept{github("")}}, false},
		{AppSpec{Accepts: []Accept{github("")}}, &App{Accepts: []Accept{github("high")}}, false},
		{AppSpec{Accepts: []Accept{github("")}}, &App{Accepts: []Accept{github("")}}, true},
	}
	for i, c := range cases {
		if got := c.spec.reflectedIn(c.app); got != c.want {
			t.Errorf("case %d: reflectedIn = %v, want %v", i, got, c.want)
		}
	}
}

// AutoPilot answers approve_up_to as null when an entry has no ceiling, and
// an AutoPilot that predates it answers without it: both read as none.
func TestGetApp_ApproveUpTo(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeRecord(w, http.StatusOK, map[string]any{"name": "billing", "lock_version": 1, "accepts": []any{
			map[string]any{"sender": "autopilot-github", "kinds": []string{"review-pr"}, "can_decide": true, "approve_up_to": "low"},
			map[string]any{"sender": "autopilot-mcp", "kinds": []string{"review-pr"}, "can_decide": true, "approve_up_to": nil},
			map[string]any{"sender": "tracker", "kinds": []string{"review-pr"}, "can_decide": true},
		}})
	}))
	a, err := c.GetApp(context.Background(), "billing")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Accepts) != 3 || a.Accepts[0].ApproveUpTo != "low" || a.Accepts[1].ApproveUpTo != "" || a.Accepts[2].ApproveUpTo != "" {
		t.Errorf("accepts = %+v", a.Accepts)
	}
}

// An AutoPilot that predates can_decide_own_pr answers without it: the
// entry reads as having it off.
func TestGetApp_WithoutCanDecideOwnPR(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeRecord(w, http.StatusOK, map[string]any{"name": "billing", "lock_version": 1, "accepts": []any{
			map[string]any{"sender": "autopilot-mcp", "kinds": []string{"review-pr"}, "can_decide": true, "can_auto_merge": false},
		}})
	}))
	a, err := c.GetApp(context.Background(), "billing")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Accepts) != 1 || !a.Accepts[0].CanDecide || a.Accepts[0].CanDecideOwnPR {
		t.Errorf("accepts = %+v", a.Accepts)
	}
}

// An AutoPilot that predates github_logins answers without it: the app reads
// as having none.
func TestGetApp_WithoutGitHubLogins(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeRecord(w, http.StatusOK, map[string]any{"name": "billing", "lock_version": 1})
	}))
	a, err := c.GetApp(context.Background(), "billing")
	if err != nil {
		t.Fatal(err)
	}
	if a.GitHubLogins != nil {
		t.Errorf("github_logins = %v", a.GitHubLogins)
	}
}

// Automatic reviews are sent whole when set: every field, the events each
// once in AutoPilot's order, the flags even when off and no ceiling as null;
// and as null to take them away. Left unset, they aren't sent. The workflows
// are sent sorted and each once, and only when there are some: never as []
// or null, which an AutoPilot that predates them would refuse.
func TestUpdateApp_SendsAutoReview(t *testing.T) {
	var bodies []string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		writeRecord(w, http.StatusOK, map[string]any{"name": "billing", "lock_version": 2})
	}))
	ctx := context.Background()
	for _, spec := range []AppSpec{
		{AutoReview: &AutoReviewSetting{On: &AutoReview{Events: []string{"ready_for_review", "opened"}, MaxPerDay: 50}}},
		{AutoReview: &AutoReviewSetting{On: &AutoReview{
			Events: []string{"synchronize", "opened", "synchronize"}, MaxPerDay: 10,
			CanDecide: true, CanDecideOwnPR: true, ApproveUpTo: "low",
		}}},
		{AutoReview: &AutoReviewSetting{}},
		{Developers: []string{}},
		{AutoReview: &AutoReviewSetting{On: &AutoReview{
			Events: []string{}, MaxPerDay: 50,
			Workflows: []string{"o/r/.github/workflows/b.yml@refs/heads/main", "O/r/.github/workflows/a.yml@refs/heads/main",
				"o/r/.github/workflows/b.yml@refs/heads/main"},
		}}},
		{AutoReview: &AutoReviewSetting{On: &AutoReview{Events: []string{"opened"}, MaxPerDay: 50, Workflows: []string{}}}},
	} {
		if _, err := c.UpdateApp(ctx, "billing", spec, 1); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		`{"auto_review":{"approve_up_to":null,"can_decide":false,"can_decide_own_pr":false,"events":["opened","ready_for_review"],"max_per_day":50}}`,
		`{"auto_review":{"approve_up_to":"low","can_decide":true,"can_decide_own_pr":true,"events":["opened","synchronize"],"max_per_day":10}}`,
		`{"auto_review":null}`,
		`{"developers":[]}`,
		`{"auto_review":{"approve_up_to":null,"can_decide":false,"can_decide_own_pr":false,"events":[],"max_per_day":50,"workflows":["O/r/.github/workflows/a.yml@refs/heads/main","o/r/.github/workflows/b.yml@refs/heads/main"]}}`,
		`{"auto_review":{"approve_up_to":null,"can_decide":false,"can_decide_own_pr":false,"events":["opened"],"max_per_day":50}}`,
	}
	for i := range want {
		if bodies[i] != want[i] {
			t.Errorf("body %d = %s\nwant      %s", i, bodies[i], want[i])
		}
	}
}

// An app read back holds a change's automatic reviews only when every field
// matches, and none only when it has none.
func TestAppSpec_AutoReviewReflectedIn(t *testing.T) {
	on := func(change func(a *AutoReview)) *AutoReview {
		a := &AutoReview{Events: []string{"opened", "ready_for_review"}, MaxPerDay: 50}
		change(a)
		return a
	}
	same := func(*AutoReview) {}
	set := func(a *AutoReview) AppSpec { return AppSpec{AutoReview: &AutoReviewSetting{On: a}} }
	cases := []struct {
		spec AppSpec
		app  *App
		want bool
	}{
		{set(on(same)), &App{AutoReview: on(same)}, true},
		{set(on(func(a *AutoReview) { a.Events = []string{"ready_for_review", "opened"} })), &App{AutoReview: on(same)}, true},
		{set(on(same)), &App{}, false},
		{set(on(func(a *AutoReview) { a.Events = []string{"opened"} })), &App{AutoReview: on(same)}, false},
		{set(on(func(a *AutoReview) { a.MaxPerDay = 10 })), &App{AutoReview: on(same)}, false},
		{set(on(func(a *AutoReview) { a.CanDecide = true })), &App{AutoReview: on(same)}, false},
		{set(on(func(a *AutoReview) { a.CanDecide, a.CanDecideOwnPR = true, true })),
			&App{AutoReview: on(func(a *AutoReview) { a.CanDecide = true })}, false},
		{set(on(func(a *AutoReview) { a.CanDecide, a.ApproveUpTo = true, "low" })),
			&App{AutoReview: on(func(a *AutoReview) { a.CanDecide = true })}, false},
		{set(on(func(a *AutoReview) { a.Workflows = []string{"o/r/.github/workflows/a.yml@refs/heads/main"} })),
			&App{AutoReview: on(same)}, false},
		{set(on(same)), &App{AutoReview: on(func(a *AutoReview) { a.Workflows = []string{"o/r/.github/workflows/a.yml@refs/heads/main"} })}, false},
		{set(on(func(a *AutoReview) {
			a.Workflows = []string{"o/r/.github/workflows/b.yml@refs/heads/main", "o/r/.github/workflows/a.yml@refs/heads/main"}
		})), &App{AutoReview: on(func(a *AutoReview) {
			a.Workflows = []string{"o/r/.github/workflows/a.yml@refs/heads/main", "o/r/.github/workflows/b.yml@refs/heads/main"}
		})}, true},
		// A change of case alone is a change.
		{set(on(func(a *AutoReview) { a.Workflows = []string{"O/r/.github/workflows/a.yml@refs/heads/main"} })),
			&App{AutoReview: on(func(a *AutoReview) { a.Workflows = []string{"o/r/.github/workflows/a.yml@refs/heads/main"} })}, false},
		// None is none, nil or empty.
		{set(on(func(a *AutoReview) { a.Workflows = []string{} })), &App{AutoReview: on(same)}, true},
		{set(nil), &App{AutoReview: on(same)}, false},
		{set(nil), &App{}, true},
		{AppSpec{}, &App{AutoReview: on(same)}, true},
	}
	for i, c := range cases {
		if got := c.spec.reflectedIn(c.app); got != c.want {
			t.Errorf("case %d: reflectedIn = %v, want %v", i, got, c.want)
		}
	}
}

// AutoPilot answers auto_review as null for an app without automatic
// reviews, and an AutoPilot that predates it answers without it: both read
// as none. A ceiling of null reads as none, and so do workflows left out, as
// an AutoPilot that predates them answers.
func TestGetApp_AutoReview(t *testing.T) {
	answers := []map[string]any{
		{"name": "billing", "lock_version": 1},
		{"name": "billing", "lock_version": 1, "auto_review": nil},
		{"name": "billing", "lock_version": 1, "auto_review": map[string]any{
			"events": []string{"opened", "synchronize"}, "max_per_day": 10, "can_decide": true,
			"can_decide_own_pr": false, "approve_up_to": nil,
		}},
	}
	for i, answer := range answers {
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeRecord(w, http.StatusOK, answer)
		}))
		a, err := c.GetApp(context.Background(), "billing")
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 && a.AutoReview != nil {
			t.Errorf("answer %d: auto_review = %+v, want none", i, a.AutoReview)
		}
		if i == 2 && (a.AutoReview == nil || !slices.Equal(a.AutoReview.Events, []string{"opened", "synchronize"}) ||
			a.AutoReview.MaxPerDay != 10 || !a.AutoReview.CanDecide || a.AutoReview.CanDecideOwnPR || a.AutoReview.ApproveUpTo != "" ||
			len(a.AutoReview.Workflows) != 0) {
			t.Errorf("auto_review = %+v", a.AutoReview)
		}
	}
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeRecord(w, http.StatusOK, map[string]any{"name": "billing", "lock_version": 1, "auto_review": map[string]any{
			"events": []string{}, "workflows": []string{"o/r/.github/workflows/a.yml@refs/heads/main"}, "max_per_day": 50,
			"can_decide": false, "can_decide_own_pr": false, "approve_up_to": nil,
		}})
	}))
	a, err := c.GetApp(context.Background(), "billing")
	if err != nil {
		t.Fatal(err)
	}
	if a.AutoReview == nil || len(a.AutoReview.Events) != 0 ||
		!slices.Equal(a.AutoReview.Workflows, []string{"o/r/.github/workflows/a.yml@refs/heads/main"}) {
		t.Errorf("auto_review = %+v, want the workflow and no event", a.AutoReview)
	}
}

// The repositories an app's reviews also read are sent sorted and each once.
// Left unset, they aren't sent, so an AutoPilot that predates the field still
// takes the app; an empty list is sent as [], which takes them all away.
func TestAppSpec_SendsReviewAlsoReads(t *testing.T) {
	var bodies []string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		writeRecord(w, http.StatusOK, map[string]any{"name": "billing", "lock_version": 2})
	}))
	ctx := context.Background()
	for _, spec := range []AppSpec{
		{},
		{ReviewAlsoReads: []string{}},
		{ReviewAlsoReads: []string{"example-org/terraform-modules", "Example-Org/Shared", "example-org/terraform-modules"}},
		{Repos: []string{"example-org/infrastructure"}, ReviewAlsoReads: []string{"example-org/terraform-modules"}},
	} {
		if _, err := c.UpdateApp(ctx, "billing", spec, 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := c.CreateApp(ctx, "billing", AppSpec{Repos: []string{"example-org/infrastructure"}}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`{}`,
		`{"review_also_reads":[]}`,
		`{"review_also_reads":["Example-Org/Shared","example-org/terraform-modules"]}`,
		`{"repos":["example-org/infrastructure"],"review_also_reads":["example-org/terraform-modules"]}`,
		`{"name":"billing","repos":["example-org/infrastructure"]}`,
	}
	for i := range want {
		if bodies[i] != want[i] {
			t.Errorf("body %d = %s\nwant      %s", i, bodies[i], want[i])
		}
	}
	if (AppSpec{ReviewAlsoReads: []string{}}).Empty() {
		t.Error("an empty list sends []")
	}
}

// An app read back holds a change's repositories to also read only when they
// match, in any order. The case of each is part of it, as AutoPilot keeps
// them as sent.
func TestAppSpec_ReviewAlsoReadsReflectedIn(t *testing.T) {
	held := &App{ReviewAlsoReads: []string{"example-org/a", "example-org/b"}}
	cases := []struct {
		spec AppSpec
		app  *App
		want bool
	}{
		{AppSpec{ReviewAlsoReads: []string{"example-org/b", "example-org/a"}}, held, true},
		{AppSpec{ReviewAlsoReads: []string{"example-org/a"}}, held, false},
		{AppSpec{ReviewAlsoReads: []string{"example-org/a", "Example-Org/B"}}, held, false},
		{AppSpec{ReviewAlsoReads: []string{}}, held, false},
		{AppSpec{ReviewAlsoReads: []string{}}, &App{}, true},
		{AppSpec{ReviewAlsoReads: []string{}}, &App{ReviewAlsoReads: []string{}}, true},
		{AppSpec{}, held, true},
	}
	for i, c := range cases {
		if got := c.spec.reflectedIn(c.app); got != c.want {
			t.Errorf("case %d: reflectedIn = %v, want %v", i, got, c.want)
		}
	}
}

// An answer without review_also_reads, from an AutoPilot that predates it,
// reads as none; one with it reads as it is.
func TestGetApp_ReviewAlsoReads(t *testing.T) {
	answers := []struct {
		answer map[string]any
		want   []string
	}{
		{map[string]any{"name": "billing", "lock_version": 1}, nil},
		{map[string]any{"name": "billing", "lock_version": 1, "review_also_reads": []string{}}, []string{}},
		{map[string]any{"name": "billing", "lock_version": 1, "review_also_reads": []string{"example-org/a", "example-org/b"}},
			[]string{"example-org/a", "example-org/b"}},
	}
	for i, a := range answers {
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeRecord(w, http.StatusOK, a.answer)
		}))
		app, err := c.GetApp(context.Background(), "billing")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(app.ReviewAlsoReads, a.want) || (a.want == nil) != (app.ReviewAlsoReads == nil) {
			t.Errorf("answer %d: review_also_reads = %#v, want %#v", i, app.ReviewAlsoReads, a.want)
		}
	}
}
