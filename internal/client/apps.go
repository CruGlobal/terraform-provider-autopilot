package client

import (
	"cmp"
	"context"
	"slices"
)

// App is a unit of consent: the repositories it owns, the senders it accepts
// work from (with the kinds it accepts from each), and the people who may
// start work for it themselves. It also holds the app's credential policy.
type App struct {
	Name       string   `json:"name"`
	Repos      []string `json:"repos"`
	Accepts    []Accept `json:"accepts"`
	Developers []string `json:"developers"`
	// Credential is "bedrock" or "seat_first", or nil when the app has no
	// credential policy of its own (or the AutoPilot predates the field).
	Credential *string `json:"credential"`
	// WhenSeatsFull is "wait" or "fall_back" with seat_first, and nil
	// otherwise.
	WhenSeatsFull *string `json:"when_seats_full"`
	LockVersion   int64   `json:"lock_version"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
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

// AppSpec is what a create or a change sends. A nil list is not sent at all;
// an empty, non-nil list is sent as []. A nil Policy is not sent either.
type AppSpec struct {
	Repos      []string
	Accepts    []Accept
	Developers []string
	Policy     *CredentialPolicy
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
	return f
}

// Empty reports whether the spec would send nothing.
func (s AppSpec) Empty() bool { return len(s.fields()) == 0 }

// acceptsBody is accepts as the API keeps them: sorted by sender, each with
// its kinds sorted. can_decide is sent only when true, so an entry whose
// kinds lack review-pr never carries it; false is the API's default.
func acceptsBody(accepts []Accept) []map[string]any {
	sorted := normalizeAccepts(accepts)
	out := make([]map[string]any, 0, len(sorted))
	for _, a := range sorted {
		entry := map[string]any{"sender": a.Sender, "kinds": a.Kinds}
		if a.CanDecide {
			entry["can_decide"] = true
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
		out = append(out, Accept{Sender: a.Sender, Kinds: kinds, CanDecide: a.CanDecide})
	}
	slices.SortFunc(out, func(x, y Accept) int { return cmp.Compare(x.Sender, y.Sender) })
	return out
}

// reflectedIn reports whether an app holds everything the spec sends.
func (s AppSpec) reflectedIn(app *App) bool {
	if s.Repos != nil && !slices.Equal(sortedUnique(s.Repos), sortedUnique(app.Repos)) {
		return false
	}
	if s.Developers != nil && !slices.Equal(sortedUnique(s.Developers), sortedUnique(app.Developers)) {
		return false
	}
	if s.Accepts != nil && !slices.EqualFunc(normalizeAccepts(s.Accepts), normalizeAccepts(app.Accepts),
		func(x, y Accept) bool {
			return x.Sender == y.Sender && x.CanDecide == y.CanDecide && slices.Equal(x.Kinds, y.Kinds)
		}) {
		return false
	}
	if s.Policy != nil && (deref(app.Credential) != s.Policy.Credential || deref(app.WhenSeatsFull) != s.Policy.WhenSeatsFull) {
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
