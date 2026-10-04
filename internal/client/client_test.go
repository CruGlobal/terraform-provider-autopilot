package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	for _, bad := range []string{"", "ftp://x", "autopilot.example.com", "https://", "https://u:p@autopilot.example.com"} {
		if _, err := New(bad, "tok"); err == nil {
			t.Errorf("New(%q) succeeded, want error", bad)
		}
	}
	for _, bad := range []string{"", "  ", "two words"} {
		if _, err := New("https://autopilot.example.com", bad); err == nil {
			t.Errorf("New with token %q succeeded, want error", bad)
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
		},
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`{"name":"billing"}`,
		`{"accepts":[{"kinds":["fix-error"],"sender":"monitor"},{"can_decide":true,"kinds":["implement-work-item","review-pr"],"sender":"tracker"}],"name":"billing","repos":[]}`,
	}
	for i := range want {
		if bodies[i] != want[i] {
			t.Errorf("body %d = %s\nwant      %s", i, bodies[i], want[i])
		}
	}
}

// A change applied by AutoPilot whose answer was lost: the client sends it
// again, meets stale_object from its own change, reads the record and finds
// its change there.
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
	spec := SenderSpec{Secrets: &SenderSecrets{Request: "request-secret-for-a-test-0000000", Callback: "callback-secret-for-a-test-000000"}}
	s := &Sender{
		RequestSecretFingerprint:  Fingerprint("request-secret-for-a-test-0000000"),
		CallbackSecretFingerprint: Fingerprint("callback-secret-for-a-test-000000"),
	}
	if !spec.reflectedIn(s) {
		t.Error("matching fingerprints should count as the secrets being held")
	}
	s.CallbackSecretFingerprint = Fingerprint("something else")
	if spec.reflectedIn(s) {
		t.Error("a different fingerprint should not")
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

func TestSortedUniqueKeepsNilApartFromEmpty(t *testing.T) {
	if sortedUnique(nil) != nil {
		t.Error("nil must stay nil (a field not sent)")
	}
	if got := sortedUnique([]string{}); got == nil || len(got) != 0 {
		t.Error("an empty list must stay an empty, non-nil list (a field sent as [])")
	}
}
