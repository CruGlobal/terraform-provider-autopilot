package autopilottest

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
