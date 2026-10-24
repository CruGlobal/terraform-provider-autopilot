package client

import (
	"cmp"
	"context"
	"slices"
)

// App is a unit of consent: the repositories it owns, the senders it accepts
// work from (with the kinds it accepts from each), the people who may start
// work for it themselves, and their GitHub logins. It also holds the app's
// credential policy, and the worker its tasks run in.
type App struct {
	Name       string   `json:"name"`
	Repos      []string `json:"repos"`
	Accepts    []Accept `json:"accepts"`
	Developers []string `json:"developers"`
	// GitHubLogins pairs GitHub logins with the developers they belong to,
	// for reviews started from GitHub. Nil when the AutoPilot predates the
	// field.
	GitHubLogins []GitHubLogin `json:"github_logins"`
	// Credential is "bedrock" or "seat_first", or nil when the app has no
	// credential policy of its own (or the AutoPilot predates the field).
	Credential *string `json:"credential"`
	// WhenSeatsFull is "wait" or "fall_back" with seat_first, and nil
	// otherwise.
	WhenSeatsFull *string `json:"when_seats_full"`
	// WorkerImage is "node", "ruby", "python" or "go", or nil for node (or
	// when the AutoPilot predates the field).
	WorkerImage *string `json:"worker_image"`
	// WorkerCPU and WorkerMemory are the size of the app's workers, in CPU
	// units and MiB, both or neither. Nil is the worker's own size.
	WorkerCPU    *int64 `json:"worker_cpu"`
	WorkerMemory *int64 `json:"worker_memory"`
	LockVersion  int64  `json:"lock_version"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

func (a *App) recordName() string {
	if a == nil {
		return ""
	}
	return a.Name
}

func (a *App) version() int64 {
	if a == nil {
		return 0
	}
	return a.LockVersion
}

// Accept is one sender an app accepts work from.
type Accept struct {
	Sender string   `json:"sender"`
	Kinds  []string `json:"kinds"`
	// CanDecide lets that sender's review-pr tasks approve or request changes
	// on the app's repositories.
	CanDecide bool `json:"can_decide"`
	// CanAutoMerge lets GitHub's auto-merge be turned on for the pull
	// requests that sender's implement-work-item and fix-error tasks open on
	// the app's repositories, when the task asks for it.
	CanAutoMerge bool `json:"can_auto_merge"`
	// CanDecideOwnPR lets a deciding review that a built-in sender makes for
	// a person decide on a pull request that may be that person's own, too.
	// False when the AutoPilot predates the field.
	CanDecideOwnPR bool `json:"can_decide_own_pr"`
	// ApproveUpTo is, with CanDecide, the most risk a pull request may carry
	// for that sender's reviews to approve it: low, medium or high. Empty
	// means no ceiling, which is also what AutoPilot's null reads as, and
	// what an AutoPilot that predates the field gives.
	ApproveUpTo string `json:"approve_up_to"`
}

// GitHubLogin is one developer's GitHub login. A review started from GitHub
// by that login runs in the developer's name.
type GitHubLogin struct {
	// Login is a GitHub login, in lowercase.
	Login string `json:"login"`
	// Developer is one of the app's developers: their sign-in login, an
	// email address in lowercase.
	Developer string `json:"developer"`
}

// CredentialPolicy is an app's credential policy. AutoPilot takes it as one:
// a write that sends credential sets when_seats_full too, so a
// when_seats_full left out is null.
type CredentialPolicy struct {
	// Credential is "bedrock" or "seat_first", or "" to take the app's own
	// policy away (sent as null).
	Credential string
	// WhenSeatsFull is "wait" or "fall_back" with seat_first, and ""
	// otherwise (not sent).
	WhenSeatsFull string
}

// WorkerSize is the size of an app's workers. AutoPilot takes it as one: a
// write that sends either field sets both, so one left out is null.
type WorkerSize struct {
	// CPU is in CPU units, 1024 to a vCPU, or 0 to take the size away (sent
	// as null, with Memory).
	CPU int64
	// Memory is in MiB, or 0 with CPU 0.
	Memory int64
}

// AppSpec is what a create or a change sends. A nil list is not sent at all;
// an empty, non-nil list is sent as []. A nil Policy, WorkerImage or Size is
// not sent either.
type AppSpec struct {
	Repos        []string
	Accepts      []Accept
	Developers   []string
	GitHubLogins []GitHubLogin
	Policy       *CredentialPolicy
	// WorkerImage is "node", "ruby", "python" or "go", or "" to take the
	// app back to node (sent as null).
	WorkerImage *string
	Size        *WorkerSize
}

func (s AppSpec) fields() Fields {
	f := Fields{}
	if s.Repos != nil {
		f["repos"] = sortedUnique(s.Repos)
	}
	if s.Accepts != nil {
		f["accepts"] = acceptsBody(s.Accepts)
	}
	if s.Developers != nil {
		f["developers"] = sortedUnique(s.Developers)
	}
	if s.GitHubLogins != nil {
		f["github_logins"] = normalizeGitHubLogins(s.GitHubLogins)
	}
	if s.Policy != nil {
		if s.Policy.Credential == "" {
			f["credential"] = nil
		} else {
			f["credential"] = s.Policy.Credential
		}
		if s.Policy.WhenSeatsFull != "" {
			f["when_seats_full"] = s.Policy.WhenSeatsFull
		}
	}
	if s.WorkerImage != nil {
		if *s.WorkerImage == "" {
			f["worker_image"] = nil
		} else {
			f["worker_image"] = *s.WorkerImage
		}
	}
	if s.Size != nil {
		if s.Size.CPU == 0 {
			f["worker_cpu"], f["worker_memory"] = nil, nil
		} else {
			f["worker_cpu"], f["worker_memory"] = s.Size.CPU, s.Size.Memory
		}
	}
	return f
}

// Empty reports whether the spec would send nothing.
func (s AppSpec) Empty() bool { return len(s.fields()) == 0 }

// acceptsBody is accepts as the API keeps them: sorted by sender, each with
// its kinds sorted. can_decide, can_auto_merge and can_decide_own_pr are sent
// only when true, so an entry that doesn't allow one never carries it, and an
// AutoPilot that predates can_auto_merge or can_decide_own_pr still takes an
// entry without it; false is the API's default for all three. approve_up_to
// is sent only when set, for the same reason: no ceiling is the API's
// default.
func acceptsBody(accepts []Accept) []map[string]any {
	sorted := normalizeAccepts(accepts)
	out := make([]map[string]any, 0, len(sorted))
	for _, a := range sorted {
		entry := map[string]any{"sender": a.Sender, "kinds": a.Kinds}
		if a.CanDecide {
			entry["can_decide"] = true
		}
		if a.CanAutoMerge {
			entry["can_auto_merge"] = true
		}
		if a.CanDecideOwnPR {
			entry["can_decide_own_pr"] = true
		}
		if a.ApproveUpTo != "" {
			entry["approve_up_to"] = a.ApproveUpTo
		}
		out = append(out, entry)
	}
	return out
}

func normalizeAccepts(accepts []Accept) []Accept {
	out := make([]Accept, 0, len(accepts))
	for _, a := range accepts {
		kinds := sortedUnique(a.Kinds)
		if kinds == nil {
			kinds = []string{}
		}
		out = append(out, Accept{
			Sender: a.Sender, Kinds: kinds,
			CanDecide: a.CanDecide, CanAutoMerge: a.CanAutoMerge, CanDecideOwnPR: a.CanDecideOwnPR,
			ApproveUpTo: a.ApproveUpTo,
		})
	}
	slices.SortFunc(out, func(x, y Accept) int { return cmp.Compare(x.Sender, y.Sender) })
	return out
}

// normalizeGitHubLogins is GitHub logins as the API keeps them: sorted by
// login, an entry listed twice exactly kept once. nil stays nil (not sent);
// an empty list stays an empty, non-nil list (sent as []). Two entries for
// one login with different developers are both kept, for AutoPilot to
// refuse; the plan refuses them first.
func normalizeGitHubLogins(logins []GitHubLogin) []GitHubLogin {
	if logins == nil {
		return nil
	}
	out := slices.Clone(logins)
	slices.SortFunc(out, func(x, y GitHubLogin) int {
		return cmp.Or(cmp.Compare(x.Login, y.Login), cmp.Compare(x.Developer, y.Developer))
	})
	return slices.Compact(out)
}

// reflectedIn reports whether an app holds everything the spec sends.
func (s AppSpec) reflectedIn(app *App) bool {
	if s.Repos != nil && !slices.Equal(sortedUnique(s.Repos), sortedUnique(app.Repos)) {
		return false
	}
	if s.Developers != nil && !slices.Equal(sortedUnique(s.Developers), sortedUnique(app.Developers)) {
		return false
	}
	if s.GitHubLogins != nil && !slices.Equal(normalizeGitHubLogins(s.GitHubLogins), normalizeGitHubLogins(app.GitHubLogins)) {
		return false
	}
	if s.Accepts != nil && !slices.EqualFunc(normalizeAccepts(s.Accepts), normalizeAccepts(app.Accepts),
		func(x, y Accept) bool {
			return x.Sender == y.Sender && x.CanDecide == y.CanDecide && x.CanAutoMerge == y.CanAutoMerge &&
				x.CanDecideOwnPR == y.CanDecideOwnPR && x.ApproveUpTo == y.ApproveUpTo && slices.Equal(x.Kinds, y.Kinds)
		}) {
		return false
	}
	if s.Policy != nil && (deref(app.Credential) != s.Policy.Credential || deref(app.WhenSeatsFull) != s.Policy.WhenSeatsFull) {
		return false
	}
	if s.WorkerImage != nil && deref(app.WorkerImage) != *s.WorkerImage {
		return false
	}
	if s.Size != nil && (derefInt(app.WorkerCPU) != s.Size.CPU || derefInt(app.WorkerMemory) != s.Size.Memory) {
		return false
	}
	return true
}

// deref is a nullable string's value, or "" for null.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// derefInt is a nullable number's value, or 0 for null.
func derefInt(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}

const appsPath = "/apps"

// ListApps lists the apps, by name.
func (c *Client) ListApps(ctx context.Context) ([]App, error) {
	return listRecords[App](ctx, c, appsPath)
}

// GetApp reads an app. An app that doesn't exist answers 404.
func (c *Client) GetApp(ctx context.Context, name string) (*App, error) {
	return getRecord[*App](ctx, c, recordPath(appsPath, name))
}

// CreateApp creates an app. Only the fields spec sets are sent; the others
// take the API's defaults (all empty).
func (c *Client) CreateApp(ctx context.Context, name string, spec AppSpec) (*App, CreateOutcome, error) {
	fields := spec.fields()
	fields["name"] = name
	return createRecord[*App](ctx, c, appsPath, name, fields)
}

// UpdateApp changes the fields spec sends; the others stay as they are.
func (c *Client) UpdateApp(ctx context.Context, name string, spec AppSpec, lockVersion int64) (*App, error) {
	return patchRecord(ctx, c, recordPath(appsPath, name), spec.fields(), lockVersion, spec.reflectedIn)
}

// DeleteApp removes an app at once. An app that is already gone is success.
func (c *Client) DeleteApp(ctx context.Context, name string, lockVersion int64) error {
	return deleteRecord(ctx, c, recordPath(appsPath, name), lockVersion)
}
