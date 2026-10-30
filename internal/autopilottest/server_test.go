package autopilottest

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
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
	// Sorted by byte, another repository falls between the two.
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "repos": []string{"ACME/x", "Zed/y", "acme/x"}})
	expect(t, "one repository in two cases, apart once sorted", st, http.StatusUnprocessableEntity, body)
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

// can_auto_merge is true only with a kind that opens a pull request, is kept
// and answered, and two entries for one sender that differ only in it are
// refused.
func TestAccepts_CanAutoMerge(t *testing.T) {
	s := New(t)
	for _, kinds := range [][]string{{"research"}, {"review-pr"}} {
		entry := map[string]any{"sender": "tracker", "kinds": kinds, "can_auto_merge": true}
		st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "accepts": []any{entry}})
		expect(t, "can_auto_merge with "+kinds[0], st, http.StatusUnprocessableEntity, body)
		if body["field"] != "/accepts/0/can_auto_merge" {
			t.Errorf("%s: field = %v", kinds[0], body["field"])
		}
	}
	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "accepts": []any{
		map[string]any{"sender": "tracker", "kinds": []string{"implement-work-item"}, "can_auto_merge": "yes"},
	}})
	expect(t, "can_auto_merge not a flag", st, http.StatusUnprocessableEntity, body)
	// null is not false: AutoPilot refuses it, for both flags.
	for _, flag := range []string{"can_auto_merge", "can_decide"} {
		st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "accepts": []any{
			map[string]any{"sender": "tracker", "kinds": []string{"implement-work-item", "review-pr"}, flag: nil},
		}})
		expect(t, flag+" null", st, http.StatusUnprocessableEntity, body)
		if body["field"] != "/accepts/0/"+flag {
			t.Errorf("%s null: field = %v", flag, body["field"])
		}
	}

	on := map[string]any{"sender": "tracker", "kinds": []string{"research", "fix-error"}, "can_auto_merge": true}
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "accepts": []any{on}})
	expect(t, "can_auto_merge with fix-error", st, http.StatusCreated, body)
	if v, _ := s.App("billing"); len(v.Accepts) != 1 || !v.Accepts[0].CanAutoMerge {
		t.Errorf("accepts = %+v, want can_auto_merge kept", v.Accepts)
	}
	accepts, _ := body["accepts"].([]any)
	if len(accepts) != 1 {
		t.Fatalf("accepts = %v", accepts)
	}
	if first, _ := accepts[0].(map[string]any); first["can_auto_merge"] != true {
		t.Errorf("accepts = %v, want can_auto_merge answered", accepts)
	}

	off := map[string]any{"sender": "tracker", "kinds": []string{"research", "fix-error"}}
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "payroll", "accepts": []any{on, off}})
	expect(t, "two entries for one sender, apart in can_auto_merge", st, http.StatusUnprocessableEntity, body)
}

// can_decide_own_pr is true only with can_decide true and only for a sender
// built into AutoPilot, is false unless sent, is kept and answered, and two
// entries for one sender that differ only in it are refused.
func TestAccepts_CanDecideOwnPR(t *testing.T) {
	s := New(t)
	refused := []struct {
		name  string
		entry map[string]any
	}{
		{"without can_decide", map[string]any{"sender": "autopilot-mcp", "kinds": []string{"review-pr"}, "can_decide_own_pr": true}},
		{"with can_decide false", map[string]any{"sender": "autopilot-github", "kinds": []string{"review-pr"},
			"can_decide": false, "can_decide_own_pr": true}},
		{"for a sender in the ledger", map[string]any{"sender": "tracker", "kinds": []string{"review-pr"},
			"can_decide": true, "can_decide_own_pr": true}},
		{"not a flag", map[string]any{"sender": "autopilot-mcp", "kinds": []string{"review-pr"},
			"can_decide": true, "can_decide_own_pr": "yes"}},
		{"null", map[string]any{"sender": "autopilot-mcp", "kinds": []string{"review-pr"},
			"can_decide": true, "can_decide_own_pr": nil}},
	}
	for _, c := range refused {
		st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "accepts": []any{c.entry}})
		expect(t, "can_decide_own_pr "+c.name, st, http.StatusUnprocessableEntity, body)
		if body["field"] != "/accepts/0/can_decide_own_pr" {
			t.Errorf("%s: field = %v", c.name, body["field"])
		}
	}

	on := func(sender string) map[string]any {
		return map[string]any{"sender": sender, "kinds": []string{"review-pr"}, "can_decide": true, "can_decide_own_pr": true}
	}
	plain := map[string]any{"sender": "tracker", "kinds": []string{"review-pr"}, "can_decide": true}
	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "accepts": []any{
		on("autopilot-github"), on("autopilot-mcp"), plain,
	}})
	expect(t, "can_decide_own_pr for the built-in senders", st, http.StatusCreated, body)
	v, _ := s.App("billing")
	if len(v.Accepts) != 3 || !v.Accepts[0].CanDecideOwnPR || !v.Accepts[1].CanDecideOwnPR || v.Accepts[2].CanDecideOwnPR {
		t.Errorf("accepts = %+v, want can_decide_own_pr kept for the built-in senders only", v.Accepts)
	}
	accepts, _ := body["accepts"].([]any)
	if len(accepts) != 3 {
		t.Fatalf("accepts = %v", accepts)
	}
	for i, want := range []any{true, true, false} {
		if entry, _ := accepts[i].(map[string]any); entry["can_decide_own_pr"] != want {
			t.Errorf("accepts[%d] = %v, want can_decide_own_pr %v answered", i, entry, want)
		}
	}

	// false is taken anywhere: without can_decide, and for a sender in the
	// ledger.
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "ledger", "accepts": []any{
		map[string]any{"sender": "autopilot-mcp", "kinds": []string{"review-pr"}, "can_decide_own_pr": false},
		map[string]any{"sender": "tracker", "kinds": []string{"research"}, "can_decide_own_pr": false},
	}})
	expect(t, "can_decide_own_pr false", st, http.StatusCreated, body)

	off := map[string]any{"sender": "autopilot-mcp", "kinds": []string{"review-pr"}, "can_decide": true}
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "payroll", "accepts": []any{on("autopilot-mcp"), off}})
	expect(t, "two entries for one sender, apart in can_decide_own_pr", st, http.StatusUnprocessableEntity, body)
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "payroll", "accepts": []any{on("autopilot-mcp"), on("autopilot-mcp")}})
	expect(t, "one entry sent twice exactly", st, http.StatusCreated, body)
}

// approve_up_to is low, medium, high or null, set only with can_decide true,
// for any sender; it is kept and answered, null when there is none; and two
// entries for one sender that differ only in it are refused.
func TestAccepts_ApproveUpTo(t *testing.T) {
	s := New(t)
	refused := []struct {
		name  string
		entry map[string]any
	}{
		{"without can_decide", map[string]any{"sender": "autopilot-github", "kinds": []string{"review-pr"}, "approve_up_to": "low"}},
		{"with can_decide false", map[string]any{"sender": "tracker", "kinds": []string{"review-pr"},
			"can_decide": false, "approve_up_to": "high"}},
		{"not a level", map[string]any{"sender": "autopilot-github", "kinds": []string{"review-pr"},
			"can_decide": true, "approve_up_to": "none"}},
		{"in another case", map[string]any{"sender": "autopilot-github", "kinds": []string{"review-pr"},
			"can_decide": true, "approve_up_to": "Low"}},
		{"not a string", map[string]any{"sender": "autopilot-github", "kinds": []string{"review-pr"},
			"can_decide": true, "approve_up_to": 1}},
	}
	for _, c := range refused {
		st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "accepts": []any{c.entry}})
		expect(t, "approve_up_to "+c.name, st, http.StatusUnprocessableEntity, body)
		if body["field"] != "/accepts/0/approve_up_to" {
			t.Errorf("%s: field = %v", c.name, body["field"])
		}
	}

	capped := func(sender, ceiling string) map[string]any {
		return map[string]any{"sender": sender, "kinds": []string{"review-pr"}, "can_decide": true, "approve_up_to": ceiling}
	}
	plain := map[string]any{"sender": "monitor", "kinds": []string{"review-pr"}, "can_decide": true}
	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "accepts": []any{
		capped("autopilot-github", "low"), capped("tracker", "medium"), plain,
	}})
	expect(t, "approve_up_to for any deciding sender", st, http.StatusCreated, body)
	v, _ := s.App("billing")
	if len(v.Accepts) != 3 || v.Accepts[0].ApproveUpTo != "low" || v.Accepts[1].ApproveUpTo != "" || v.Accepts[2].ApproveUpTo != "medium" {
		t.Errorf("accepts = %+v, want each ceiling kept", v.Accepts)
	}
	accepts, _ := body["accepts"].([]any)
	if len(accepts) != 3 {
		t.Fatalf("accepts = %v", accepts)
	}
	for i, want := range []any{"low", nil, "medium"} {
		if entry, _ := accepts[i].(map[string]any); entry["approve_up_to"] != want {
			t.Errorf("accepts[%d] = %v, want approve_up_to %v answered", i, entry, want)
		}
	}

	// null is no ceiling, taken anywhere, even without can_decide.
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "ledger", "accepts": []any{
		map[string]any{"sender": "autopilot-mcp", "kinds": []string{"review-pr"}, "approve_up_to": nil},
		map[string]any{"sender": "tracker", "kinds": []string{"research"}, "approve_up_to": nil},
	}})
	expect(t, "approve_up_to null", st, http.StatusCreated, body)

	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "payroll", "accepts": []any{
		capped("autopilot-mcp", "low"), capped("autopilot-mcp", "high"),
	}})
	expect(t, "two entries for one sender, apart in approve_up_to", st, http.StatusUnprocessableEntity, body)
	if body["field"] != "/accepts/1/sender" {
		t.Errorf("two entries for one sender: field = %v", body["field"])
	}
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "payroll", "accepts": []any{
		capped("autopilot-mcp", "low"), capped("autopilot-mcp", "low"),
	}})
	expect(t, "one entry sent twice exactly", st, http.StatusCreated, body)
}

func TestDevelopers_LowercaseASCIIOnly(t *testing.T) {
	s := New(t)
	for _, bad := range []string{
		"Someone@example.com", "someone@Example.com", "someoné@example.com", "someone", "some one@example.com",
		// AutoPilot's own rule refuses these too.
		"someone@example.com.", "someone@.example.com", "someone@example..com", "someone@example",
		"some!one@example.com", "some,one@example.com", "{someone}@example.com", "@example.com",
		strings.Repeat("a", 65) + "@example.com", "someone@example.com@example.com",
	} {
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
// and its prefix revives it. Retiring it, making the tombstone and reviving it
// each raise lock_version, as AutoPilot does.
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
	if body["lock_version"] != float64(4) {
		t.Errorf("lock_version = %v, want 4: created, retired, made a tombstone, revived", body["lock_version"])
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
	if body["lock_version"] != float64(3) {
		t.Errorf("lock_version = %v, want 3: created, retired, revived", body["lock_version"])
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

// The credential policy is one: a write that sends credential sets both
// fields, when_seats_full goes with seat_first only, and alone it is refused.
func TestCredentialPolicy_Rules(t *testing.T) {
	s := New(t)
	refused := []struct {
		fields map[string]any
		field  string
	}{
		{map[string]any{"credential": "seats"}, "/credential"},
		{map[string]any{"credential": 1}, "/credential"},
		{map[string]any{"credential": "seat_first"}, "/when_seats_full"},
		{map[string]any{"credential": "seat_first", "when_seats_full": nil}, "/when_seats_full"},
		{map[string]any{"credential": "seat_first", "when_seats_full": "later"}, "/when_seats_full"},
		{map[string]any{"credential": "bedrock", "when_seats_full": "wait"}, "/when_seats_full"},
		{map[string]any{"credential": nil, "when_seats_full": "fall_back"}, "/when_seats_full"},
		{map[string]any{"when_seats_full": "wait"}, "/when_seats_full"},
		{map[string]any{"when_seats_full": nil}, "/when_seats_full"},
	}
	for _, c := range refused {
		body := map[string]any{"name": "billing"}
		for k, v := range c.fields {
			body[k] = v
		}
		st, answer := call(t, s, http.MethodPost, "/v1/admin/apps", "", body)
		expect(t, fmt.Sprint(c.fields), st, http.StatusUnprocessableEntity, answer)
		if answer["field"] != c.field {
			t.Errorf("%v: field = %v, want %s", c.fields, answer["field"], c.field)
		}
	}

	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{
		"name": "billing", "credential": "seat_first", "when_seats_full": "wait",
	})
	expect(t, "seat_first and wait", st, http.StatusCreated, body)
	if body["credential"] != "seat_first" || body["when_seats_full"] != "wait" {
		t.Errorf("created %v", body)
	}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "1", map[string]any{"credential": "bedrock"})
	expect(t, "bedrock alone", st, http.StatusOK, body)
	if body["credential"] != "bedrock" || body["when_seats_full"] != nil {
		t.Errorf("changed to %v", body)
	}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "2", map[string]any{"developers": []string{}})
	expect(t, "another field", st, http.StatusOK, body)
	if body["credential"] != "bedrock" {
		t.Errorf("a change without the policy took it: %v", body)
	}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "2", map[string]any{"credential": nil})
	expect(t, "taken away", st, http.StatusOK, body)
	if body["credential"] != nil || body["lock_version"] != float64(3) {
		t.Errorf("taken away: %v", body)
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

func TestWorker_Rules(t *testing.T) {
	s := New(t)
	refused := []struct {
		fields map[string]any
		field  string
	}{
		{map[string]any{"worker_image": "rust"}, "/worker_image"},
		{map[string]any{"worker_image": "Node"}, "/worker_image"},
		{map[string]any{"worker_image": "ruby_browser"}, "/worker_image"},
		{map[string]any{"worker_image": 1}, "/worker_image"},
		{map[string]any{"worker_cpu": 4096}, "/worker_memory"},
		{map[string]any{"worker_memory": 16384}, "/worker_cpu"},
		{map[string]any{"worker_cpu": 4096, "worker_memory": nil}, "/worker_memory"},
		{map[string]any{"worker_cpu": "4096", "worker_memory": 16384}, "/worker_cpu"},
		{map[string]any{"worker_cpu": 4096.5, "worker_memory": 16384}, "/worker_cpu"},
		{map[string]any{"worker_cpu": -4096, "worker_memory": 16384}, "/worker_cpu"},
		{map[string]any{"worker_cpu": 0, "worker_memory": 0}, "/worker_cpu"},
		{map[string]any{"worker_cpu": 8192, "worker_memory": 16384}, "/worker_cpu"},
		{map[string]any{"worker_cpu": 4096, "worker_memory": 17408}, "/worker_memory"},
		{map[string]any{"worker_cpu": 4096, "worker_memory": 4096}, "/worker_memory"},
		{map[string]any{"worker_cpu": 256, "worker_memory": 4096}, "/worker_memory"},
	}
	for _, c := range refused {
		body := map[string]any{"name": "billing"}
		for k, v := range c.fields {
			body[k] = v
		}
		st, answer := call(t, s, http.MethodPost, "/v1/admin/apps", "", body)
		expect(t, fmt.Sprint(c.fields), st, http.StatusUnprocessableEntity, answer)
		if answer["field"] != c.field {
			t.Errorf("%v: field = %v, want %s", c.fields, answer["field"], c.field)
		}
	}

	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{
		"name": "billing", "worker_image": "ruby", "worker_cpu": 4096, "worker_memory": 16384,
	})
	expect(t, "ruby at 4 vCPU and 16 GB", st, http.StatusCreated, body)
	if body["worker_image"] != "ruby" || body["worker_cpu"] != float64(4096) || body["worker_memory"] != float64(16384) {
		t.Errorf("created %v", body)
	}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "1", map[string]any{"developers": []string{}})
	expect(t, "another field", st, http.StatusOK, body)
	if body["worker_image"] != "ruby" || body["worker_cpu"] != float64(4096) {
		t.Errorf("a change without the worker took it: %v", body)
	}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "1", map[string]any{"worker_cpu": nil})
	expect(t, "size taken away", st, http.StatusOK, body)
	if body["worker_cpu"] != nil || body["worker_memory"] != nil || body["worker_image"] != "ruby" || body["lock_version"] != float64(2) {
		t.Errorf("size taken away: %v", body)
	}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "2", map[string]any{"worker_image": "ruby-browser"})
	expect(t, "the image with a browser", st, http.StatusOK, body)
	if body["worker_image"] != "ruby-browser" || body["lock_version"] != float64(3) {
		t.Errorf("the image with a browser: %v", body)
	}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "3", map[string]any{"worker_image": nil})
	expect(t, "back to node", st, http.StatusOK, body)
	if body["worker_image"] != nil || body["lock_version"] != float64(4) {
		t.Errorf("back to node: %v", body)
	}
}

// GitHub logins: each a lowercase GitHub login paired with one of the app's
// developers, each login at most once, at most 100, kept sorted by login.
func TestGitHubLogins_Rules(t *testing.T) {
	s := New(t)
	dev := []string{"alice@example.org"}
	pair := func(login, developer string) map[string]any {
		return map[string]any{"login": login, "developer": developer}
	}
	refused := []struct {
		what   string
		body   map[string]any
		field  string
		wanted string
	}{
		{"uppercase", map[string]any{"github_logins": []any{pair("Alice", "alice@example.org")}}, "/github_logins/0/login", ""},
		{"a leading -", map[string]any{"github_logins": []any{pair("-alice", "alice@example.org")}}, "/github_logins/0/login", ""},
		{"a leading _", map[string]any{"github_logins": []any{pair("_alice", "alice@example.org")}}, "/github_logins/0/login", ""},
		{"a dot", map[string]any{"github_logins": []any{pair("alice.gh", "alice@example.org")}}, "/github_logins/0/login", ""},
		{"40 characters", map[string]any{"github_logins": []any{pair(strings.Repeat("a", 40), "alice@example.org")}}, "/github_logins/0/login", ""},
		{"empty", map[string]any{"github_logins": []any{pair("", "alice@example.org")}}, "/github_logins/0/login", ""},
		{"a developer in uppercase", map[string]any{"github_logins": []any{pair("alice", "Alice@example.org")}}, "/github_logins/0/developer", ""},
		{"a developer not an email", map[string]any{"github_logins": []any{pair("alice", "alice")}}, "/github_logins/0/developer", ""},
		{"no login", map[string]any{"github_logins": []any{map[string]any{"developer": "alice@example.org"}}}, "/github_logins/0/login", ""},
		{"no developer", map[string]any{"github_logins": []any{map[string]any{"login": "alice"}}}, "/github_logins/0/developer", ""},
		{"an unknown field", map[string]any{"github_logins": []any{map[string]any{"login": "alice", "developer": "alice@example.org", "admin": true}}}, "/github_logins/0/admin", ""},
		{"not a list", map[string]any{"github_logins": "alice"}, "/github_logins", ""},
		{"not an object", map[string]any{"github_logins": []any{"alice"}}, "/github_logins/0", ""},
		// AutoPilot reads an entry's fields in sorted order, developer first.
		{"both wrong", map[string]any{"github_logins": []any{pair("BAD", "BAD")}}, "/github_logins/0/developer", ""},
		{"a developer AutoPilot's rule refuses", map[string]any{"developers": []string{"alice@example.org"},
			"github_logins": []any{pair("alice", "alice@example.org.")}}, "/github_logins/0/developer", ""},
		// It reads every field before it pairs logins with developers.
		{"unpaired, and another field wrong", map[string]any{"worker_image": "bogus",
			"github_logins": []any{pair("alice", "alice@example.org")}}, "/worker_image", ""},
		{"one login, two developers", map[string]any{"developers": []string{"alice@example.org", "bob@example.org"},
			"github_logins": []any{pair("alice", "alice@example.org"), pair("alice", "bob@example.org")}}, "/github_logins/1/login", ""},
		{"no developers", map[string]any{"github_logins": []any{pair("alice", "alice@example.org")}}, "/github_logins/0", "developer the app doesn't list"},
		{"someone not a developer", map[string]any{"developers": dev,
			"github_logins": []any{pair("alice", "alice@example.org"), pair("bob", "bob@example.org")}}, "/github_logins/1", "developer the app doesn't list"},
	}
	for _, c := range refused {
		body := map[string]any{"name": "billing"}
		for k, v := range c.body {
			body[k] = v
		}
		st, answer := call(t, s, http.MethodPost, "/v1/admin/apps", "", body)
		expect(t, c.what, st, http.StatusUnprocessableEntity, answer)
		if answer["field"] != c.field {
			t.Errorf("%s: field = %v, want %s", c.what, answer["field"], c.field)
		}
		if msg, _ := answer["message"].(string); !strings.Contains(msg, c.wanted) {
			t.Errorf("%s: message = %q", c.what, msg)
		}
	}
	many := make([]any, 0, 101)
	for i := range 101 {
		many = append(many, pair(fmt.Sprintf("login-%d", i), "alice@example.org"))
	}
	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "developers": dev, "github_logins": many})
	expect(t, "101 logins", st, http.StatusUnprocessableEntity, body)
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "developers": dev, "github_logins": many[:100]})
	expect(t, "100 logins", st, http.StatusCreated, body)
	s.RemoveApp("billing")

	// Kept sorted by login, an entry sent twice exactly once.
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{
		"name": "billing", "developers": []string{"alice@example.org", "bob@example.org"},
		"github_logins": []any{pair("bob_work", "bob@example.org"), pair(strings.Repeat("a", 39), "alice@example.org"), pair("bob_work", "bob@example.org"), pair("0bob", "bob@example.org")},
	})
	expect(t, "create", st, http.StatusCreated, body)
	logins, _ := body["github_logins"].([]any)
	if len(logins) != 3 {
		t.Fatalf("github_logins = %v", body["github_logins"])
	}
	if first, _ := logins[0].(map[string]any); first["login"] != "0bob" || first["developer"] != "bob@example.org" {
		t.Errorf("github_logins = %v, want sorted by login", logins)
	}

	// A create for the name with other logins is not a retry of the one
	// that made it; with the same ones, it is.
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{
		"name": "billing", "developers": []string{"alice@example.org", "bob@example.org"},
		"github_logins": []any{pair("0bob", "bob@example.org")},
	})
	expect(t, "a create with other logins", st, http.StatusConflict, body)
	if body["code"] != "name_taken" {
		t.Errorf("code = %v", body["code"])
	}
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{
		"name": "billing", "developers": []string{"alice@example.org", "bob@example.org"},
		"github_logins": []any{pair("0bob", "bob@example.org"), pair("bob_work", "bob@example.org"), pair(strings.Repeat("a", 39), "alice@example.org")},
	})
	expect(t, "the same create again", st, http.StatusOK, body)

	// A change that sends one of the two is checked against the record.
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "1", map[string]any{"github_logins": []any{pair("carol", "carol@example.org")}})
	expect(t, "a login for someone not a developer", st, http.StatusUnprocessableEntity, body)
	if body["field"] != "/github_logins" {
		t.Errorf("field = %v", body["field"])
	}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "1", map[string]any{"developers": []string{"alice@example.org"}})
	expect(t, "a developer taken out who still has logins", st, http.StatusUnprocessableEntity, body)
	if body["field"] != "/developers" {
		t.Errorf("field = %v", body["field"])
	}
	// One that sends both is checked at the entry.
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "1", map[string]any{
		"developers": []string{"alice@example.org"}, "github_logins": []any{pair("alice", "alice@example.org"), pair("bob", "bob@example.org")},
	})
	expect(t, "both, one unpaired", st, http.StatusUnprocessableEntity, body)
	if body["field"] != "/github_logins/1" {
		t.Errorf("field = %v", body["field"])
	}
	// Taking a developer and their logins out in one change is fine.
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "1", map[string]any{
		"developers": []string{"alice@example.org"}, "github_logins": []any{pair("alice", "alice@example.org")},
	})
	expect(t, "a developer and their logins out", st, http.StatusOK, body)
	if body["lock_version"] != float64(2) {
		t.Errorf("lock_version = %v", body["lock_version"])
	}

	// A record left with a login for someone gone, as an older AutoPilot
	// could leave it, still takes a change that sends neither.
	s.ChangeAppOutOfBand("billing", func(v *AppView) { v.Developers = nil })
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "3", map[string]any{"repos": []string{"example-org/billing"}})
	expect(t, "a change that sends neither", st, http.StatusOK, body)
}

// An app may let reviews started from GitHub decide, as it may the MCP's,
// and no sender in the ledger may take autopilot-github's name.
func TestGitHubSender_MayDecide(t *testing.T) {
	s := New(t)
	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "accepts": []any{
		map[string]any{"sender": "autopilot-github", "kinds": []string{"review-pr"}, "can_decide": true},
	}})
	expect(t, "can_decide for autopilot-github", st, http.StatusCreated, body)
	if v, _ := s.App("billing"); len(v.Accepts) != 1 || !v.Accepts[0].CanDecide {
		t.Errorf("accepts = %+v, want can_decide kept", v.Accepts)
	}
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "payroll", "accepts": []any{
		map[string]any{"sender": "autopilot-github", "kinds": []string{"review-pr"}},
	}})
	expect(t, "autopilot-github for review-pr", st, http.StatusCreated, body)
	st, body = call(t, s, http.MethodPost, "/v1/admin/senders", "", map[string]any{
		"name": "autopilot-github", "request_secret": randomSecret(t), "callback_secret": randomSecret(t)})
	expect(t, "a sender named autopilot-github", st, http.StatusConflict, body)
	if body["code"] != "name_reserved" {
		t.Errorf("code = %v", body["code"])
	}
	// Nor the branch prefix of a sender built into AutoPilot.
	for _, prefix := range []string{"autopilot-github/", "mcp/"} {
		st, body = call(t, s, http.MethodPost, "/v1/admin/senders", "", map[string]any{
			"name": "tracker", "branch_prefix": prefix, "request_secret": randomSecret(t), "callback_secret": randomSecret(t)})
		expect(t, "the branch prefix "+prefix, st, http.StatusConflict, body)
		if body["code"] != "branch_prefix_taken" || body["field"] != "/branch_prefix" {
			t.Errorf("%s: code = %v, field = %v", prefix, body["code"], body["field"])
		}
	}
}

// auto_review is refused where AutoPilot refuses it, at the field AutoPilot
// names, fields read in sorted order.
func TestAutoReview_Refused(t *testing.T) {
	s := New(t)
	cases := []struct {
		auto  any
		field string
	}{
		{true, "/auto_review"},
		{[]any{}, "/auto_review"},
		{"on", "/auto_review"},
		{map[string]any{"events": []string{}}, "/auto_review/events"},
		{map[string]any{"events": "opened"}, "/auto_review/events"},
		{map[string]any{"events": nil}, "/auto_review/events"},
		{map[string]any{"events": []any{"opened", "pushed"}}, "/auto_review/events/1"},
		{map[string]any{"events": []any{"Opened"}}, "/auto_review/events/0"},
		{map[string]any{"events": []any{"reopened"}}, "/auto_review/events/0"},
		{map[string]any{"events": []any{1}}, "/auto_review/events/0"},
		{map[string]any{"max_per_day": 0}, "/auto_review/max_per_day"},
		{map[string]any{"max_per_day": 201}, "/auto_review/max_per_day"},
		{map[string]any{"max_per_day": 1.5}, "/auto_review/max_per_day"},
		{map[string]any{"max_per_day": "50"}, "/auto_review/max_per_day"},
		{map[string]any{"max_per_day": nil}, "/auto_review/max_per_day"},
		{map[string]any{"can_decide": "yes"}, "/auto_review/can_decide"},
		{map[string]any{"can_decide": nil}, "/auto_review/can_decide"},
		{map[string]any{"can_decide_own_pr": true}, "/auto_review/can_decide_own_pr"},
		{map[string]any{"can_decide": false, "can_decide_own_pr": true}, "/auto_review/can_decide_own_pr"},
		{map[string]any{"approve_up_to": "low"}, "/auto_review/approve_up_to"},
		{map[string]any{"can_decide": true, "approve_up_to": "none"}, "/auto_review/approve_up_to"},
		{map[string]any{"can_decide": true, "approve_up_to": "LOW"}, "/auto_review/approve_up_to"},
		{map[string]any{"name": "auto-review:billing"}, "/auto_review/name"},
		// An unknown key that isn't a plain word is pointed at through the object.
		{map[string]any{"a b": 1}, "/auto_review"},
		// Read right after accepts, before the fields that sort after it.
		{map[string]any{"max_per_day": 0, "zz": 1}, "/auto_review/max_per_day"},
		// The first wrong field in sorted order is the one named.
		{map[string]any{"max_per_day": 0, "events": "opened"}, "/auto_review/events"},
		// No event is a wrong field only once every field is read.
		{map[string]any{"max_per_day": 0, "events": []string{}}, "/auto_review/max_per_day"},
	}
	for _, c := range cases {
		st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "auto_review": c.auto})
		expect(t, fmt.Sprintf("auto_review %v", c.auto), st, http.StatusUnprocessableEntity, body)
		if body["field"] != c.field {
			t.Errorf("auto_review %v: field = %v, want %s", c.auto, body["field"], c.field)
		}
	}
}

// auto_review is null unless set; sent, each field it leaves out is its
// default, the events kept once each in AutoPilot's order; a change that
// leaves it out keeps it, and null takes it away. A create with it is a
// retry only with the same.
func TestAutoReview_Kept(t *testing.T) {
	s := New(t)
	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing"})
	expect(t, "create without it", st, http.StatusCreated, body)
	if body["auto_review"] != nil {
		t.Errorf("auto_review = %v, want null", body["auto_review"])
	}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "1", map[string]any{"auto_review": map[string]any{}})
	expect(t, "every default", st, http.StatusOK, body)
	want := map[string]any{
		"events": []any{"opened", "ready_for_review"}, "workflows": []any{}, "max_per_day": float64(50),
		"can_decide": false, "can_decide_own_pr": false, "approve_up_to": nil,
	}
	if got, _ := json.Marshal(body["auto_review"]); string(got) != mustJSON(want) {
		t.Errorf("auto_review = %s, want %s", got, mustJSON(want))
	}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "2", map[string]any{"auto_review": map[string]any{
		"events": []string{"synchronize", "opened", "synchronize"}, "max_per_day": 200, "can_decide": true,
		"can_decide_own_pr": true, "approve_up_to": "medium",
	}})
	expect(t, "every field", st, http.StatusOK, body)
	v, _ := s.App("billing")
	if v.AutoReview == nil || strings.Join(v.AutoReview.Events, ",") != "opened,synchronize" || v.AutoReview.MaxPerDay != 200 ||
		!v.AutoReview.CanDecide || !v.AutoReview.CanDecideOwnPR || v.AutoReview.ApproveUpTo != "medium" || v.LockVersion != 3 {
		t.Errorf("app = %+v, auto_review = %+v", v, v.AutoReview)
	}
	// It is one setting: a field left out is its default again.
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "3", map[string]any{"auto_review": map[string]any{"events": []string{"opened"}}})
	expect(t, "events alone", st, http.StatusOK, body)
	if v, _ = s.App("billing"); v.AutoReview == nil || v.AutoReview.CanDecide || v.AutoReview.MaxPerDay != 50 {
		t.Errorf("auto_review = %+v, want the defaults but events", v.AutoReview)
	}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "4", map[string]any{"developers": []string{}})
	expect(t, "a change without it", st, http.StatusOK, body)
	if v, _ = s.App("billing"); v.AutoReview == nil {
		t.Errorf("a change that leaves auto_review out took it away")
	}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "4", map[string]any{"auto_review": nil})
	expect(t, "null", st, http.StatusOK, body)
	if v, _ = s.App("billing"); v.AutoReview != nil || body["auto_review"] != nil || v.LockVersion != 5 {
		t.Errorf("auto_review = %+v after null, lock_version %d", v.AutoReview, v.LockVersion)
	}

	made := map[string]any{"name": "ledger", "auto_review": map[string]any{"max_per_day": 10}}
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", made)
	expect(t, "create with it", st, http.StatusCreated, body)
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", made)
	expect(t, "the same create again", st, http.StatusOK, body)
	for _, other := range []map[string]any{
		{"name": "ledger"},
		{"name": "ledger", "auto_review": map[string]any{}},
	} {
		st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", other)
		expect(t, fmt.Sprintf("create %v", other), st, http.StatusConflict, body)
	}
}

// auto_review's workflows are refused where AutoPilot refuses them, at the
// field AutoPilot names: each one at its place, the list as a whole, and the
// events beside them.
func TestAutoReview_WorkflowsRefused(t *testing.T) {
	s := New(t)
	const file = "example-org/billing-api/.github/workflows/autopilot-review.yml"
	main := file + "@refs/heads/main"
	cases := []struct {
		auto  map[string]any
		field string
	}{
		// No event and no workflow: nothing could start one.
		{map[string]any{"events": []string{}, "workflows": []string{}}, "/auto_review/events"},
		{map[string]any{"events": []string{}}, "/auto_review/events"},
		// Workflows with the events left out, which would default to reviews
		// on their own before any workflow asks.
		{map[string]any{"workflows": []string{main}}, "/auto_review/events"},
		{map[string]any{"workflows": []string{main}, "max_per_day": 5}, "/auto_review/events"},
		{map[string]any{"workflows": "x"}, "/auto_review/workflows"},
		{map[string]any{"workflows": nil}, "/auto_review/workflows"},
		{map[string]any{"workflows": map[string]any{}}, "/auto_review/workflows"},
		{map[string]any{"workflows": []any{1}}, "/auto_review/workflows/0"},
		{map[string]any{"workflows": []any{main, nil}}, "/auto_review/workflows/1"},
		// Every item must be a string before any is read as a workflow.
		{map[string]any{"workflows": []any{"nonsense", 1}}, "/auto_review/workflows/1"},
		// A pull request's ref, a tag, a commit, or no branch.
		{map[string]any{"workflows": []string{file + "@refs/pull/5/merge"}}, "/auto_review/workflows/0"},
		{map[string]any{"workflows": []string{file + "@refs/tags/v1"}}, "/auto_review/workflows/0"},
		{map[string]any{"workflows": []string{file + "@4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d9c8b7a6f5e"}}, "/auto_review/workflows/0"},
		{map[string]any{"workflows": []string{file}}, "/auto_review/workflows/0"},
		{map[string]any{"workflows": []string{file + "@main"}}, "/auto_review/workflows/0"},
		{map[string]any{"workflows": []string{file + "@refs/heads/"}}, "/auto_review/workflows/0"},
		// Not straight in .github/workflows, not YAML, or a repository git
		// or GitHub wouldn't name.
		{map[string]any{"workflows": []string{"example-org/billing-api/.github/workflows/sub/r.yml@refs/heads/main"}}, "/auto_review/workflows/0"},
		{map[string]any{"workflows": []string{"example-org/billing-api/.github/actions/r.yml@refs/heads/main"}}, "/auto_review/workflows/0"},
		{map[string]any{"workflows": []string{"example-org/billing-api/.github/workflows/r.json@refs/heads/main"}}, "/auto_review/workflows/0"},
		{map[string]any{"workflows": []string{"example-org/billing-api/.github/workflows/.yml@refs/heads/main"}}, "/auto_review/workflows/0"},
		{map[string]any{"workflows": []string{"example-org/../.github/workflows/r.yml@refs/heads/main"}}, "/auto_review/workflows/0"},
		{map[string]any{"workflows": []string{"example-org/./.github/workflows/r.yml@refs/heads/main"}}, "/auto_review/workflows/0"},
		{map[string]any{"workflows": []string{"example_org/billing-api/.github/workflows/r.yml@refs/heads/main"}}, "/auto_review/workflows/0"},
		{map[string]any{"workflows": []string{strings.Repeat("a", 40) + "/r/.github/workflows/r.yml@refs/heads/main"}}, "/auto_review/workflows/0"},
		{map[string]any{"workflows": []string{"o/" + strings.Repeat("r", 101) + "/.github/workflows/r.yml@refs/heads/main"}}, "/auto_review/workflows/0"},
		{map[string]any{"workflows": []string{"o/r/.github/workflows/" + strings.Repeat("f", 201) + ".yml@refs/heads/main"}}, "/auto_review/workflows/0"},
		{map[string]any{"workflows": []string{"o/r/.github/workflows/r.yml@refs/heads/" + strings.Repeat("b", 201)}}, "/auto_review/workflows/0"},
		{map[string]any{"workflows": []string{main + "\n"}}, "/auto_review/workflows/0"},
		// The same workflow twice, its repository in another case.
		{map[string]any{"workflows": []string{main, "Example-Org/Billing-API/.github/workflows/autopilot-review.yml@refs/heads/main"}}, "/auto_review/workflows/1"},
		// Read in sorted order: max_per_day before workflows, workflows
		// before the checks that span fields.
		{map[string]any{"workflows": []string{"nonsense"}, "max_per_day": 0}, "/auto_review/max_per_day"},
		{map[string]any{"workflows": []string{"nonsense"}, "events": []string{}}, "/auto_review/workflows/0"},
		{map[string]any{"workflows": []string{main}, "approve_up_to": "low"}, "/auto_review/approve_up_to"},
	}
	// Branches git wouldn't make.
	for _, branch := range []string{"../main", "main/", "a..b", "a//b", ".hidden", "x.lock", "-x", "/x", "x.", "a b", "a/.b", "a@b", "a~b"} {
		cases = append(cases, struct {
			auto  map[string]any
			field string
		}{map[string]any{"events": []string{}, "workflows": []string{file + "@refs/heads/" + branch}}, "/auto_review/workflows/0"})
	}
	var many []string
	for i := range 21 {
		many = append(many, fmt.Sprintf("example-org/billing-api/.github/workflows/w%d.yml@refs/heads/main", i))
	}
	cases = append(cases, struct {
		auto  map[string]any
		field string
	}{map[string]any{"events": []string{}, "workflows": many}, "/auto_review/workflows"})
	for _, c := range cases {
		st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "auto_review": c.auto})
		expect(t, fmt.Sprintf("auto_review %v", c.auto), st, http.StatusUnprocessableEntity, body)
		if body["field"] != c.field {
			t.Errorf("auto_review %v: field = %v (%v), want %s", c.auto, body["field"], body["message"], c.field)
		}
	}
}

// auto_review's workflows are kept as sent, sorted, each once, and none
// unless sent; with them, events may be none; and a create with them is a
// retry only with the same.
func TestAutoReview_WorkflowsKept(t *testing.T) {
	s := New(t)
	const main = "example-org/billing-api/.github/workflows/autopilot-review.yml@refs/heads/main"
	const shared = "Example-Org/.github/.github/workflows/autopilot-review.yaml@refs/heads/release/v2"
	// Another file, or the same in another case, is another workflow.
	const other = "example-org/billing-api/.github/workflows/Autopilot-Review.yml@refs/heads/main"
	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "auto_review": map[string]any{
		"events": []string{}, "workflows": []string{main, shared, main, other},
	}})
	expect(t, "workflows alone", st, http.StatusCreated, body)
	want := map[string]any{
		"events": []any{}, "workflows": []any{shared, other, main}, "max_per_day": float64(50),
		"can_decide": false, "can_decide_own_pr": false, "approve_up_to": nil,
	}
	if got, _ := json.Marshal(body["auto_review"]); string(got) != mustJSON(want) {
		t.Errorf("auto_review = %s, want %s", got, mustJSON(want))
	}
	v, _ := s.App("billing")
	if v.AutoReview == nil || strings.Join(v.AutoReview.Workflows, ",") != shared+","+other+","+main {
		t.Errorf("auto_review = %+v", v.AutoReview)
	}
	// The same create again is a retry; other workflows aren't.
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "auto_review": map[string]any{
		"events": []string{}, "workflows": []string{other, shared, main},
	}})
	expect(t, "the same create again", st, http.StatusOK, body)
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "auto_review": map[string]any{
		"events": []string{}, "workflows": []string{main},
	}})
	expect(t, "a create with other workflows", st, http.StatusConflict, body)
	// With events too.
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "1", map[string]any{"auto_review": map[string]any{
		"events": []string{"opened"}, "workflows": []string{main},
	}})
	expect(t, "events and a workflow", st, http.StatusOK, body)
	if v, _ = s.App("billing"); v.AutoReview == nil || strings.Join(v.AutoReview.Events, ",") != "opened" ||
		strings.Join(v.AutoReview.Workflows, ",") != main || v.LockVersion != 2 {
		t.Errorf("auto_review = %+v, lock_version %d", v.AutoReview, v.LockVersion)
	}
	// Left out, there are none, as for any field of the one setting.
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "2", map[string]any{"auto_review": map[string]any{
		"events": []string{"opened"},
	}})
	expect(t, "workflows left out", st, http.StatusOK, body)
	if v, _ = s.App("billing"); v.AutoReview == nil || len(v.AutoReview.Workflows) != 0 || v.LockVersion != 3 {
		t.Errorf("auto_review = %+v, lock_version %d, want no workflows", v.AutoReview, v.LockVersion)
	}
	// Twenty, with one sent twice, is twenty.
	var twenty []string
	for i := range 20 {
		twenty = append(twenty, fmt.Sprintf("example-org/billing-api/.github/workflows/w%02d.yml@refs/heads/main", i))
	}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "3", map[string]any{"auto_review": map[string]any{
		"events": []string{}, "workflows": append(slices.Clone(twenty), twenty[0]),
	}})
	expect(t, "twenty, one twice", st, http.StatusOK, body)
	if v, _ = s.App("billing"); v.AutoReview == nil || !slices.Equal(v.AutoReview.Workflows, twenty) {
		t.Errorf("auto_review = %+v", v.AutoReview)
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// review_also_reads is refused where AutoPilot refuses it, at the same
// field: each repository at its place when it isn't owner/name or names one
// already listed in another case, and the list when it isn't a list or holds
// more than 5. Beside the app's repositories, the first it may not read, in
// sorted order, is refused where it was sent. Fields are read in sorted
// order (this fake reads repos first), and the rules that span fields come
// after every field.
func TestReviewAlsoReads_Refused(t *testing.T) {
	s := New(t)
	own := []string{"example-org/infra"}
	var six []string
	for i := range 6 {
		six = append(six, fmt.Sprintf("example-org/modules-%d", i))
	}
	cases := []struct {
		what  string
		body  map[string]any
		field string
	}{
		{"not a list", map[string]any{"review_also_reads": "example-org/modules"}, "/review_also_reads"},
		{"null", map[string]any{"review_also_reads": nil}, "/review_also_reads"},
		{"an object", map[string]any{"review_also_reads": map[string]any{}}, "/review_also_reads"},
		{"not a string", map[string]any{"review_also_reads": []any{1}}, "/review_also_reads/0"},
		{"null in it", map[string]any{"review_also_reads": []any{"example-org/modules", nil}}, "/review_also_reads/1"},
		// Every item must be a string before any is read as a repository.
		{"nonsense, then not a string", map[string]any{"review_also_reads": []any{"nonsense", 1}}, "/review_also_reads/1"},
		{"six", map[string]any{"review_also_reads": six}, "/review_also_reads"},
		{"case only", map[string]any{"review_also_reads": []string{"example-org/modules", "Example-Org/Modules"}}, "/review_also_reads/1"},
		// Beside the app's repositories: one it owns, in any case, or of another owner.
		{"one it owns", map[string]any{"review_also_reads": []string{"example-org/modules", "example-org/infra"}}, "/review_also_reads/1"},
		{"one it owns, in another case", map[string]any{"review_also_reads": []string{"Example-Org/INFRA"}}, "/review_also_reads/0"},
		{"another owner", map[string]any{"review_also_reads": []string{"example-org/modules", "other/modules"}}, "/review_also_reads/1"},
		// The first in sorted order, where it was sent.
		{"two wrong", map[string]any{"review_also_reads": []string{"zed/modules", "example-org/infra"}}, "/review_also_reads/1"},
		// credential is read before it, when_seats_full and worker_image after.
		{"credential wrong too", map[string]any{"credential": "bogus", "review_also_reads": []string{"nonsense"}}, "/credential"},
		{"when_seats_full wrong too", map[string]any{"when_seats_full": "bogus", "review_also_reads": []string{"nonsense"}}, "/review_also_reads/0"},
		{"worker_image wrong too", map[string]any{"worker_image": "bogus", "review_also_reads": []string{"nonsense"}}, "/review_also_reads/0"},
		// Each field is read before the rules that span fields: GitHub
		// logins first, then what reviews also read, then the policy.
		{"worker_image wrong, another owner", map[string]any{"worker_image": "bogus", "review_also_reads": []string{"other/modules"}}, "/worker_image"},
		{"a login unpaired, another owner", map[string]any{"github_logins": []any{map[string]any{"login": "alice", "developer": "alice@example.org"}},
			"review_also_reads": []string{"other/modules"}}, "/github_logins/0"},
		{"seat_first alone, another owner", map[string]any{"credential": "seat_first", "review_also_reads": []string{"other/modules"}}, "/review_also_reads/0"},
	}
	bad := []string{
		"example-org", "example-org/", "/modules", "example-org/a/b", "example_org/modules", "example.org/modules",
		strings.Repeat("o", 40) + "/modules", "example-org/" + strings.Repeat("n", 101), "example-org/.", "example-org/..",
		"example-org/...", "example-org/a b", "example-org/modules\n", " example-org/modules", "example-org/modulé", "",
	}
	for _, b := range bad {
		cases = append(cases, struct {
			what  string
			body  map[string]any
			field string
		}{fmt.Sprintf("%q", b), map[string]any{"review_also_reads": []string{"example-org/modules", b}}, "/review_also_reads/1"})
	}
	for _, c := range cases {
		body := map[string]any{"name": "infrastructure", "repos": own}
		for k, v := range c.body {
			body[k] = v
		}
		st, answer := call(t, s, http.MethodPost, "/v1/admin/apps", "", body)
		expect(t, c.what, st, http.StatusUnprocessableEntity, answer)
		if answer["field"] != c.field {
			t.Errorf("%s: field = %v (%v), want %s", c.what, answer["field"], answer["message"], c.field)
		}
	}
	// An app that owns no repository has no owner to read from.
	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "infrastructure", "review_also_reads": []string{"example-org/modules"}})
	expect(t, "no repos", st, http.StatusUnprocessableEntity, body)
	if msg, _ := body["message"].(string); body["field"] != "/review_also_reads/0" || !strings.Contains(msg, "owns none") {
		t.Errorf("no repos: %v", body)
	}
}

// review_also_reads is kept as sent, sorted, each once, and none unless
// sent; a write that leaves it out keeps it, and [] takes it away. A create
// with it is a retry only with the same. A change is held to the app's
// repositories by what it leaves, only when it sends one of the two.
func TestReviewAlsoReads_Kept(t *testing.T) {
	s := New(t)
	own := []string{"example-org/infra", "other/thing"}
	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "plain", "repos": []string{"example-org/plain"}})
	expect(t, "none", st, http.StatusCreated, body)
	if got := mustJSON(body["review_also_reads"]); got != "[]" {
		t.Errorf("review_also_reads = %s, want []", got)
	}
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "infrastructure", "repos": own,
		"review_also_reads": []string{"example-org/zeta", "Example-Org/modules", "example-org/zeta", "OTHER/.github"}})
	expect(t, "create", st, http.StatusCreated, body)
	want := `["Example-Org/modules","OTHER/.github","example-org/zeta"]`
	if got := mustJSON(body["review_also_reads"]); got != want {
		t.Errorf("review_also_reads = %s, want %s", got, want)
	}
	// The same create again, in another order, is a retry; another isn't.
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "infrastructure", "repos": own,
		"review_also_reads": []string{"OTHER/.github", "example-org/zeta", "Example-Org/modules"}})
	expect(t, "the same create again", st, http.StatusOK, body)
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "infrastructure", "repos": own,
		"review_also_reads": []string{"example-org/zeta"}})
	expect(t, "a create with another list", st, http.StatusConflict, body)

	// A change that leaves it out keeps it.
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/infrastructure", "1", map[string]any{"developers": []string{"alice@example.org"}})
	expect(t, "left out", st, http.StatusOK, body)
	if got := mustJSON(body["review_also_reads"]); got != want {
		t.Errorf("review_also_reads = %s, want it kept", got)
	}
	// Sent alone, it is held to the record's repositories.
	for _, also := range [][]string{{"example-org/infra"}, {"third/modules"}} {
		st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/infrastructure", "2", map[string]any{"review_also_reads": also})
		expect(t, fmt.Sprintf("%v alone", also), st, http.StatusUnprocessableEntity, body)
		if body["field"] != "/review_also_reads" {
			t.Errorf("%v alone: field = %v", also, body["field"])
		}
	}
	// repos sent alone is held to the record's list: owning one it reads,
	// or giving up the owner one is of.
	for _, repos := range [][]string{{"example-org/infra", "other/thing", "example-org/zeta"}, {"example-org/infra"}} {
		st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/infrastructure", "2", map[string]any{"repos": repos})
		expect(t, fmt.Sprintf("repos %v alone", repos), st, http.StatusUnprocessableEntity, body)
		if msg, _ := body["message"].(string); body["field"] != "/repos" || !strings.Contains(msg, "same write") {
			t.Errorf("repos %v alone: %v", repos, body)
		}
	}
	// Both, held to each other at the place, and nothing changes.
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/infrastructure", "2", map[string]any{
		"repos": []string{"example-org/infra"}, "review_also_reads": []string{"example-org/zeta", "OTHER/.github"},
	})
	expect(t, "both, one of another owner", st, http.StatusUnprocessableEntity, body)
	if body["field"] != "/review_also_reads/1" {
		t.Errorf("field = %v", body["field"])
	}
	if v, _ := s.App("infrastructure"); v.LockVersion != 2 {
		t.Errorf("lock_version = %d, want nothing changed", v.LockVersion)
	}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/infrastructure", "2", map[string]any{
		"repos": []string{"example-org/infra"}, "review_also_reads": []string{"example-org/zeta"},
	})
	expect(t, "both together", st, http.StatusOK, body)
	// A repeat one version behind of the change the record holds is that
	// change; another is stale. A value it can't take is refused first.
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/infrastructure", "2", map[string]any{"review_also_reads": []string{"example-org/zeta"}})
	expect(t, "a repeat one version behind", st, http.StatusOK, body)
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/infrastructure", "2", map[string]any{"review_also_reads": []string{"example-org/modules"}})
	expect(t, "another change one version behind", st, http.StatusConflict, body)
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/infrastructure", "2", map[string]any{"review_also_reads": []string{"nonsense"}})
	expect(t, "a value it can't take, one version behind", st, http.StatusUnprocessableEntity, body)
	// A change of case alone is a change, kept as sent.
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/infrastructure", "3", map[string]any{"review_also_reads": []string{"Example-Org/Zeta"}})
	expect(t, "a change of case", st, http.StatusOK, body)
	if v, _ := s.App("infrastructure"); v.LockVersion != 4 || !slices.Equal(v.ReviewAlsoReads, []string{"Example-Org/Zeta"}) {
		t.Errorf("review_also_reads = %v, lock_version %d", v.ReviewAlsoReads, v.LockVersion)
	}
	// [] takes them all away.
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/infrastructure", "4", map[string]any{"review_also_reads": []string{}})
	expect(t, "[]", st, http.StatusOK, body)
	if got := mustJSON(body["review_also_reads"]); got != "[]" || body["lock_version"] != float64(5) {
		t.Errorf("review_also_reads = %s, lock_version %v", got, body["lock_version"])
	}

	// A record left out of step with its repositories, as a release that
	// predates the field could leave it, still takes a change that sends
	// neither.
	s.ChangeAppOutOfBand("infrastructure", func(v *AppView) { v.ReviewAlsoReads = []string{"third/modules"} })
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/infrastructure", "6", map[string]any{"developers": []string{}})
	expect(t, "a change that sends neither", st, http.StatusOK, body)

	// What is given from outside a write is kept as AutoPilot reads it: only
	// what it can read, each once without regard to case, sorted, at most 5.
	s.SeedApp(AppView{Name: "seeded", Repos: []string{"c/own"}, ReviewAlsoReads: []string{
		"nonsense", "example-org/b", "Example-Org/B", "example-org/a", "c/1", "c/2", "c/3", "c/4",
	}})
	if v, _ := s.App("seeded"); !slices.Equal(v.ReviewAlsoReads, []string{"c/1", "c/2", "c/3", "c/4", "example-org/a"}) {
		t.Errorf("seeded review_also_reads = %v", v.ReviewAlsoReads)
	}
	// Names AutoPilot takes, at their longest.
	long := strings.Repeat("o", 39)
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "edges", "repos": []string{long + "/own"},
		"review_also_reads": []string{long + "/" + strings.Repeat("n", 100), long + "/.github", long + "/a.", long + "/..a", long + "/_", long + "/-"}[:5]})
	expect(t, "names at their longest, and of dots beside others", st, http.StatusCreated, body)
}

// An app's fields are read in sorted order, as AutoPilot reads them (this
// fake still reads repos first), so a body with two wrong names the first;
// the rules that span fields come after every field, in AutoPilot's order:
// GitHub logins, what reviews also read, the credential policy, then the
// worker size.
func TestAppFields_ReadInOrder(t *testing.T) {
	s := New(t)
	unpaired := []any{map[string]any{"login": "alice", "developer": "alice@example.org"}}
	cases := []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"accepts": "x", "credential": "bogus"}, "/accepts"},
		{map[string]any{"auto_review": "x", "credential": "bogus"}, "/auto_review"},
		{map[string]any{"credential": "bogus", "developers": []string{"Not An Email"}}, "/credential"},
		{map[string]any{"worker_cpu": "x", "worker_image": "bogus"}, "/worker_cpu"},
		{map[string]any{"worker_image": "bogus", "worker_memory": "x"}, "/worker_image"},
		{map[string]any{"github_logins": unpaired, "credential": "seat_first"}, "/github_logins/0"},
		{map[string]any{"credential": "seat_first", "worker_cpu": 1024}, "/when_seats_full"},
		// max_reviews_per_day comes after github_logins and before
		// review_also_reads; work_item_projects after when_seats_full and
		// before the worker's fields.
		{map[string]any{"github_logins": "x", "max_reviews_per_day": 0}, "/github_logins"},
		{map[string]any{"max_reviews_per_day": 0, "review_also_reads": "x"}, "/max_reviews_per_day"},
		{map[string]any{"when_seats_full": "bogus", "work_item_projects": "x"}, "/when_seats_full"},
		{map[string]any{"work_item_projects": "x", "worker_cpu": "x"}, "/work_item_projects"},
		// Each is read before the rules that span fields.
		{map[string]any{"github_logins": unpaired, "max_reviews_per_day": 0}, "/max_reviews_per_day"},
		{map[string]any{"credential": "seat_first", "work_item_projects": []string{"beta"}}, "/work_item_projects/0"},
		// approve_as_user comes after accepts and before auto_review.
		{map[string]any{"accepts": "x", "approve_as_user": 1}, "/accepts"},
		{map[string]any{"approve_as_user": 1, "auto_review": "x"}, "/approve_as_user"},
	}
	for _, c := range cases {
		body := map[string]any{"name": "billing"}
		for k, v := range c.body {
			body[k] = v
		}
		st, answer := call(t, s, http.MethodPost, "/v1/admin/apps", "", body)
		expect(t, mustJSON(c.body), st, http.StatusUnprocessableEntity, answer)
		if answer["field"] != c.field {
			t.Errorf("%s: field = %v, want %s", mustJSON(c.body), answer["field"], c.field)
		}
	}
}

// work_item_projects is refused where AutoPilot refuses it, at the same
// field: the list when it isn't a list or holds more than 20 after repeats
// are kept once, an item that isn't a string at its place before any is read
// as a project, and then the first that isn't a project's identifier.
func TestWorkItemProjects_Refused(t *testing.T) {
	s := New(t)
	var many []string
	for i := range 21 {
		many = append(many, fmt.Sprintf("P%d", i))
	}
	cases := []struct {
		what  string
		value any
		field string
	}{
		{"not a list", "BETA", "/work_item_projects"},
		{"null", nil, "/work_item_projects"},
		{"an object", map[string]any{}, "/work_item_projects"},
		{"not a string", []any{1}, "/work_item_projects/0"},
		{"null in it", []any{"BETA", nil}, "/work_item_projects/1"},
		{"lowercase, then not a string", []any{"beta", 1}, "/work_item_projects/1"},
		{"the first wrong, in the order sent", []string{"BETA", "alpha", "x"}, "/work_item_projects/1"},
		{"twenty-one", many, "/work_item_projects"},
	}
	for _, bad := range []string{
		"", "beta", "Beta", "bETA", "1BETA", "9", "BETA-1", "BETA_1", "BETA 1", " BETA", "BETA ", "BETA\n", "ABCDEFGHIJK",
		"ÄPI", "W.B", "BETA-123",
	} {
		cases = append(cases, struct {
			what  string
			value any
			field string
		}{fmt.Sprintf("%q", bad), []string{"BETA", bad}, "/work_item_projects/1"})
	}
	for _, c := range cases {
		st, answer := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "work_item_projects": c.value})
		expect(t, c.what, st, http.StatusUnprocessableEntity, answer)
		if answer["field"] != c.field {
			t.Errorf("%s: field = %v (%v), want %s", c.what, answer["field"], answer["message"], c.field)
		}
	}
	// Twenty, with one listed twice, is twenty.
	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "work_item_projects": append(many[:20], "P0")})
	expect(t, "twenty, one twice", st, http.StatusCreated, body)
	// Identifiers at their edges.
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "edges",
		"work_item_projects": []string{"A", "Z", "ABCDEFGHIJ", "A123456789", "Q0"}})
	expect(t, "identifiers at their edges", st, http.StatusCreated, body)
}

// work_item_projects is kept sorted, each once, and none unless sent; a
// write that leaves it out keeps it, and [] takes it away. A create with it
// is a retry only with the same.
func TestWorkItemProjects_Kept(t *testing.T) {
	s := New(t)
	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "plain"})
	expect(t, "none", st, http.StatusCreated, body)
	if got := mustJSON(body["work_item_projects"]); got != "[]" {
		t.Errorf("work_item_projects = %s, want []", got)
	}
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing",
		"work_item_projects": []string{"BETA", "ALPHA", "BETA"}})
	expect(t, "create", st, http.StatusCreated, body)
	want := `["ALPHA","BETA"]`
	if got := mustJSON(body["work_item_projects"]); got != want {
		t.Errorf("work_item_projects = %s, want %s", got, want)
	}
	// The same create again, in another order, is a retry; another isn't.
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "work_item_projects": []string{"ALPHA", "BETA"}})
	expect(t, "the same create again", st, http.StatusOK, body)
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "work_item_projects": []string{"ALPHA"}})
	expect(t, "a create with another list", st, http.StatusConflict, body)
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing"})
	expect(t, "a create without it", st, http.StatusConflict, body)

	// A change that leaves it out keeps it.
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "1", map[string]any{"developers": []string{"alice@example.org"}})
	expect(t, "left out", st, http.StatusOK, body)
	if got := mustJSON(body["work_item_projects"]); got != want {
		t.Errorf("work_item_projects = %s, want it kept", got)
	}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "2", map[string]any{"work_item_projects": []string{"GAMMA"}})
	expect(t, "a change", st, http.StatusOK, body)
	if v, _ := s.App("billing"); v.LockVersion != 3 || !slices.Equal(v.WorkItemProjects, []string{"GAMMA"}) {
		t.Errorf("work_item_projects = %v, lock_version %d", v.WorkItemProjects, v.LockVersion)
	}
	// The same list again changes nothing.
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "3", map[string]any{"work_item_projects": []string{"GAMMA", "GAMMA"}})
	expect(t, "the same again", st, http.StatusOK, body)
	if body["lock_version"] != float64(3) {
		t.Errorf("lock_version = %v, want nothing changed", body["lock_version"])
	}
	// A repeat one version behind of the change the record holds is that
	// change; another is stale. A value it can't take is refused first.
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "2", map[string]any{"work_item_projects": []string{"GAMMA"}})
	expect(t, "a repeat one version behind", st, http.StatusOK, body)
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "2", map[string]any{"work_item_projects": []string{"BETA"}})
	expect(t, "another change one version behind", st, http.StatusConflict, body)
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "2", map[string]any{"work_item_projects": []string{"ops"}})
	expect(t, "a value it can't take, one version behind", st, http.StatusUnprocessableEntity, body)
	// [] takes them all away.
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "3", map[string]any{"work_item_projects": []string{}})
	expect(t, "[]", st, http.StatusOK, body)
	if got := mustJSON(body["work_item_projects"]); got != "[]" || body["lock_version"] != float64(4) {
		t.Errorf("work_item_projects = %s, lock_version %v", got, body["lock_version"])
	}

	// What is given from outside a write is kept as AutoPilot reads it: only
	// what it can read, each once, sorted, at most 20.
	var given []string
	for i := range 25 {
		given = append(given, fmt.Sprintf("P%02d", i))
	}
	s.SeedApp(AppView{Name: "seeded", WorkItemProjects: append(given, "beta", "P03", "")})
	if v, _ := s.App("seeded"); !slices.Equal(v.WorkItemProjects, given[:20]) {
		t.Errorf("seeded work_item_projects = %v", v.WorkItemProjects)
	}
}

// max_reviews_per_day is a whole number from 1 to 200, or null, refused at
// /max_reviews_per_day otherwise, as AutoPilot refuses it.
func TestMaxReviewsPerDay_Refused(t *testing.T) {
	s := New(t)
	for _, raw := range []string{`0`, `201`, `-1`, `1.5`, `200.5`, `"20"`, `true`, `false`, `[]`, `{}`, `1e3`} {
		st, answer := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "max_reviews_per_day": json.RawMessage(raw)})
		expect(t, raw, st, http.StatusUnprocessableEntity, answer)
		if answer["field"] != "/max_reviews_per_day" {
			t.Errorf("%s: field = %v, want /max_reviews_per_day", raw, answer["field"])
		}
	}
	for i, c := range []struct {
		raw  string
		want any
	}{{`1`, float64(1)}, {`200`, float64(200)}, {`1.0`, float64(1)}, {`2e1`, float64(20)}, {`null`, nil}} {
		name := fmt.Sprintf("app-%d", i)
		st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": name, "max_reviews_per_day": json.RawMessage(c.raw)})
		expect(t, c.raw, st, http.StatusCreated, body)
		if body["max_reviews_per_day"] != c.want {
			t.Errorf("%s: max_reviews_per_day = %v, want %v", c.raw, body["max_reviews_per_day"], c.want)
		}
	}
}

// max_reviews_per_day is null unless sent; a write that leaves it out keeps
// it, and null takes it back to AutoPilot's default. A create with it is a
// retry only with the same.
func TestMaxReviewsPerDay_Kept(t *testing.T) {
	s := New(t)
	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "plain"})
	expect(t, "none", st, http.StatusCreated, body)
	if v, ok := body["max_reviews_per_day"]; !ok || v != nil {
		t.Errorf("max_reviews_per_day = %v (sent %v), want null", v, ok)
	}
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "max_reviews_per_day": 100})
	expect(t, "create", st, http.StatusCreated, body)
	if body["max_reviews_per_day"] != float64(100) {
		t.Errorf("max_reviews_per_day = %v, want 100", body["max_reviews_per_day"])
	}
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "max_reviews_per_day": 100})
	expect(t, "the same create again", st, http.StatusOK, body)
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "max_reviews_per_day": 99})
	expect(t, "a create with another cap", st, http.StatusConflict, body)
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing"})
	expect(t, "a create without it", st, http.StatusConflict, body)

	// A change that leaves it out keeps it.
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "1", map[string]any{"developers": []string{"alice@example.org"}})
	expect(t, "left out", st, http.StatusOK, body)
	if body["max_reviews_per_day"] != float64(100) {
		t.Errorf("max_reviews_per_day = %v, want it kept", body["max_reviews_per_day"])
	}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "2", map[string]any{"max_reviews_per_day": 200})
	expect(t, "a change", st, http.StatusOK, body)
	if v, _ := s.App("billing"); v.LockVersion != 3 || v.MaxReviewsPerDay != 200 {
		t.Errorf("max_reviews_per_day = %d, lock_version %d", v.MaxReviewsPerDay, v.LockVersion)
	}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "3", map[string]any{"max_reviews_per_day": 200})
	expect(t, "the same again", st, http.StatusOK, body)
	if body["lock_version"] != float64(3) {
		t.Errorf("lock_version = %v, want nothing changed", body["lock_version"])
	}
	// A repeat one version behind of the change the record holds is that
	// change; another is stale. A value it can't take is refused first.
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "2", map[string]any{"max_reviews_per_day": 200})
	expect(t, "a repeat one version behind", st, http.StatusOK, body)
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "2", map[string]any{"max_reviews_per_day": 150})
	expect(t, "another change one version behind", st, http.StatusConflict, body)
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "2", map[string]any{"max_reviews_per_day": 0})
	expect(t, "a value it can't take, one version behind", st, http.StatusUnprocessableEntity, body)
	// null takes it back to the default.
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "3", map[string]any{"max_reviews_per_day": nil})
	expect(t, "null", st, http.StatusOK, body)
	if v, ok := body["max_reviews_per_day"]; !ok || v != nil || body["lock_version"] != float64(4) {
		t.Errorf("max_reviews_per_day = %v, lock_version %v", v, body["lock_version"])
	}

	// What is given from outside a write is kept as AutoPilot reads it: a
	// cap outside 1 to 200 reads as none.
	for given, want := range map[int64]int64{5: 5, 200: 200, 0: 0, 201: 0, -3: 0} {
		s.SeedApp(AppView{Name: "seeded", MaxReviewsPerDay: given})
		if v, _ := s.App("seeded"); v.MaxReviewsPerDay != want {
			t.Errorf("seeded %d: max_reviews_per_day = %d, want %d", given, v.MaxReviewsPerDay, want)
		}
	}
}

// approve_as_user is true or false, refused at /approve_as_user otherwise,
// null included; false unless sent; a write that leaves it out keeps it. A
// create with it is a retry only with the same.
func TestApproveAsUser(t *testing.T) {
	s := New(t)
	for _, raw := range []string{`null`, `"true"`, `1`, `0`, `[]`, `{}`} {
		st, answer := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "approve_as_user": json.RawMessage(raw)})
		expect(t, raw, st, http.StatusUnprocessableEntity, answer)
		if answer["field"] != "/approve_as_user" {
			t.Errorf("%s: field = %v, want /approve_as_user", raw, answer["field"])
		}
	}
	st, body := call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "plain"})
	expect(t, "none", st, http.StatusCreated, body)
	if body["approve_as_user"] != false {
		t.Errorf("approve_as_user = %v, want false", body["approve_as_user"])
	}
	// false sent on a create is the default, so a create without it is a
	// retry of it.
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "plain", "approve_as_user": false})
	expect(t, "false, a retry", st, http.StatusOK, body)
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "approve_as_user": true})
	expect(t, "create", st, http.StatusCreated, body)
	if body["approve_as_user"] != true {
		t.Errorf("approve_as_user = %v, want true", body["approve_as_user"])
	}
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing", "approve_as_user": true})
	expect(t, "the same create again", st, http.StatusOK, body)
	st, body = call(t, s, http.MethodPost, "/v1/admin/apps", "", map[string]any{"name": "billing"})
	expect(t, "a create without it", st, http.StatusConflict, body)

	// A change that leaves it out keeps it.
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "1", map[string]any{"developers": []string{"alice@example.org"}})
	expect(t, "left out", st, http.StatusOK, body)
	if body["approve_as_user"] != true {
		t.Errorf("approve_as_user = %v, want it kept", body["approve_as_user"])
	}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "2", map[string]any{"approve_as_user": false})
	expect(t, "off", st, http.StatusOK, body)
	if v, _ := s.App("billing"); v.LockVersion != 3 || v.ApproveAsUser {
		t.Errorf("approve_as_user = %v, lock_version %d", v.ApproveAsUser, v.LockVersion)
	}
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "3", map[string]any{"approve_as_user": false})
	expect(t, "off again", st, http.StatusOK, body)
	if body["lock_version"] != float64(3) {
		t.Errorf("lock_version = %v, want nothing changed", body["lock_version"])
	}
	// A repeat one version behind of the change the record holds is that
	// change; another is stale. A value it can't take is refused first.
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "2", map[string]any{"approve_as_user": false})
	expect(t, "a repeat one version behind", st, http.StatusOK, body)
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "2", map[string]any{"approve_as_user": true})
	expect(t, "another change one version behind", st, http.StatusConflict, body)
	st, body = call(t, s, http.MethodPatch, "/v1/admin/apps/billing", "2", map[string]any{"approve_as_user": nil})
	expect(t, "a value it can't take, one version behind", st, http.StatusUnprocessableEntity, body)

	s.SeedApp(AppView{Name: "seeded", ApproveAsUser: true})
	if v, _ := s.App("seeded"); !v.ApproveAsUser {
		t.Error("seeded approve_as_user is off")
	}
}
