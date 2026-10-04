package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
)

// record is what every admin record shares: a name that is its key, and the
// lock_version a change has to name in If-Match.
type record interface {
	recordName() string
	version() int64
}

// CreateOutcome says what a create's answer meant.
type CreateOutcome int

const (
	// Created means AutoPilot made the record (201), or this call's own
	// earlier send made it and its answer was lost on the way back (a 200
	// after an unanswered send).
	Created CreateOutcome = iota
	// Adopted means the name already held a record with exactly these fields,
	// made by something other than this call: AutoPilot answered 200 although
	// every send was answered. Terraform now manages that record.
	Adopted
)

// recordPath is the route of one record in a collection.
func recordPath(collection, name string) string {
	return collection + "/" + url.PathEscape(name)
}

// createRecord POSTs fields to collection. A create is safe to send again
// (the name is the key, and an identical create is answered with the
// record), so the client's retries cover a lost answer, and a later apply
// that sends the same create again gets the same record back.
func createRecord[T record](ctx context.Context, c *Client, collection, name string, fields Fields) (T, CreateOutcome, error) {
	var zero T
	var trace sendTrace
	var raw json.RawMessage
	if err := c.Post(ctx, collection, fields, &raw, withSendTrace(&trace)); err != nil {
		return zero, Created, err
	}
	out, err := decodeRecord[T](http.MethodPost, collection, trace.status, raw)
	if err != nil {
		return zero, Created, err
	}
	if out.recordName() != name {
		return zero, Created, &Error{Method: http.MethodPost, Path: collection, Status: trace.status,
			Message: fmt.Sprintf("a create for %q was answered with a record named %q", name, out.recordName())}
	}
	outcome := Created
	if trace.status == http.StatusOK && !trace.earlierSendUnanswered {
		outcome = Adopted
	}
	return out, outcome, nil
}

// listRecords GETs a collection: {"results": [...]}.
func listRecords[T any](ctx context.Context, c *Client, collection string) ([]T, error) {
	var out struct {
		Results []T `json:"results"`
	}
	if err := c.Get(ctx, collection, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// getRecord GETs one record.
func getRecord[T record](ctx context.Context, c *Client, path string) (T, error) {
	var zero T
	var raw json.RawMessage
	if err := c.Get(ctx, path, &raw); err != nil {
		return zero, err
	}
	out, err := decodeRecord[T](http.MethodGet, path, http.StatusOK, raw)
	if err != nil {
		return zero, err
	}
	return out, nil
}

// patchRecord PATCHes fields under If-Match: "<lockVersion>".
//
// The client sends a PATCH again when an attempt's answer is lost. AutoPilot
// answers a PATCH that names the version just before the record's, when the
// record already holds exactly what it sends, with 200: that is the same
// change, applied by the attempt whose answer was lost. So the repeat simply
// succeeds.
//
// The rest is a fallback for an AutoPilot that answers such a repeat with
// stale_object instead. A stale_object after an unanswered send is checked:
// when the record now stands exactly one version past lockVersion and holds
// what this PATCH sent (applied), the change is this call's own and the
// record is returned. Any other stale_object is returned as it is, for the
// caller to report without overwriting anything.
func patchRecord[T record](ctx context.Context, c *Client, path string, fields Fields, lockVersion int64, applied func(T) bool) (T, error) {
	var zero T
	var raw json.RawMessage
	err := c.Patch(ctx, path, fields, &raw, WithIfMatch(lockVersion))
	if err != nil {
		apiErr, ok := asError(err)
		if !ok || apiErr.Code != CodeStaleObject || !apiErr.EarlierSendUnanswered {
			return zero, err
		}
		current, gerr := getRecord[T](ctx, c, path)
		if gerr != nil || current.version() != lockVersion+1 || !applied(current) {
			return zero, err
		}
		return current, nil
	}
	return decodeRecord[T](http.MethodPatch, path, http.StatusOK, raw)
}

// deleteRecord DELETEs a record under If-Match. A record AutoPilot says is
// already gone (not_found) is success.
func deleteRecord(ctx context.Context, c *Client, path string, lockVersion int64) error {
	err := c.Delete(ctx, path, nil, WithIfMatch(lockVersion))
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}

// decodeRecord decodes a record and insists it is usable: it has a name and
// a lock_version, which every record the API answers with carries.
func decodeRecord[T record](method, path string, status int, raw json.RawMessage) (T, error) {
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, &Error{Method: method, Path: path, Status: status, Err: err,
			Message: fmt.Sprintf("response could not be decoded (%s): %s", shapeOf(raw), err)}
	}
	// recordName and version are nil-safe, so a body of `null` lands here too.
	if out.recordName() == "" || out.version() < 1 {
		return out, &Error{Method: method, Path: path, Status: status,
			Message: fmt.Sprintf("response is not a record with a name and a lock_version (%s). "+
				"Check that the endpoint is AutoPilot's admin API", shapeOf(raw))}
	}
	return out, nil
}

// sortedUnique returns the values sorted, without repeats: the form the API
// keeps every list in, so what is read back compares equal to what was sent.
// nil stays nil (a field that is not sent); an empty list stays an empty,
// non-nil list (a field sent as []).
func sortedUnique(values []string) []string {
	if values == nil {
		return nil
	}
	out := slices.Clone(values)
	slices.Sort(out)
	return slices.Compact(out)
}
