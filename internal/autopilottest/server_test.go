package autopilottest

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// call sends one request to the fake and decodes its JSON answer.
func call(t *testing.T, s *Server, method, path, ifMatch string, body any) (int, map[string]any) {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, s.URL+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.Token())
	req.Header.Set("Content-Type", "application/json")
	if ifMatch != "" {
		req.Header.Set("If-Match", strconv.Quote(ifMatch))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func randomSecret(t *testing.T) string {
	t.Helper()
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

func expect(t *testing.T, what string, gotStatus, wantStatus int, body map[string]any) {
	t.Helper()
	if gotStatus != wantStatus {
		t.Fatalf("%s: status %d, want %d (%v)", what, gotStatus, wantStatus, body)
	}
}

// A PATCH replaces a list with the one it sends, as sent, so a change of case
// alone is a change.
func TestPatch_CaseOnlyChangeIsAChange(t *testing.T) {
	s := New(t)
	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "repos": []string{"Example-Org/Billing"}})
	expect(t, "create", st, http.StatusCreated, body)
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "1", map[string]any{"repos": []string{"example-org/billing"}})
	expect(t, "patch", st, http.StatusOK, body)
	if body["lock_version"] != float64(2) {
		t.Errorf("lock_version = %v, want 2: a change of case is a change", body["lock_version"])
	}
	if v, _ := s.App("billing"); len(v.Repos) != 1 || v.Repos[0] != "example-org/billing" {
		t.Errorf("repos = %v, want the new case, as sent", v.Repos)
	}
}

// Repositories are matched without regard to case: one named twice in
// different cases is refused, twice exactly is kept once, and a second app
// can't take one in another case.
func TestRepos_MatchedWithoutRegardToCase(t *testing.T) {
	s := New(t)
	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "repos": []string{"example-org/a", "Example-Org/A"}})
	expect(t, "one repository in two cases", st, http.StatusUnprocessableEntity, body)
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "repos": []string{"example-org/a", "example-org/a"}})
	expect(t, "one repository twice exactly", st, http.StatusCreated, body)
	if v, _ := s.App("billing"); len(v.Repos) != 1 {
		t.Errorf("repos = %v, want one", v.Repos)
	}
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "payroll", "repos": []string{"EXAMPLE-ORG/a"}})
	expect(t, "a second app taking the same repository", st, http.StatusConflict, body)
	if body["code"] != "repo_taken" {
		t.Errorf("code = %v", body["code"])
	}
}

// An accepts entry sent twice exactly is kept once; two different entries for
// one sender are refused.
func TestAccepts_Duplicates(t *testing.T) {
	s := New(t)
	entry := map[string]any{"sender": "tracker", "kinds": []string{"research"}}
	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "accepts": []any{entry, entry}})
	expect(t, "the same entry twice", st, http.StatusCreated, body)
	if v, _ := s.App("billing"); len(v.Accepts) != 1 {
		t.Errorf("accepts = %v, want one", v.Accepts)
	}
	other := map[string]any{"sender": "tracker", "kinds": []string{"fix-error"}}
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "payroll", "accepts": []any{entry, other}})
	expect(t, "two entries for one sender", st, http.StatusUnprocessableEntity, body)
}

func TestDevelopers_LowercaseASCIIOnly(t *testing.T) {
	s := New(t)
	for _, bad := range []string{"Someone@example.com", "someone@Example.com", "someoné@example.com", "someone", "some one@example.com"} {
		st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "developers": []string{bad}})
		expect(t, bad, st, http.StatusUnprocessableEntity, body)
		if body["field"] != "/developers/0" {
			t.Errorf("%s: field = %v", bad, body["field"])
		}
	}
	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "developers": []string{"some.one+ap@example.com"}})
	expect(t, "lowercase ASCII", st, http.StatusCreated, body)
}

func TestLengthLimits(t *testing.T) {
	s := New(t)
	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": strings.Repeat("a", 65)})
	expect(t, "a 65-character app name", st, http.StatusUnprocessableEntity, body)
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": strings.Repeat("a", 64)})
	expect(t, "a 64-character app name", st, http.StatusCreated, body)

	sender := func(name, prefix string) map[string]any {
		return map[string]any{"name": name, "branch_prefix": prefix, "request_secret": randomSecret(t), "callback_secret": randomSecret(t)}
	}
	st, body = call(t, s, http.MethodPost, "/v1/admin/senders", "", sender("tracker", strings.Repeat("a", 41)+"/"))
	expect(t, "a 42-character branch prefix", st, http.StatusUnprocessableEntity, body)
	st, body = call(t, s, http.MethodPost, "/v1/admin/senders", "", sender("tracker", strings.Repeat("a", 40)+"/"))
	expect(t, "a 41-character branch prefix", st, http.StatusCreated, body)
}

func TestSecrets_OneAloneIsRefused(t *testing.T) {
	s := New(t)
	st, body := call(t, s, http.MethodPost, "/v1/admin/senders", "", map[string]any{
		"name": "tracker", "request_secret": randomSecret(t), "callback_secret": randomSecret(t)})
	expect(t, "create", st, http.StatusCreated, body)
	st, body = call(t, s, http.MethodPatch, "/v1/admin/senders/tracker", "1", map[string]any{"request_secret": randomSecret(t)})
	expect(t, "one secret alone", st, http.StatusUnprocessableEntity, body)
}

// A PATCH that names the version just before the record's, when the record
// already holds what it sends, is that same change whose answer was lost:
// 200. Anything else that is not the current version is stale.
func TestPatch_RepeatOneVersionBehind(t *testing.T) {
	s := New(t)
	request, callback := randomSecret(t), randomSecret(t)
	st, body := call(t, s, http.MethodPost, "/v1/admin/senders", "", map[string]any{
		"name": "tracker", "request_secret": request, "callback_secret": callback})
	expect(t, "create", st, http.StatusCreated, body)

	newRequest, newCallback := randomSecret(t), randomSecret(t)
	change := map[string]any{"kinds": []string{"research"}, "request_secret": newRequest, "callback_secret": newCallback}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/senders/tracker", "1", change)
	expect(t, "the change", st, http.StatusOK, body)
	if body["lock_version"] != float64(2) {
		t.Fatalf("lock_version = %v", body["lock_version"])
	}

	st, body = call(t, s, http.MethodPatch, "/v1/admin/senders/tracker", "1", change)
	expect(t, "the same change again, one version behind", st, http.StatusOK, body)
	if body["lock_version"] != float64(2) {
		t.Errorf("a repeat must not change the record: lock_version = %v", body["lock_version"])
	}

	st, body = call(t, s, http.MethodPatch, "/v1/admin/senders/tracker", "1", map[string]any{"kinds": []string{"fix-error"}})
	expect(t, "another change, one version behind", st, http.StatusConflict, body)

	st, body = call(t, s, http.MethodPatch, "/v1/admin/senders/tracker", "1", map[string]any{
		"request_secret": request, "callback_secret": callback})
	expect(t, "the old secrets, one version behind", st, http.StatusConflict, body)

	st, body = call(t, s, http.MethodPatch, "/v1/admin/senders/tracker", "", change)
	expect(t, "no If-Match", st, http.StatusPreconditionRequired, body)

	// Apps too, and two versions behind is stale whatever the body.
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing"})
	expect(t, "create app", st, http.StatusCreated, body)
	for i, devs := range [][]string{{"a@example.com"}, {"b@example.com"}} {
		st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", strconv.Itoa(i+1), map[string]any{"developers": devs})
		expect(t, "app change", st, http.StatusOK, body)
	}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "2", map[string]any{"developers": []string{"b@example.com"}})
	expect(t, "app repeat, one behind", st, http.StatusOK, body)
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "1", map[string]any{"developers": []string{"b@example.com"}})
	expect(t, "two behind", st, http.StatusConflict, body)
}

func newSender(t *testing.T, s *Server, name string, extra map[string]any) (request, callback string) {
	t.Helper()
	request, callback = randomSecret(t), randomSecret(t)
	body := map[string]any{"name": name, "request_secret": request, "callback_secret": callback}
	for k, v := range extra {
		body[k] = v
	}
	st, resp := call(t, s, http.MethodPost, "/v1/admin/senders", "", body)
	expect(t, "create "+name, st, http.StatusCreated, resp)
	return request, callback
}

// A deleted sender that owes nothing becomes a tombstone: it reads as 404,
// its name and prefix stay taken, and only a create with both of its secrets
// and its prefix revives it, at the next lock_version.
func TestSender_TombstoneAndRevive(t *testing.T) {
	s := New(t)
	request, callback := newSender(t, s, "tracker", nil)
	st, body := call(t, s, http.MethodDelete, "/v1/admin/senders/tracker", "", nil)
	expect(t, "delete", st, http.StatusNoContent, body)
	if v, _ := s.Sender("tracker"); !v.Tombstone || v.RequestSecret != "" {
		t.Fatalf("want a tombstone with its secrets wiped: %+v", v)
	}
	st, body = call(t, s, http.MethodGet, "/v1/admin/senders/tracker", "", nil)
	expect(t, "read a tombstone", st, http.StatusNotFound, body)

	st, body = call(t, s, http.MethodPost, "/v1/admin/senders", "", map[string]any{
		"name": "tracker", "request_secret": randomSecret(t), "callback_secret": callback})
	expect(t, "revive with another secret", st, http.StatusConflict, body)
	if body["code"] != "name_retired" {
		t.Errorf("code = %v", body["code"])
	}
	st, body = call(t, s, http.MethodPost, "/v1/admin/senders", "", map[string]any{
		"name": "tracker", "branch_prefix": "elsewhere/", "request_secret": request, "callback_secret": callback})
	expect(t, "revive with another prefix", st, http.StatusConflict, body)
	st, body = call(t, s, http.MethodPost, "/v1/admin/senders", "", map[string]any{
		"name": "other", "branch_prefix": "tracker/", "request_secret": randomSecret(t), "callback_secret": randomSecret(t)})
	expect(t, "take the tombstone's prefix", st, http.StatusConflict, body)
	if body["code"] != "branch_prefix_taken" || body["field"] != "/branch_prefix" {
		t.Errorf("refusal = %v", body)
	}

	st, body = call(t, s, http.MethodPost, "/v1/admin/senders", "", map[string]any{
		"name": "tracker", "kinds": []string{"research"}, "request_secret": request, "callback_secret": callback})
	expect(t, "revive with both secrets and the prefix", st, http.StatusCreated, body)
	if body["lock_version"] != float64(2) {
		t.Errorf("lock_version = %v, want the next one", body["lock_version"])
	}
	if v, _ := s.Sender("tracker"); v.Retired || len(v.Kinds) != 1 || v.Kinds[0] != "research" {
		t.Errorf("revived as %+v, want the fields the create sent", v)
	}
}

// A retired sender still owing work comes back at once to a create with both
// of its secrets and its prefix, so terraform apply -replace has no gap.
// Other secrets get name_retired.
func TestSender_RetiredRevivesAtOnce(t *testing.T) {
	s := New(t)
	s.RetiredSendersOweWork(true)
	request, callback := newSender(t, s, "tracker", nil)
	call(t, s, http.MethodDelete, "/v1/admin/senders/tracker", "", nil)
	if v, _ := s.Sender("tracker"); !v.Retired || v.Tombstone {
		t.Fatalf("want a retired sender still owing work: %+v", v)
	}
	st, body := call(t, s, http.MethodPost, "/v1/admin/senders", "", map[string]any{
		"name": "tracker", "request_secret": request, "callback_secret": randomSecret(t)})
	expect(t, "other secrets", st, http.StatusConflict, body)
	st, body = call(t, s, http.MethodPost, "/v1/admin/senders", "", map[string]any{
		"name": "tracker", "request_secret": request, "callback_secret": callback})
	expect(t, "the same sender", st, http.StatusCreated, body)
	if body["lock_version"] != float64(2) {
		t.Errorf("lock_version = %v", body["lock_version"])
	}
}

// A branch prefix is set on create and never changes.
func TestPatch_BranchPrefixNeverChanges(t *testing.T) {
	s := New(t)
	newSender(t, s, "tracker", nil)
	for _, prefix := range []string{"elsewhere/", "tracker/"} {
		st, body := call(t, s, http.MethodPatch, "/v1/admin/senders/tracker", "1", map[string]any{"branch_prefix": prefix})
		expect(t, "a PATCH with "+prefix, st, http.StatusUnprocessableEntity, body)
		if body["field"] != "/branch_prefix" {
			t.Errorf("field = %v", body["field"])
		}
	}
}

// The default branch prefix is the name's, so its refusal points at /name.
func TestSender_DefaultPrefixTakenPointsAtName(t *testing.T) {
	s := New(t)
	newSender(t, s, "first", map[string]any{"branch_prefix": "second/"})
	st, body := call(t, s, http.MethodPost, "/v1/admin/senders", "", map[string]any{
		"name": "second", "request_secret": randomSecret(t), "callback_secret": randomSecret(t)})
	expect(t, "default prefix taken", st, http.StatusConflict, body)
	if body["code"] != "branch_prefix_taken" || body["field"] != "/name" {
		t.Errorf("refusal = %v", body)
	}
}

// No value ever serves both directions: a new secret can't be the sender's
// current secret for the other direction.
func TestSender_SecretCantServeBothDirections(t *testing.T) {
	s := New(t)
	request, callback := newSender(t, s, "tracker", nil)
	st, body := call(t, s, http.MethodPatch, "/v1/admin/senders/tracker", "1", map[string]any{
		"request_secret": callback, "callback_secret": randomSecret(t)})
	expect(t, "request secret = current callback secret", st, http.StatusUnprocessableEntity, body)
	if body["field"] != "/request_secret" {
		t.Errorf("field = %v", body["field"])
	}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/senders/tracker", "1", map[string]any{
		"request_secret": request, "callback_secret": callback})
	expect(t, "the same secrets again", st, http.StatusOK, body)
}

// An overlap that runs out on its own leaves lock_version as it is; ending
// it on request is a recorded change.
func TestSender_OverlapEnding(t *testing.T) {
	s := New(t)
	newSender(t, s, "tracker", nil)
	rotate := func(version string) {
		st, body := call(t, s, http.MethodPatch, "/v1/admin/senders/tracker", version, map[string]any{
			"request_secret": randomSecret(t), "callback_secret": randomSecret(t)})
		expect(t, "rotate", st, http.StatusOK, body)
		if body["previous_secrets_until"] == nil {
			t.Fatal("a rotation should start an overlap")
		}
	}
	rotate("1")
	s.ExpireOverlap("tracker")
	st, body := call(t, s, http.MethodGet, "/v1/admin/senders/tracker", "", nil)
	expect(t, "read", st, http.StatusOK, body)
	if body["previous_secrets_until"] != nil || body["lock_version"] != float64(2) {
		t.Errorf("after the overlap ran out: %v", body)
	}
	rotate("2")
	st, body = call(t, s, http.MethodDelete, "/v1/admin/senders/tracker/previous-secrets", "", nil)
	expect(t, "end the overlap", st, http.StatusNoContent, body)
	_, body = call(t, s, http.MethodGet, "/v1/admin/senders/tracker", "", nil)
	if body["previous_secrets_until"] != nil || body["lock_version"] != float64(4) {
		t.Errorf("after ending the overlap: %v", body)
	}
}

func TestIfMatch_BareNumberAndNonsense(t *testing.T) {
	s := New(t)
	call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing"})
	patch := func(ifMatch string) int {
		req, _ := http.NewRequest(http.MethodPatch, s.URL+"/v1/admin/apps/billing", strings.NewReader(`{"developers": []}`))
		req.Header.Set("Authorization", "Bearer "+s.Token())
		req.Header.Set("If-Match", ifMatch)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := patch("1"); got != http.StatusOK {
		t.Errorf("bare number: %d", got)
	}
	if got := patch(`"one"`); got != http.StatusConflict {
		t.Errorf("not a version: %d", got)
	}
}

// Under /v1/admin a route answers a method it doesn't take with 405 and
// Allow, and a path with no route with the not_found refusal.
func TestRoutes_MethodNotAllowedAndUnknownPath(t *testing.T) {
	s := New(t)
	req, _ := http.NewRequest(http.MethodPut, s.URL+"/v1/admin/senders/tracker", nil)
	req.Header.Set("Authorization", "Bearer "+s.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET, PATCH, DELETE" {
		t.Errorf("PUT: %d, Allow %q", resp.StatusCode, resp.Header.Get("Allow"))
	}
	st, body := call(t, s, http.MethodGet, "/v1/admin/nothing-here", "", nil)
	expect(t, "unknown admin path", st, http.StatusNotFound, body)
	if body["code"] != "not_found" {
		t.Errorf("code = %v", body["code"])
	}
}

func TestPost_RefusesReadOnlyFields(t *testing.T) {
	s := New(t)
	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "lock_version": 1})
	expect(t, "lock_version on create", st, http.StatusUnprocessableEntity, body)
	if message, _ := body["message"].(string); body["field"] != "/lock_version" || !strings.Contains(message, "read-only") {
		t.Errorf("refusal = %v", body)
	}
}

func TestUnauthorized_SaysBearer(t *testing.T) {
	s := New(t)
	req, _ := http.NewRequest(http.MethodGet, s.URL+"/v1/admin/apps", nil)
	req.Header.Set("Authorization", "bearer "+s.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the scheme is matched without regard to case: %d", resp.StatusCode)
	}
	req.Header.Set("Authorization", "Bearer wrong")
	if resp, err = http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("wrong token: %d, WWW-Authenticate %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
}
