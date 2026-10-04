package client

import (
	"context"
	"slices"
)

// Sender is a system that sends AutoPilot tasks: its name, the hosts its
// callbacks may go to, the kinds of task it may send, the prefix its
// branches start with, and fingerprints of its two secrets. AutoPilot never
// sends the secrets back.
type Sender struct {
	Name                      string   `json:"name"`
	CallbackHosts             []string `json:"callback_hosts"`
	Kinds                     []string `json:"kinds"`
	BranchPrefix              string   `json:"branch_prefix"`
	RequestSecretFingerprint  string   `json:"request_secret_fingerprint"`
	CallbackSecretFingerprint string   `json:"callback_secret_fingerprint"`
	SecretsChangedAt          string   `json:"secrets_changed_at"`
	PreviousSecretsUntil      *string  `json:"previous_secrets_until"`
	LockVersion               int64    `json:"lock_version"`
	CreatedAt                 string   `json:"created_at"`
	UpdatedAt                 string   `json:"updated_at"`
}

func (s *Sender) recordName() string {
	if s == nil {
		return ""
	}
	return s.Name
}

func (s *Sender) version() int64 {
	if s == nil {
		return 0
	}
	return s.LockVersion
}

// SenderSecrets are a sender's two secrets. They are always sent together: a
// create needs both, and a change sends both again to rotate them.
type SenderSecrets struct {
	Request  string
	Callback string
}

// SenderSpec is what a create or a change sends. A nil list, a nil
// BranchPrefix or nil Secrets is not sent at all; an empty, non-nil list is
// sent as [].
type SenderSpec struct {
	CallbackHosts []string
	Kinds         []string
	BranchPrefix  *string
	Secrets       *SenderSecrets
}

func (s SenderSpec) fields() Fields {
	f := Fields{}
	if s.CallbackHosts != nil {
		f["callback_hosts"] = sortedUnique(s.CallbackHosts)
	}
	if s.Kinds != nil {
		f["kinds"] = sortedUnique(s.Kinds)
	}
	if s.BranchPrefix != nil {
		f["branch_prefix"] = *s.BranchPrefix
	}
	if s.Secrets != nil {
		f["request_secret"] = s.Secrets.Request
		f["callback_secret"] = s.Secrets.Callback
	}
	return f
}

// Empty reports whether the spec would send nothing.
func (s SenderSpec) Empty() bool { return len(s.fields()) == 0 }

// reflectedIn reports whether a sender holds everything the spec sends.
func (s SenderSpec) reflectedIn(sender *Sender) bool {
	if s.CallbackHosts != nil && !slices.Equal(sortedUnique(s.CallbackHosts), sortedUnique(sender.CallbackHosts)) {
		return false
	}
	if s.Kinds != nil && !slices.Equal(sortedUnique(s.Kinds), sortedUnique(sender.Kinds)) {
		return false
	}
	if s.BranchPrefix != nil && *s.BranchPrefix != sender.BranchPrefix {
		return false
	}
	if s.Secrets != nil && (Fingerprint(s.Secrets.Request) != sender.RequestSecretFingerprint ||
		Fingerprint(s.Secrets.Callback) != sender.CallbackSecretFingerprint) {
		return false
	}
	return true
}

const sendersPath = "/senders"

// ListSenders lists the senders that aren't retired, by name.
func (c *Client) ListSenders(ctx context.Context) ([]Sender, error) {
	return listRecords[Sender](ctx, c, sendersPath)
}

// GetSender reads a sender. A sender that doesn't exist, or that is retired,
// answers 404.
func (c *Client) GetSender(ctx context.Context, name string) (*Sender, error) {
	return getRecord[*Sender](ctx, c, recordPath(sendersPath, name))
}

// CreateSender creates a sender. spec.Secrets is required.
func (c *Client) CreateSender(ctx context.Context, name string, spec SenderSpec) (*Sender, CreateOutcome, error) {
	fields := spec.fields()
	fields["name"] = name
	return createRecord[*Sender](ctx, c, sendersPath, name, fields)
}

// UpdateSender changes the fields spec sends; the others stay as they are.
// Sending new secrets rotates them, and AutoPilot keeps the old pair for an
// overlap; sending the same secrets again changes nothing.
func (c *Client) UpdateSender(ctx context.Context, name string, spec SenderSpec, lockVersion int64) (*Sender, error) {
	return patchRecord(ctx, c, recordPath(sendersPath, name), spec.fields(), lockVersion, spec.reflectedIn)
}

// DeleteSender retires a sender: it can send no new tasks, its tasks already
// taken run on, and AutoPilot removes it for good once none is left. It reads
// as 404 from then on. A sender that is already gone is success.
func (c *Client) DeleteSender(ctx context.Context, name string, lockVersion int64) error {
	return deleteRecord(ctx, c, recordPath(sendersPath, name), lockVersion)
}
