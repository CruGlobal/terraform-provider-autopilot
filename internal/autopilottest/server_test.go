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

// Repositories are matched, and kept unique, without regard to case.
func TestRepos_KeptUniqueWithoutRegardToCase(t *testing.T) {
	s := New(t)
	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "repos": []string{"example-org/a", "Example-Org/A"}})
	expect(t, "create", st, http.StatusCreated, body)
	if v, _ := s.App("billing"); len(v.Repos) != 1 {
		t.Errorf("repos = %v, want one", v.Repos)
	}
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "payroll", "repos": []string{"EXAMPLE-ORG/a"}})
	expect(t, "a second app taking the same repository", st, http.StatusConflict, body)
	if body["code"] != "repo_taken" {
		t.Errorf("code = %v", body["code"])
	}
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
