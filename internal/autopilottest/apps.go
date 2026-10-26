package autopilottest

import (
	"cmp"
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

var (
	appNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	repoPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`)
	// The characters of a developer's login (see validEmail).
	emailCharacters = regexp.MustCompile(`^[a-z0-9._%+@-]+$`)
	// A GitHub login in lowercase ASCII: a letter or digit, then letters,
	// digits, - and _, at most 39.
	gitHubLoginPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,38}$`)
	maxAppName         = 64
	reviewKind         = "review-pr"
	maxRepos           = 50
	maxAccepts         = 20
	maxDevelopers      = 100
	maxGitHubLogins    = 100
	appWritable        = []string{"repos", "accepts", "developers", "github_logins", "credential", "when_seats_full", "worker_image", "worker_cpu", "worker_memory", "auto_review"}
	appReadOnly        = []string{"lock_version", "created_at", "updated_at"}
	acceptWritable     = []string{"sender", "kinds", "can_decide", "can_auto_merge", "can_decide_own_pr", "approve_up_to"}
	appCreateFields    = append([]string{"name"}, appWritable...)
	// An app's credential policy: what its tasks run on, and with
	// seat_first, what they do when every seat is full.
	credentials   = []string{"bedrock", "seat_first"}
	whenSeatsFull = []string{"wait", "fall_back"}
	// The worker images an app's tasks may run in.
	workerImages = []string{"node", "ruby", "ruby-browser", "python", "go"}
	// The risk levels a deciding review may approve up to, lowest first.
	riskLevels = []string{"low", "medium", "high"}
	// The pull_request actions an app's automatic reviews may start on, in
	// the order AutoPilot keeps them, and those they start on unless set.
	autoEvents        = []string{"opened", "ready_for_review", "synchronize"}
	defaultAutoEvents = []string{"opened", "ready_for_review"}
	// An app's cap on its automatic reviews in 24 hours: its default, and
	// the most it may be.
	defaultAutoPerDay int64 = 50
	maxAutoPerDay     int64 = 200
)

// AutoReview is an app's automatic reviews. An empty ApproveUpTo is no
// ceiling (null).
type AutoReview struct {
	Events         []string
	MaxPerDay      int64
	CanDecide      bool
	CanDecideOwnPR bool
	ApproveUpTo    string
}

func cloneAutoReview(a *AutoReview) *AutoReview {
	if a == nil {
		return nil
	}
	out := *a
	out.Events = slices.Clone(a.Events)
	return &out
}

func sameAutoReview(x, y *AutoReview) bool {
	if x == nil || y == nil {
		return x == y
	}
	return slices.Equal(x.Events, y.Events) && x.MaxPerDay == y.MaxPerDay && x.CanDecide == y.CanDecide &&
		x.CanDecideOwnPR == y.CanDecideOwnPR && x.ApproveUpTo == y.ApproveUpTo
}

// autoReviewJSON is automatic reviews as AutoPilot shows them: every field,
// or null for none.
func autoReviewJSON(a *AutoReview) any {
	if a == nil {
		return nil
	}
	return map[string]any{
		"events": slices.Clone(a.Events), "max_per_day": a.MaxPerDay, "can_decide": a.CanDecide,
		"can_decide_own_pr": a.CanDecideOwnPR, "approve_up_to": nullable(a.ApproveUpTo),
	}
}

// readAutoReview reads auto_review as AutoPilot does: null for none, or an
// object whose fields are read in sorted order, each left out at its
// default; the events kept once each, in autoEvents' order; and
// can_decide_own_pr and approve_up_to only with can_decide.
func readAutoReview(raw json.RawMessage) (*AutoReview, *refusal) {
	if string(raw) == "null" {
		return nil, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, invalid("/auto_review", "is an object with events, max_per_day, can_decide, can_decide_own_pr and approve_up_to, or null")
	}
	out := AutoReview{Events: slices.Clone(defaultAutoEvents), MaxPerDay: defaultAutoPerDay}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		at := "/auto_review/" + k
		var ref *refusal
		switch k {
		case "events":
			var events []json.RawMessage
			if err := json.Unmarshal(obj[k], &events); err != nil || len(events) == 0 {
				return nil, invalid(at, "is a list of one or more of opened, ready_for_review, synchronize")
			}
			sent := []string{}
			for i, e := range events {
				var name string
				if err := json.Unmarshal(e, &name); err != nil || !slices.Contains(autoEvents, name) {
					return nil, invalid(at+"/"+itoa(i), "is a list of one or more of opened, ready_for_review, synchronize")
				}
				sent = append(sent, name)
			}
			out.Events = []string{}
			for _, name := range autoEvents {
				if slices.Contains(sent, name) {
					out.Events = append(out.Events, name)
				}
			}
		case "max_per_day":
			var n *float64
			if err := json.Unmarshal(obj[k], &n); err != nil || n == nil || *n != math.Trunc(*n) ||
				*n < 1 || *n > float64(maxAutoPerDay) {
				return nil, invalid(at, "is a whole number from 1 to 200: the most automatic reviews in 24 hours")
			}
			out.MaxPerDay = int64(*n)
		case "can_decide":
			if out.CanDecide, ref = boolValue(obj[k], at); ref != nil {
				return nil, ref
			}
		case "can_decide_own_pr":
			if out.CanDecideOwnPR, ref = boolValue(obj[k], at); ref != nil {
				return nil, ref
			}
		case "approve_up_to":
			if out.ApproveUpTo, ref = oneOrNull(obj[k], at, riskLevels); ref != nil {
				return nil, ref
			}
		default:
			return nil, &refusal{http.StatusUnprocessableEntity, "invalid_attribute", "AutoPilot doesn't know this field", at}
		}
	}
	// The own pull request's toggle and the ceiling each narrow can_decide,
	// so each needs it.
	if out.CanDecideOwnPR && !out.CanDecide {
		return nil, invalid("/auto_review/can_decide_own_pr", "is true only with can_decide true")
	}
	if out.ApproveUpTo != "" && !out.CanDecide {
		return nil, invalid("/auto_review/approve_up_to", "is set only with can_decide true")
	}
	return &out, nil
}

// workerMemory is the memory, in MiB, that Fargate allows with each CPU size
// an app's workers may have: at most 4 vCPU and 16 GB.
var workerMemory = map[int64][]int64{
	256:  {512, 1024, 2048},
	512:  gigabytes(1024, 4096),
	1024: gigabytes(2048, 8192),
	2048: gigabytes(4096, 16384),
	4096: gigabytes(8192, 16384),
}

// gigabytes is from to to MiB, in steps of 1 GB.
func gigabytes(from, to int64) []int64 {
	var out []int64
	for m := from; m <= to; m += 1024 {
		out = append(out, m)
	}
	return out
}

// pullRequestKinds are the kinds whose tasks open a pull request, the only
// ones auto-merge applies to.
var pullRequestKinds = []string{"implement-work-item", "fix-error"}

// builtInSenders are the senders built into AutoPilot, the MCP's and
// GitHub's, whose reviews run for a person. Only their entries may set
// can_decide_own_pr.
var builtInSenders = []string{"autopilot-mcp", "autopilot-github"}

// GitHubLogin is one developer's GitHub login.
type GitHubLogin struct {
	Login     string
	Developer string
}

// validEmail is AutoPilot's rule for a developer's login, an email address
// in lowercase ASCII: at most 254 characters; a local part of 1 to 64 before
// the first @; a domain after it with a dot, that doesn't start or end with
// one, has no two in a row and no other @; and only emailCharacters.
func validEmail(text string) bool {
	at := strings.IndexByte(text, '@')
	if at < 0 {
		return false
	}
	local, domain := text[:at], text[at+1:]
	return len(text) <= 254 && len(local) >= 1 && len(local) <= 64 &&
		strings.Contains(domain, ".") && !strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".") &&
		!strings.Contains(domain, "..") && !strings.Contains(domain, "@") &&
		emailCharacters.MatchString(text)
}

// sortLogins is logins sorted by login, as AutoPilot keeps them.
func sortLogins(in []GitHubLogin) []GitHubLogin {
	out := slices.Clone(in)
	if out == nil {
		out = []GitHubLogin{}
	}
	slices.SortFunc(out, func(x, y GitHubLogin) int { return cmp.Compare(x.Login, y.Login) })
	return out
}

// unmappedLogin is the first of logins whose developer isn't in developers,
// or nil.
func unmappedLogin(logins []GitHubLogin, developers []string) *GitHubLogin {
	for i := range logins {
		if !slices.Contains(developers, logins[i].Developer) {
			return &logins[i]
		}
	}
	return nil
}

// Accept is one sender an app accepts work from.
type Accept struct {
	Sender         string
	Kinds          []string
	CanDecide      bool
	CanAutoMerge   bool
	CanDecideOwnPR bool
	// ApproveUpTo is the most risk a pull request may carry for the
	// sender's reviews to approve it; empty is no ceiling (null).
	ApproveUpTo string
}

// appRecord is an app as the fake stores it. An empty credential or
// whenSeatsFull is null: the app has no credential policy of its own. An
// empty workerImage is null (node), and a zero workerCPU and workerMemory
// are null (the worker's own size).
type appRecord struct {
	name                      string
	repos                     []string
	accepts                   []Accept
	developers                []string
	githubLogins              []GitHubLogin
	credential, whenSeatsFull string
	workerImage               string
	workerCPU, workerMemory   int64
	autoReview                *AutoReview
	lockVersion               int64
	createdAt, updatedAt      time.Time
}

// AppView is a copy of a stored app, for assertions. An empty Credential,
// WhenSeatsFull or WorkerImage is null, and so is a zero WorkerCPU or
// WorkerMemory.
type AppView struct {
	Name          string
	Repos         []string
	Accepts       []Accept
	Developers    []string
	GitHubLogins  []GitHubLogin
	Credential    string
	WhenSeatsFull string
	WorkerImage   string
	WorkerCPU     int64
	WorkerMemory  int64
	// AutoReview is nil when the app has no automatic reviews.
	AutoReview  *AutoReview
	LockVersion int64
}

func (rec *appRecord) view() AppView {
	return AppView{
		Name: rec.name, Repos: slices.Clone(rec.repos), Accepts: cloneAccepts(rec.accepts),
		Developers: slices.Clone(rec.developers), GitHubLogins: slices.Clone(rec.githubLogins),
		Credential: rec.credential, WhenSeatsFull: rec.whenSeatsFull,
		WorkerImage: rec.workerImage, WorkerCPU: rec.workerCPU, WorkerMemory: rec.workerMemory,
		AutoReview:  cloneAutoReview(rec.autoReview),
		LockVersion: rec.lockVersion,
	}
}

func cloneAccepts(in []Accept) []Accept {
	out := make([]Accept, 0, len(in))
	for _, a := range in {
		out = append(out, Accept{
			Sender: a.Sender, Kinds: slices.Clone(a.Kinds),
			CanDecide: a.CanDecide, CanAutoMerge: a.CanAutoMerge, CanDecideOwnPR: a.CanDecideOwnPR,
			ApproveUpTo: a.ApproveUpTo,
		})
	}
	return out
}

func sortAccepts(in []Accept) []Accept {
	out := cloneAccepts(in)
	for i := range out {
		out[i].Kinds = sortedUnique(out[i].Kinds)
	}
	slices.SortFunc(out, func(x, y Accept) int { return cmp.Compare(x.Sender, y.Sender) })
	return out
}

func acceptsEqual(x, y []Accept) bool {
	return slices.EqualFunc(sortAccepts(x), sortAccepts(y), func(a, b Accept) bool {
		return sameAccept(a, b)
	})
}

// sameAccept reports whether two entries, kinds sorted, are exactly equal.
func sameAccept(a, b Accept) bool {
	return a.Sender == b.Sender && a.CanDecide == b.CanDecide && a.CanAutoMerge == b.CanAutoMerge &&
		a.CanDecideOwnPR == b.CanDecideOwnPR && a.ApproveUpTo == b.ApproveUpTo && slices.Equal(a.Kinds, b.Kinds)
}

func (rec *appRecord) json() map[string]any {
	accepts := make([]any, 0, len(rec.accepts))
	for _, a := range sortAccepts(rec.accepts) {
		accepts = append(accepts, map[string]any{
			"sender": a.Sender, "kinds": a.Kinds, "can_decide": a.CanDecide, "can_auto_merge": a.CanAutoMerge,
			"can_decide_own_pr": a.CanDecideOwnPR, "approve_up_to": nullable(a.ApproveUpTo),
		})
	}
	logins := make([]any, 0, len(rec.githubLogins))
	for _, l := range sortLogins(rec.githubLogins) {
		logins = append(logins, map[string]any{"login": l.Login, "developer": l.Developer})
	}
	return map[string]any{
		"name":            rec.name,
		"repos":           sortedUnique(rec.repos),
		"accepts":         accepts,
		"developers":      sortedUnique(rec.developers),
		"github_logins":   logins,
		"credential":      nullable(rec.credential),
		"when_seats_full": nullable(rec.whenSeatsFull),
		"worker_image":    nullable(rec.workerImage),
		"worker_cpu":      nullableInt(rec.workerCPU),
		"worker_memory":   nullableInt(rec.workerMemory),
		"auto_review":     autoReviewJSON(rec.autoReview),
		"lock_version":    rec.lockVersion,
		"created_at":      timestamp(rec.createdAt),
		"updated_at":      timestamp(rec.updatedAt),
	}
}

// nullable is "" as null, and any other string as itself.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableInt is 0 as null, and any other number as itself.
func nullableInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

// --- test controls ----------------------------------------------------------

// SeedApp stores an app directly, as if another root made it.
func (s *Server) SeedApp(v AppView) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.apps[v.Name] = &appRecord{
		name: v.Name, repos: sortedUniqueFold(v.Repos), accepts: sortAccepts(v.Accepts),
		developers: sortedUnique(v.Developers), githubLogins: sortLogins(v.GitHubLogins),
		credential: v.Credential, whenSeatsFull: v.WhenSeatsFull,
		workerImage: v.WorkerImage, workerCPU: v.WorkerCPU, workerMemory: v.WorkerMemory,
		autoReview:  cloneAutoReview(v.AutoReview),
		lockVersion: 1, createdAt: now, updatedAt: now,
	}
}

// App returns a copy of a stored app.
func (s *Server) App(name string) (AppView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.apps[name]
	if !ok {
		return AppView{}, false
	}
	return rec.view(), true
}

// ChangeAppOutOfBand applies change to an app and bumps its lock_version, as
// a write from outside this Terraform would.
func (s *Server) ChangeAppOutOfBand(name string, change func(v *AppView)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.apps[name]
	if !ok {
		return
	}
	v := rec.view()
	change(&v)
	rec.repos, rec.accepts, rec.developers = sortedUniqueFold(v.Repos), sortAccepts(v.Accepts), sortedUnique(v.Developers)
	rec.githubLogins = sortLogins(v.GitHubLogins)
	rec.credential, rec.whenSeatsFull = v.Credential, v.WhenSeatsFull
	rec.workerImage, rec.workerCPU, rec.workerMemory = v.WorkerImage, v.WorkerCPU, v.WorkerMemory
	rec.autoReview = cloneAutoReview(v.AutoReview)
	rec.lockVersion++
	rec.updatedAt = time.Now()
}

// RemoveApp removes an app, as a delete from outside would.
func (s *Server) RemoveApp(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.apps, name)
}

// --- routes -------------------------------------------------------------------

func (s *Server) appRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/admin/apps", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		names := make([]string, 0, len(s.apps))
		for name := range s.apps {
			names = append(names, name)
		}
		slices.Sort(names)
		results := make([]any, 0, len(names))
		for _, name := range names {
			results = append(results, s.apps[name].json())
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
	})

	mux.HandleFunc("GET /v1/admin/apps/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		rec, ok := s.apps[r.PathValue("name")]
		if !ok {
			writeError(w, notFound("app"))
			return
		}
		writeJSON(w, http.StatusOK, rec.json())
	})

	mux.HandleFunc("POST /v1/admin/apps", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		status, body, ref := s.createApp(r)
		if ref != nil {
			writeError(w, *ref)
			return
		}
		writeJSON(w, status, body)
	})

	mux.HandleFunc("PATCH /v1/admin/apps/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		body, ref := s.patchApp(r)
		if ref != nil {
			writeError(w, *ref)
			return
		}
		writeJSON(w, http.StatusOK, body)
	})

	mux.HandleFunc("DELETE /v1/admin/apps/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		name := r.PathValue("name")
		rec, ok := s.apps[name]
		if !ok {
			writeError(w, notFound("app"))
			return
		}
		if ref := ifMatch(r, rec.lockVersion, false); ref != nil {
			writeError(w, *ref)
			return
		}
		delete(s.apps, name)
		w.WriteHeader(http.StatusNoContent)
	})
}

// appFields are the writable fields of an app, as read from a body. A nil
// list was not sent. policySent says the body sent credential, which sets
// both credential and whenSeatsFull ("" for null). imageSent says it sent
// worker_image ("" for null), and sizeSent that it sent worker_cpu or
// worker_memory, which sets both (0 for null).
type appFields struct {
	repos      []string
	accepts    []Accept
	developers []string
	// githubLogins is nil when not sent. loginAt is where each login was
	// first sent, as a pointer, for a refusal to name.
	githubLogins              []GitHubLogin
	loginAt                   map[string]string
	policySent                bool
	credential, whenSeatsFull string
	imageSent                 bool
	workerImage               string
	sizeSent                  bool
	workerCPU, workerMemory   int64
	// autoSent says the body sent auto_review, whole; autoReview is nil for
	// null.
	autoSent   bool
	autoReview *AutoReview
}

func readAppFields(obj map[string]json.RawMessage) (appFields, *refusal) {
	var f appFields
	if raw, ok := obj["repos"]; ok {
		repos, ref := stringList(raw, "/repos")
		if ref != nil {
			return f, ref
		}
		if len(repos) > maxRepos {
			return f, invalid("/repos", "an app owns at most 50 repositories")
		}
		for i, repo := range repos {
			if !repoPattern.MatchString(repo) {
				return f, invalid("/repos/"+itoa(i), "a repository is owner/name")
			}
		}
		// Matched without regard to case: a repository named twice in
		// different cases is refused; named twice exactly, it is kept once.
		// A case-sensitive sort can put other repositories between two that
		// differ only in case, so each is compared with all the others.
		f.repos = sortedUnique(repos)
		seen := make(map[string]bool, len(f.repos))
		for _, repo := range f.repos {
			folded := strings.ToLower(repo)
			if seen[folded] {
				return f, invalid("/repos", "the list names one repository twice, in different cases")
			}
			seen[folded] = true
		}
	}
	if raw, ok := obj["accepts"]; ok {
		var entries []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
			return f, invalid("/accepts", "must be a list of objects")
		}
		if len(entries) > maxAccepts {
			return f, invalid("/accepts", "an app accepts at most 20 senders")
		}
		f.accepts = []Accept{}
		seen := map[string]Accept{}
		for i, e := range entries {
			at := "/accepts/" + itoa(i)
			if ref := refuseUnknown(e, at, acceptWritable...); ref != nil {
				return f, ref
			}
			rawSender, ok := e["sender"]
			if !ok {
				return f, invalid(at+"/sender", "an accepted sender needs a name")
			}
			sender, ref := stringValue(rawSender, at+"/sender")
			if ref != nil {
				return f, ref
			}
			if !senderNamePattern.MatchString(sender) {
				return f, invalid(at+"/sender", "a sender's name is a lowercase letter, then lowercase letters, digits and -, at most 40 characters")
			}
			rawKinds, ok := e["kinds"]
			if !ok {
				return f, invalid(at+"/kinds", "an accepted sender needs at least one kind")
			}
			kinds, ref := stringList(rawKinds, at+"/kinds")
			if ref != nil {
				return f, ref
			}
			if len(kinds) == 0 {
				return f, invalid(at+"/kinds", "an accepted sender needs at least one kind")
			}
			for j, k := range kinds {
				if !slices.Contains(KnownKinds, k) {
					return f, invalid(at+"/kinds/"+itoa(j), "AutoPilot doesn't know this kind")
				}
			}
			// Each flag is false unless sent, and must be true or false when
			// sent: null is refused.
			flags := map[string]bool{}
			for _, name := range []string{"can_decide", "can_auto_merge", "can_decide_own_pr"} {
				if raw, ok := e[name]; ok {
					if flags[name], ref = boolValue(raw, at+"/"+name); ref != nil {
						return f, ref
					}
				}
			}
			canDecide, canAutoMerge, canDecideOwnPR := flags["can_decide"], flags["can_auto_merge"], flags["can_decide_own_pr"]
			if canDecide && !slices.Contains(kinds, reviewKind) {
				return f, invalid(at+"/can_decide", "can_decide is only for an entry whose kinds include review-pr")
			}
			// The own pull request's toggle widens can_decide, so it needs it,
			// and only a built-in sender's reviews run for a person.
			if canDecideOwnPR && !canDecide {
				return f, invalid(at+"/can_decide_own_pr", "is true only with can_decide true")
			}
			if canDecideOwnPR && !slices.Contains(builtInSenders, sender) {
				return f, invalid(at+"/can_decide_own_pr",
					"is true only for autopilot-mcp and autopilot-github, whose reviews run for a person")
			}
			// The ceiling on what a deciding review may approve narrows
			// can_decide, so it needs it. null is no ceiling, as the record
			// shows it.
			approveUpTo := ""
			if raw, ok := e["approve_up_to"]; ok {
				if approveUpTo, ref = oneOrNull(raw, at+"/approve_up_to", riskLevels); ref != nil {
					return f, ref
				}
			}
			if approveUpTo != "" && !canDecide {
				return f, invalid(at+"/approve_up_to", "is set only with can_decide true")
			}
			if canAutoMerge && !slices.ContainsFunc(kinds, func(k string) bool { return slices.Contains(pullRequestKinds, k) }) {
				return f, invalid(at+"/can_auto_merge", "is true only with implement-work-item or fix-error in kinds")
			}
			entry := Accept{
				Sender: sender, Kinds: sortedUnique(kinds),
				CanDecide: canDecide, CanAutoMerge: canAutoMerge, CanDecideOwnPR: canDecideOwnPR,
				ApproveUpTo: approveUpTo,
			}
			// An entry sent twice exactly is kept once; two different entries
			// for one sender are refused.
			if prior, dup := seen[sender]; dup {
				if sameAccept(prior, entry) {
					continue
				}
				return f, invalid(at+"/sender", "an app accepts each sender at most once")
			}
			seen[sender] = entry
			f.accepts = append(f.accepts, entry)
		}
		f.accepts = sortAccepts(f.accepts)
	}
	if raw, ok := obj["developers"]; ok {
		devs, ref := stringList(raw, "/developers")
		if ref != nil {
			return f, ref
		}
		if len(devs) > maxDevelopers {
			return f, invalid("/developers", "an app has at most 100 developers")
		}
		for i, d := range devs {
			if !validEmail(d) {
				return f, invalid("/developers/"+itoa(i), "a developer is their login: an email address in lowercase ASCII")
			}
		}
		f.developers = sortedUnique(devs)
	}
	if raw, ok := obj["github_logins"]; ok {
		logins, at, ref := readGitHubLogins(raw)
		if ref != nil {
			return f, ref
		}
		f.githubLogins, f.loginAt = logins, at
	}
	// The credential policy is one: a write that sends credential sets both
	// fields, and when_seats_full means nothing without it.
	rawCredential, sentCredential := obj["credential"]
	rawWhen, sentWhen := obj["when_seats_full"]
	if sentCredential {
		credential, ref := oneOrNull(rawCredential, "/credential", credentials)
		if ref != nil {
			return f, ref
		}
		when := ""
		if sentWhen {
			if when, ref = oneOrNull(rawWhen, "/when_seats_full", whenSeatsFull); ref != nil {
				return f, ref
			}
		}
		switch {
		case credential == "seat_first" && when == "":
			return f, invalid("/when_seats_full", "is wait or fall_back when credential is seat_first")
		case credential != "seat_first" && when != "":
			return f, invalid("/when_seats_full", "is only for credential seat_first")
		}
		f.policySent, f.credential, f.whenSeatsFull = true, credential, when
	} else if sentWhen {
		return f, invalid("/when_seats_full", "is sent with credential: a write that sends credential sets both")
	}
	if raw, ok := obj["worker_image"]; ok {
		image, ref := oneOrNull(raw, "/worker_image", workerImages)
		if ref != nil {
			return f, ref
		}
		f.imageSent, f.workerImage = true, image
	}
	// The worker's size is one too: a write that sends either field sets
	// both, so one left out is null.
	rawCPU, sentCPU := obj["worker_cpu"]
	rawMemory, sentMemory := obj["worker_memory"]
	if sentCPU || sentMemory {
		var cpu, memory int64
		var ref *refusal
		if sentCPU {
			if cpu, ref = wholeOrNull(rawCPU, "/worker_cpu", "is CPU units, 1024 to a vCPU, or null"); ref != nil {
				return f, ref
			}
		}
		if sentMemory {
			if memory, ref = wholeOrNull(rawMemory, "/worker_memory", "is MiB, or null"); ref != nil {
				return f, ref
			}
		}
		allowed, knownCPU := workerMemory[cpu]
		switch {
		case cpu == 0 && memory == 0:
		case cpu == 0:
			return f, invalid("/worker_cpu", "is sent with worker_memory: the two are one size, set together")
		case memory == 0:
			return f, invalid("/worker_memory", "is sent with worker_cpu: the two are one size, set together")
		case !knownCPU:
			return f, invalid("/worker_cpu", "is 256, 512, 1024, 2048, 4096 or null")
		case !slices.Contains(allowed, memory):
			return f, invalid("/worker_memory", "is MiB that Fargate allows with worker_cpu, at most 16 GB")
		}
		f.sizeSent, f.workerCPU, f.workerMemory = true, cpu, memory
	}
	if raw, ok := obj["auto_review"]; ok {
		auto, ref := readAutoReview(raw)
		if ref != nil {
			return f, ref
		}
		f.autoSent, f.autoReview = true, auto
	}
	// Once every field is read: a write that sends both developers and
	// github_logins is checked here, at the entry; one that sends only one of
	// them is checked against the record.
	if f.githubLogins != nil && f.developers != nil {
		if l := unmappedLogin(f.githubLogins, f.developers); l != nil {
			return f, invalid(f.loginAt[l.Login], "names a developer the app doesn't list: each GitHub login's developer is one of the app's developers")
		}
	}
	return f, nil
}

// readGitHubLogins reads github_logins: a list of {login, developer}, each
// login at most once (an entry sent twice exactly is kept once), at most 100,
// sorted by login. at says where each login was first sent.
func readGitHubLogins(raw json.RawMessage) ([]GitHubLogin, map[string]string, *refusal) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
		return nil, nil, invalid("/github_logins", "is a list of GitHub logins, each with the developer it is")
	}
	out := []GitHubLogin{}
	at, developerOf := map[string]string{}, map[string]string{}
	for i, rawEntry := range entries {
		base := "/github_logins/" + itoa(i)
		var e map[string]json.RawMessage
		if err := json.Unmarshal(rawEntry, &e); err != nil || e == nil {
			return nil, nil, invalid(base, "is an object with login and developer")
		}
		// AutoPilot reads an entry's fields in sorted order, so the first
		// field it names is the first wrong one in that order.
		keys := make([]string, 0, len(e))
		for k := range e {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		var login, developer *string
		for _, k := range keys {
			switch k {
			case "developer":
				d, ref := stringValue(e[k], base+"/developer")
				if ref != nil || !validEmail(d) {
					return nil, nil, invalid(base+"/developer", "a developer is an email address in lowercase ASCII")
				}
				developer = &d
			case "login":
				l, ref := stringValue(e[k], base+"/login")
				if ref != nil || !gitHubLoginPattern.MatchString(l) {
					return nil, nil, invalid(base+"/login", "a GitHub login in lowercase: a letter or digit, then letters, digits, - and _, at most 39 characters")
				}
				login = &l
			default:
				return nil, nil, &refusal{http.StatusUnprocessableEntity, "invalid_attribute", "AutoPilot doesn't know this field", base + "/" + k}
			}
		}
		if login == nil {
			return nil, nil, invalid(base+"/login", "is required")
		}
		if developer == nil {
			return nil, nil, invalid(base+"/developer", "is required")
		}
		if prior, seen := developerOf[*login]; seen {
			if prior != *developer {
				return nil, nil, invalid(base+"/login", "is listed twice: each GitHub login is one developer")
			}
			continue
		}
		developerOf[*login], at[*login] = *developer, base
		out = append(out, GitHubLogin{Login: *login, Developer: *developer})
	}
	if len(out) > maxGitHubLogins {
		return nil, nil, invalid("/github_logins", "lists at most 100 GitHub logins")
	}
	return sortLogins(out), at, nil
}

// wholeOrNull reads a whole number above zero, or null as 0.
func wholeOrNull(raw json.RawMessage, pointer, message string) (int64, *refusal) {
	var out *int64
	if err := json.Unmarshal(raw, &out); err != nil || (out != nil && *out <= 0) {
		return 0, invalid(pointer, message)
	}
	if out == nil {
		return 0, nil
	}
	return *out, nil
}

// oneOrNull reads one of allowed, or null as "".
func oneOrNull(raw json.RawMessage, pointer string, allowed []string) (string, *refusal) {
	var out *string
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", invalid(pointer, "must be one of "+strings.Join(allowed, ", ")+", or null")
	}
	if out == nil {
		return "", nil
	}
	if !slices.Contains(allowed, *out) {
		return "", invalid(pointer, "must be one of "+strings.Join(allowed, ", ")+", or null")
	}
	return *out, nil
}

// holds reports whether the app already holds everything f sends.
func (rec *appRecord) holds(f appFields) bool {
	return (f.repos == nil || slices.Equal(f.repos, rec.repos)) &&
		(f.accepts == nil || acceptsEqual(f.accepts, rec.accepts)) &&
		(f.developers == nil || slices.Equal(f.developers, rec.developers)) &&
		(f.githubLogins == nil || slices.Equal(f.githubLogins, rec.githubLogins)) &&
		(!f.policySent || (f.credential == rec.credential && f.whenSeatsFull == rec.whenSeatsFull)) &&
		(!f.imageSent || f.workerImage == rec.workerImage) &&
		(!f.sizeSent || (f.workerCPU == rec.workerCPU && f.workerMemory == rec.workerMemory)) &&
		(!f.autoSent || sameAutoReview(f.autoReview, rec.autoReview))
}

// repoOwner names the app other than except that owns repo, matched without
// regard to case, or "".
func (s *Server) repoOwner(repo, except string) string {
	for name, rec := range s.apps {
		if name == except {
			continue
		}
		for _, owned := range rec.repos {
			if strings.EqualFold(owned, repo) {
				return name
			}
		}
	}
	return ""
}

func (s *Server) refuseTakenRepos(repos []string, except string) *refusal {
	for i, repo := range repos {
		if owner := s.repoOwner(repo, except); owner != "" {
			return &refusal{http.StatusConflict, "repo_taken", repo + " belongs to the app " + owner, "/repos/" + itoa(i)}
		}
	}
	return nil
}

func (s *Server) createApp(r *http.Request) (int, map[string]any, *refusal) {
	obj, ref := readObject(r)
	if ref != nil {
		return 0, nil, ref
	}
	for _, k := range appReadOnly {
		if _, ok := obj[k]; ok {
			return 0, nil, invalid("/"+k, "this field is read-only")
		}
	}
	if ref := refuseUnknown(obj, "", appCreateFields...); ref != nil {
		return 0, nil, ref
	}
	raw, ok := obj["name"]
	if !ok {
		return 0, nil, invalid("/name", "an app needs a name")
	}
	name, ref := stringValue(raw, "/name")
	if ref != nil {
		return 0, nil, ref
	}
	if !appNamePattern.MatchString(name) || len(name) > maxAppName {
		return 0, nil, invalid("/name", "an app's name is a lowercase letter or digit, then lowercase letters, digits, _ and -, at most 64 characters")
	}
	f, ref := readAppFields(obj)
	if ref != nil {
		return 0, nil, ref
	}
	repos, accepts, devs := orEmpty(f.repos), f.accepts, orEmpty(f.developers)
	if accepts == nil {
		accepts = []Accept{}
	}
	logins := sortLogins(f.githubLogins)
	// Each GitHub login's developer is one of the app's developers; a
	// create that sends no developers has none.
	if l := unmappedLogin(logins, devs); l != nil {
		return 0, nil, invalid(f.loginAt[l.Login], "names a developer the app doesn't list: each GitHub login's developer is one of the app's developers")
	}
	now := time.Now()
	if existing, ok := s.apps[name]; ok {
		same := slices.Equal(existing.repos, repos) && acceptsEqual(existing.accepts, accepts) && slices.Equal(existing.developers, devs) &&
			slices.Equal(existing.githubLogins, logins) &&
			existing.credential == f.credential && existing.whenSeatsFull == f.whenSeatsFull &&
			existing.workerImage == f.workerImage && existing.workerCPU == f.workerCPU && existing.workerMemory == f.workerMemory &&
			sameAutoReview(existing.autoReview, f.autoReview)
		if !same {
			return 0, nil, &refusal{http.StatusConflict, "name_taken", "an app with this name exists with other fields", "/name"}
		}
		return http.StatusOK, existing.json(), nil
	}
	if ref := s.refuseTakenRepos(repos, name); ref != nil {
		return 0, nil, ref
	}
	rec := &appRecord{name: name, repos: repos, accepts: accepts, developers: devs, githubLogins: logins,
		credential: f.credential, whenSeatsFull: f.whenSeatsFull,
		workerImage: f.workerImage, workerCPU: f.workerCPU, workerMemory: f.workerMemory,
		autoReview:  f.autoReview,
		lockVersion: 1, createdAt: now, updatedAt: now}
	s.apps[name] = rec
	return http.StatusCreated, rec.json(), nil
}

func (s *Server) patchApp(r *http.Request) (map[string]any, *refusal) {
	name := r.PathValue("name")
	rec, ok := s.apps[name]
	if !ok {
		ref := notFound("app")
		return nil, &ref
	}
	behind, ref := changePrecondition(r, rec.lockVersion)
	if ref != nil {
		return nil, ref
	}
	obj, ref := readObject(r)
	if ref != nil {
		return nil, ref
	}
	if _, ok := obj["name"]; ok {
		return nil, invalid("/name", "an app's name can't be changed")
	}
	for _, k := range appReadOnly {
		if _, ok := obj[k]; ok {
			return nil, invalid("/"+k, "this field is read-only")
		}
	}
	if ref := refuseUnknown(obj, "", appWritable...); ref != nil {
		return nil, ref
	}
	f, ref := readAppFields(obj)
	if ref != nil {
		return nil, ref
	}
	if behind {
		if rec.holds(f) {
			return rec.json(), nil
		}
		return nil, staleRefusal()
	}
	// Each GitHub login's developer is one of the app's developers, by what
	// the change leaves. Only a change that sends one of the two is held to
	// it, so a record an older AutoPilot left that way takes other changes.
	if (f.developers != nil || f.githubLogins != nil) && !rec.holds(f) {
		developers, logins := rec.developers, rec.githubLogins
		if f.developers != nil {
			developers = f.developers
		}
		if f.githubLogins != nil {
			logins = f.githubLogins
		}
		if unmappedLogin(logins, developers) != nil {
			if f.githubLogins != nil {
				return nil, invalid("/github_logins", "names a developer the app doesn't list: each GitHub login's developer is one of the app's developers")
			}
			return nil, invalid("/developers", "takes out a developer that github_logins still names; take their GitHub logins out too")
		}
	}
	if ref := s.refuseTakenRepos(f.repos, name); ref != nil {
		return nil, ref
	}
	// A PATCH replaces a list with the one it sends, as sent, so a change of
	// case alone is a change.
	changed := false
	if f.repos != nil && !slices.Equal(f.repos, rec.repos) {
		rec.repos, changed = f.repos, true
	}
	if f.accepts != nil && !acceptsEqual(f.accepts, rec.accepts) {
		rec.accepts, changed = f.accepts, true
	}
	if f.developers != nil && !slices.Equal(f.developers, rec.developers) {
		rec.developers, changed = f.developers, true
	}
	if f.githubLogins != nil && !slices.Equal(f.githubLogins, rec.githubLogins) {
		rec.githubLogins, changed = f.githubLogins, true
	}
	if f.policySent && (f.credential != rec.credential || f.whenSeatsFull != rec.whenSeatsFull) {
		rec.credential, rec.whenSeatsFull, changed = f.credential, f.whenSeatsFull, true
	}
	if f.imageSent && f.workerImage != rec.workerImage {
		rec.workerImage, changed = f.workerImage, true
	}
	if f.sizeSent && (f.workerCPU != rec.workerCPU || f.workerMemory != rec.workerMemory) {
		rec.workerCPU, rec.workerMemory, changed = f.workerCPU, f.workerMemory, true
	}
	if f.autoSent && !sameAutoReview(f.autoReview, rec.autoReview) {
		rec.autoReview, changed = f.autoReview, true
	}
	if changed {
		rec.lockVersion++
		rec.updatedAt = time.Now()
	}
	return rec.json(), nil
}

func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
