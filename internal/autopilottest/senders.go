package autopilottest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"regexp"
	"slices"
	"time"
	"unicode/utf8"
)

// KnownKinds are the kinds of task the fake knows. The real list is longer
// and grows; these are enough to exercise the rules.
var KnownKinds = []string{"fix-error", "implement-work-item", "research", "review-pr"}

// DefaultKinds is what a sender may send when its create names no kinds.
var DefaultKinds = []string{"implement-work-item"}

// RotationOverlap is how long AutoPilot keeps a sender's old secrets after a
// change sends new ones.
const RotationOverlap = 24 * time.Hour

var (
	senderNamePattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	branchPrefixPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*/$`)
	hostLabelPattern    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// senderRecord is a sender as the fake stores it, secrets included.
type senderRecord struct {
	name                   string
	callbackHosts          []string
	kinds                  []string
	branchPrefix           string
	requestSecret          string
	callbackSecret         string
	previousRequestSecret  string
	previousCallbackSecret string
	secretsChangedAt       time.Time
	previousSecretsUntil   *time.Time
	lockVersion            int64
	createdAt, updatedAt   time.Time
	retired                bool
}

// SenderView is a copy of a stored sender, secrets included, for assertions.
type SenderView struct {
	Name                   string
	CallbackHosts          []string
	Kinds                  []string
	BranchPrefix           string
	RequestSecret          string
	CallbackSecret         string
	PreviousRequestSecret  string
	PreviousCallbackSecret string
	PreviousSecretsUntil   *time.Time
	LockVersion            int64
	Retired                bool
}

func (rec *senderRecord) view() SenderView {
	return SenderView{
		Name: rec.name, CallbackHosts: slices.Clone(rec.callbackHosts), Kinds: slices.Clone(rec.kinds),
		BranchPrefix: rec.branchPrefix, RequestSecret: rec.requestSecret, CallbackSecret: rec.callbackSecret,
		PreviousRequestSecret: rec.previousRequestSecret, PreviousCallbackSecret: rec.previousCallbackSecret,
		PreviousSecretsUntil: rec.previousSecretsUntil, LockVersion: rec.lockVersion, Retired: rec.retired,
	}
}

// Fingerprint is the API's fingerprint of a secret.
func Fingerprint(secret string) string {
	sum := sha256.Sum256([]byte("autopilot-fingerprint:" + secret))
	return hex.EncodeToString(sum[:])[:16]
}

func (rec *senderRecord) json(now time.Time) map[string]any {
	var until any
	if rec.previousSecretsUntil != nil && now.Before(*rec.previousSecretsUntil) {
		until = timestamp(*rec.previousSecretsUntil)
	}
	return map[string]any{
		"name":                        rec.name,
		"callback_hosts":              sortedUnique(rec.callbackHosts),
		"kinds":                       sortedUnique(rec.kinds),
		"branch_prefix":               rec.branchPrefix,
		"request_secret_fingerprint":  Fingerprint(rec.requestSecret),
		"callback_secret_fingerprint": Fingerprint(rec.callbackSecret),
		"secrets_changed_at":          timestamp(rec.secretsChangedAt),
		"previous_secrets_until":      until,
		"lock_version":                rec.lockVersion,
		"created_at":                  timestamp(rec.createdAt),
		"updated_at":                  timestamp(rec.updatedAt),
	}
}

// --- test controls ----------------------------------------------------------

// SenderSeed is a sender put straight into the fake, as if another root made it.
type SenderSeed struct {
	Name           string
	CallbackHosts  []string
	Kinds          []string // DefaultKinds when nil
	BranchPrefix   string   // the name and a / when empty
	RequestSecret  string
	CallbackSecret string
}

// SeedSender stores a sender directly.
func (s *Server) SeedSender(seed SenderSeed) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	kinds := seed.Kinds
	if kinds == nil {
		kinds = DefaultKinds
	}
	prefix := seed.BranchPrefix
	if prefix == "" {
		prefix = seed.Name + "/"
	}
	s.senders[seed.Name] = &senderRecord{
		name: seed.Name, callbackHosts: sortedUnique(seed.CallbackHosts), kinds: sortedUnique(kinds),
		branchPrefix: prefix, requestSecret: seed.RequestSecret, callbackSecret: seed.CallbackSecret,
		secretsChangedAt: now, lockVersion: 1, createdAt: now, updatedAt: now,
	}
}

// Sender returns a copy of a stored sender (retired ones included).
func (s *Server) Sender(name string) (SenderView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.senders[name]
	if !ok {
		return SenderView{}, false
	}
	return rec.view(), true
}

// ChangeSenderOutOfBand applies change to a live sender and bumps its
// lock_version, as a write from outside this Terraform would.
func (s *Server) ChangeSenderOutOfBand(name string, change func(v *SenderView)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.senders[name]
	if !ok {
		return
	}
	v := rec.view()
	change(&v)
	now := time.Now()
	if v.RequestSecret != rec.requestSecret || v.CallbackSecret != rec.callbackSecret {
		rec.previousRequestSecret, rec.previousCallbackSecret = rec.requestSecret, rec.callbackSecret
		until := now.Add(RotationOverlap)
		rec.previousSecretsUntil = &until
		rec.secretsChangedAt = now
	}
	rec.callbackHosts, rec.kinds, rec.branchPrefix = sortedUnique(v.CallbackHosts), sortedUnique(v.Kinds), v.BranchPrefix
	rec.requestSecret, rec.callbackSecret = v.RequestSecret, v.CallbackSecret
	rec.lockVersion++
	rec.updatedAt = now
}

// RemoveSender removes a sender for good, as AutoPilot does once a retired
// sender's tasks are all finished, or as a delete from outside would.
func (s *Server) RemoveSender(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.senders, name)
}

// --- routes -------------------------------------------------------------------

func (s *Server) senderRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/admin/senders", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		names := make([]string, 0, len(s.senders))
		for name, rec := range s.senders {
			if !rec.retired {
				names = append(names, name)
			}
		}
		slices.Sort(names)
		now := time.Now()
		results := make([]any, 0, len(names))
		for _, name := range names {
			results = append(results, s.senders[name].json(now))
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
	})

	mux.HandleFunc("GET /v1/admin/senders/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		rec, ok := s.senders[r.PathValue("name")]
		if !ok || rec.retired {
			writeError(w, notFound("sender"))
			return
		}
		writeJSON(w, http.StatusOK, rec.json(time.Now()))
	})

	mux.HandleFunc("POST /v1/admin/senders", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		status, body, ref := s.createSender(r)
		if ref != nil {
			writeError(w, *ref)
			return
		}
		writeJSON(w, status, body)
	})

	mux.HandleFunc("PATCH /v1/admin/senders/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		body, ref := s.patchSender(r)
		if ref != nil {
			writeError(w, *ref)
			return
		}
		writeJSON(w, http.StatusOK, body)
	})

	mux.HandleFunc("DELETE /v1/admin/senders/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		name := r.PathValue("name")
		rec, ok := s.senders[name]
		if !ok || rec.retired {
			writeError(w, notFound("sender"))
			return
		}
		if ref := ifMatch(r, rec.lockVersion, false); ref != nil {
			writeError(w, *ref)
			return
		}
		if s.retiredLinger {
			rec.retired = true
		} else {
			delete(s.senders, name)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("DELETE /v1/admin/senders/{name}/previous-secrets", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		rec, ok := s.senders[r.PathValue("name")]
		if !ok || rec.retired {
			writeError(w, notFound("sender"))
			return
		}
		if rec.previousSecretsUntil != nil {
			rec.previousRequestSecret, rec.previousCallbackSecret = "", ""
			rec.previousSecretsUntil = nil
			rec.lockVersion++
			rec.updatedAt = time.Now()
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func notFound(what string) refusal {
	return refusal{http.StatusNotFound, "not_found", "no such " + what, ""}
}

// senderFields are the writable fields of a sender, as read from a body.
type senderFields struct {
	callbackHosts  []string
	kinds          []string
	branchPrefix   *string
	requestSecret  *string
	callbackSecret *string
}

var senderReadOnly = []string{
	"request_secret_fingerprint", "callback_secret_fingerprint", "secrets_changed_at",
	"previous_secrets_until", "lock_version", "created_at", "updated_at",
}

func readSenderFields(obj map[string]json.RawMessage) (senderFields, *refusal) {
	var f senderFields
	if raw, ok := obj["callback_hosts"]; ok {
		hosts, ref := stringList(raw, "/callback_hosts")
		if ref != nil {
			return f, ref
		}
		if len(hosts) > 10 {
			return f, invalid("/callback_hosts", "a sender has at most 10 callback hosts")
		}
		for i, h := range hosts {
			if !validCallbackHost(h) {
				return f, invalid("/callback_hosts/"+itoa(i), "a callback host is a lowercase DNS name with at least one dot, not an IP address and not localhost")
			}
		}
		f.callbackHosts = sortedUnique(hosts)
	}
	if raw, ok := obj["kinds"]; ok {
		kinds, ref := stringList(raw, "/kinds")
		if ref != nil {
			return f, ref
		}
		if len(kinds) == 0 {
			return f, invalid("/kinds", "a sender may send at least one kind")
		}
		for i, k := range kinds {
			if !slices.Contains(KnownKinds, k) {
				return f, invalid("/kinds/"+itoa(i), "AutoPilot doesn't know this kind")
			}
		}
		f.kinds = sortedUnique(kinds)
	}
	if raw, ok := obj["branch_prefix"]; ok {
		prefix, ref := stringValue(raw, "/branch_prefix")
		if ref != nil {
			return f, ref
		}
		if !branchPrefixPattern.MatchString(prefix) || len(prefix) > 41 {
			return f, invalid("/branch_prefix", "a branch prefix is ^[a-z][a-z0-9-]*/$, at most 41 characters")
		}
		f.branchPrefix = &prefix
	}
	for _, k := range []string{"request_secret", "callback_secret"} {
		raw, ok := obj[k]
		if !ok {
			continue
		}
		secret, ref := stringValue(raw, "/"+k)
		if ref != nil {
			return f, ref
		}
		if utf8.RuneCountInString(secret) < 32 {
			return f, invalid("/"+k, "a secret is at least 32 characters")
		}
		if k == "request_secret" {
			f.requestSecret = &secret
		} else {
			f.callbackSecret = &secret
		}
	}
	if (f.requestSecret == nil) != (f.callbackSecret == nil) {
		missing := "/request_secret"
		if f.callbackSecret == nil {
			missing = "/callback_secret"
		}
		return f, invalid(missing, "send both secrets together; one alone is refused")
	}
	if f.requestSecret != nil && *f.requestSecret == *f.callbackSecret {
		return f, invalid("/callback_secret", "the two secrets must differ")
	}
	return f, nil
}

// holds reports whether the sender already holds everything f sends,
// secrets compared by fingerprint.
func (rec *senderRecord) holds(f senderFields) bool {
	if f.callbackHosts != nil && !slices.Equal(f.callbackHosts, rec.callbackHosts) {
		return false
	}
	if f.kinds != nil && !slices.Equal(f.kinds, rec.kinds) {
		return false
	}
	if f.branchPrefix != nil && *f.branchPrefix != rec.branchPrefix {
		return false
	}
	if f.requestSecret != nil && (Fingerprint(*f.requestSecret) != Fingerprint(rec.requestSecret) ||
		Fingerprint(*f.callbackSecret) != Fingerprint(rec.callbackSecret)) {
		return false
	}
	return true
}

func validCallbackHost(h string) bool {
	if h == "localhost" || net.ParseIP(h) != nil || len(h) > 253 {
		return false
	}
	labels := splitDots(h)
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if !hostLabelPattern.MatchString(l) {
			return false
		}
	}
	return true
}

func (s *Server) prefixTaken(prefix, except string) bool {
	for name, rec := range s.senders {
		if name != except && rec.branchPrefix == prefix {
			return true
		}
	}
	return false
}

func (s *Server) createSender(r *http.Request) (int, map[string]any, *refusal) {
	obj, ref := readObject(r)
	if ref != nil {
		return 0, nil, ref
	}
	if ref := refuseUnknown(obj, "", "name", "callback_hosts", "kinds", "branch_prefix", "request_secret", "callback_secret"); ref != nil {
		return 0, nil, ref
	}
	raw, ok := obj["name"]
	if !ok {
		return 0, nil, invalid("/name", "a sender needs a name")
	}
	name, ref := stringValue(raw, "/name")
	if ref != nil {
		return 0, nil, ref
	}
	if !senderNamePattern.MatchString(name) {
		return 0, nil, invalid("/name", "a sender's name is a lowercase letter, then lowercase letters, digits and -, at most 40 characters")
	}
	f, ref := readSenderFields(obj)
	if ref != nil {
		return 0, nil, ref
	}
	if f.requestSecret == nil {
		return 0, nil, invalid("/request_secret", "a new sender needs both secrets")
	}
	if s.reserved[name] {
		return 0, nil, &refusal{http.StatusConflict, "name_reserved", "this sender name is reserved", "/name"}
	}
	hosts := f.callbackHosts
	if hosts == nil {
		hosts = []string{}
	}
	kinds := f.kinds
	if kinds == nil {
		kinds = DefaultKinds
	}
	prefix := name + "/"
	if f.branchPrefix != nil {
		prefix = *f.branchPrefix
	}
	now := time.Now()
	if existing, ok := s.senders[name]; ok {
		if existing.retired {
			return 0, nil, &refusal{http.StatusConflict, "name_retired", "this sender is retired and still finishing its tasks", "/name"}
		}
		same := slices.Equal(existing.callbackHosts, sortedUnique(hosts)) && slices.Equal(existing.kinds, sortedUnique(kinds)) &&
			existing.branchPrefix == prefix && existing.requestSecret == *f.requestSecret && existing.callbackSecret == *f.callbackSecret
		if !same {
			return 0, nil, &refusal{http.StatusConflict, "name_taken", "a sender with this name exists with other fields", "/name"}
		}
		return http.StatusOK, existing.json(now), nil
	}
	if s.prefixTaken(prefix, name) {
		return 0, nil, &refusal{http.StatusConflict, "branch_prefix_taken", "another sender has this branch prefix", "/branch_prefix"}
	}
	rec := &senderRecord{
		name: name, callbackHosts: sortedUnique(hosts), kinds: sortedUnique(kinds), branchPrefix: prefix,
		requestSecret: *f.requestSecret, callbackSecret: *f.callbackSecret,
		secretsChangedAt: now, lockVersion: 1, createdAt: now, updatedAt: now,
	}
	s.senders[name] = rec
	return http.StatusCreated, rec.json(now), nil
}

func (s *Server) patchSender(r *http.Request) (map[string]any, *refusal) {
	name := r.PathValue("name")
	rec, ok := s.senders[name]
	if !ok || rec.retired {
		ref := notFound("sender")
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
		return nil, invalid("/name", "a sender's name can't be changed")
	}
	for _, k := range senderReadOnly {
		if _, ok := obj[k]; ok {
			return nil, invalid("/"+k, "this field is read-only")
		}
	}
	if ref := refuseUnknown(obj, "", "callback_hosts", "kinds", "branch_prefix", "request_secret", "callback_secret"); ref != nil {
		return nil, ref
	}
	f, ref := readSenderFields(obj)
	if ref != nil {
		return nil, ref
	}
	now := time.Now()
	if behind {
		if rec.holds(f) {
			return rec.json(now), nil
		}
		return nil, staleRefusal()
	}
	if f.branchPrefix != nil && s.prefixTaken(*f.branchPrefix, name) {
		return nil, &refusal{http.StatusConflict, "branch_prefix_taken", "another sender has this branch prefix", "/branch_prefix"}
	}

	changed := false
	if f.callbackHosts != nil && !slices.Equal(f.callbackHosts, rec.callbackHosts) {
		rec.callbackHosts, changed = f.callbackHosts, true
	}
	if f.kinds != nil && !slices.Equal(f.kinds, rec.kinds) {
		rec.kinds, changed = f.kinds, true
	}
	if f.branchPrefix != nil && *f.branchPrefix != rec.branchPrefix {
		rec.branchPrefix, changed = *f.branchPrefix, true
	}
	if f.requestSecret != nil && (*f.requestSecret != rec.requestSecret || *f.callbackSecret != rec.callbackSecret) {
		rec.previousRequestSecret, rec.previousCallbackSecret = rec.requestSecret, rec.callbackSecret
		rec.requestSecret, rec.callbackSecret = *f.requestSecret, *f.callbackSecret
		until := now.Add(RotationOverlap)
		rec.previousSecretsUntil = &until
		rec.secretsChangedAt = now
		changed = true
	}
	if changed {
		rec.lockVersion++
		rec.updatedAt = now
	}
	return rec.json(now), nil
}
