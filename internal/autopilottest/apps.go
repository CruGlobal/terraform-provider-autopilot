package autopilottest

import (
	"cmp"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

var (
	appNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	repoPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`)
	// A login in lowercase ASCII: printable ASCII but uppercase, @ and space,
	// then @, then a domain with a dot.
	loginPattern    = regexp.MustCompile(`^[\x21-\x3f\x5b-\x7e]+@[\x21-\x3f\x5b-\x7e]+\.[\x21-\x3f\x5b-\x7e]+$`)
	maxAppName      = 64
	reviewKind      = "review-pr"
	maxRepos        = 50
	maxAccepts      = 20
	maxDevelopers   = 100
	appWritable     = []string{"repos", "accepts", "developers"}
	appReadOnly     = []string{"lock_version", "created_at", "updated_at"}
	acceptWritable  = []string{"sender", "kinds", "can_decide"}
	appCreateFields = append([]string{"name"}, appWritable...)
)

// Accept is one sender an app accepts work from.
type Accept struct {
	Sender    string
	Kinds     []string
	CanDecide bool
}

// appRecord is an app as the fake stores it.
type appRecord struct {
	name                 string
	repos                []string
	accepts              []Accept
	developers           []string
	lockVersion          int64
	createdAt, updatedAt time.Time
}

// AppView is a copy of a stored app, for assertions.
type AppView struct {
	Name        string
	Repos       []string
	Accepts     []Accept
	Developers  []string
	LockVersion int64
}

func (rec *appRecord) view() AppView {
	return AppView{
		Name: rec.name, Repos: slices.Clone(rec.repos), Accepts: cloneAccepts(rec.accepts),
		Developers: slices.Clone(rec.developers), LockVersion: rec.lockVersion,
	}
}

func cloneAccepts(in []Accept) []Accept {
	out := make([]Accept, 0, len(in))
	for _, a := range in {
		out = append(out, Accept{Sender: a.Sender, Kinds: slices.Clone(a.Kinds), CanDecide: a.CanDecide})
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
		return a.Sender == b.Sender && a.CanDecide == b.CanDecide && slices.Equal(a.Kinds, b.Kinds)
	})
}

func (rec *appRecord) json() map[string]any {
	accepts := make([]any, 0, len(rec.accepts))
	for _, a := range sortAccepts(rec.accepts) {
		accepts = append(accepts, map[string]any{"sender": a.Sender, "kinds": a.Kinds, "can_decide": a.CanDecide})
	}
	return map[string]any{
		"name":         rec.name,
		"repos":        sortedUnique(rec.repos),
		"accepts":      accepts,
		"developers":   sortedUnique(rec.developers),
		"lock_version": rec.lockVersion,
		"created_at":   timestamp(rec.createdAt),
		"updated_at":   timestamp(rec.updatedAt),
	}
}

// --- test controls ----------------------------------------------------------

// SeedApp stores an app directly, as if another root made it.
func (s *Server) SeedApp(v AppView) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.apps[v.Name] = &appRecord{
		name: v.Name, repos: sortedUniqueFold(v.Repos), accepts: sortAccepts(v.Accepts),
		developers: sortedUnique(v.Developers), lockVersion: 1, createdAt: now, updatedAt: now,
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
// list was not sent.
type appFields struct {
	repos      []string
	accepts    []Accept
	developers []string
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
			canDecide := false
			if rawDecide, ok := e["can_decide"]; ok {
				if err := json.Unmarshal(rawDecide, &canDecide); err != nil {
					return f, invalid(at+"/can_decide", "must be true or false")
				}
			}
			if canDecide && !slices.Contains(kinds, reviewKind) {
				return f, invalid(at+"/can_decide", "can_decide is only for an entry whose kinds include review-pr")
			}
			entry := Accept{Sender: sender, Kinds: sortedUnique(kinds), CanDecide: canDecide}
			// An entry sent twice exactly is kept once; two different entries
			// for one sender are refused.
			if prior, dup := seen[sender]; dup {
				if prior.CanDecide == entry.CanDecide && slices.Equal(prior.Kinds, entry.Kinds) {
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
			if !loginPattern.MatchString(d) {
				return f, invalid("/developers/"+itoa(i), "a developer is their login: an email address in lowercase ASCII")
			}
		}
		f.developers = sortedUnique(devs)
	}
	return f, nil
}

// holds reports whether the app already holds everything f sends.
func (rec *appRecord) holds(f appFields) bool {
	return (f.repos == nil || slices.Equal(f.repos, rec.repos)) &&
		(f.accepts == nil || acceptsEqual(f.accepts, rec.accepts)) &&
		(f.developers == nil || slices.Equal(f.developers, rec.developers))
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
	now := time.Now()
	if existing, ok := s.apps[name]; ok {
		same := slices.Equal(existing.repos, repos) && acceptsEqual(existing.accepts, accepts) && slices.Equal(existing.developers, devs)
		if !same {
			return 0, nil, &refusal{http.StatusConflict, "name_taken", "an app with this name exists with other fields", "/name"}
		}
		return http.StatusOK, existing.json(), nil
	}
	if ref := s.refuseTakenRepos(repos, name); ref != nil {
		return 0, nil, ref
	}
	rec := &appRecord{name: name, repos: repos, accepts: accepts, developers: devs, lockVersion: 1, createdAt: now, updatedAt: now}
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
