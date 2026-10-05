package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Codes AutoPilot's admin API sends in a refusal. The message is free to
// change; these slugs are the contract a provider branches on.
const (
	CodeBadRequest           = "bad_request"           // 400: the body could not be read
	CodeNotJSON              = "not_json"              // 400: the body is not a JSON object
	CodeUnauthorized         = "unauthorized"          // 401: no admin token, the wrong one, or no admin API here
	CodeNotFound             = "not_found"             // 404: no such record, a retired sender, or no such admin route
	CodeMethodNotAllowed     = "method_not_allowed"    // 405: the route doesn't take that method
	CodeNameTaken            = "name_taken"            // 409: a create for a name that exists with other fields
	CodeNameReserved         = "name_reserved"         // 409: a create for a reserved name
	CodeNameRetired          = "name_retired"          // 409: a create for a deleted sender's name, other than its revival
	CodeBranchPrefixTaken    = "branch_prefix_taken"   // 409: another sender, or a tombstone, has that branch prefix
	CodeRepoTaken            = "repo_taken"            // 409: another app owns that repository; the message names it
	CodeStaleObject          = "stale_object"          // 409: If-Match does not match the record's lock_version
	CodeTooLarge             = "too_large"             // 413: the body is over 64 KiB
	CodeInvalidAttribute     = "invalid_attribute"     // 422: a field breaks its rules, or is unknown; Field names it
	CodePreconditionRequired = "precondition_required" // 428: a PATCH without If-Match
	CodeUnavailable          = "unavailable"           // 503: retry with backoff
)

// HasCode reports whether err is an API error carrying the given code.
func HasCode(err error, code string) bool {
	e, ok := asError(err)
	return ok && e.Code == code
}

// Error is any non-2xx answer (or a transport failure) from the API. Status is
// 0 for transport failures. Code is empty when the server did not send one.
type Error struct {
	Method  string
	Path    string
	Status  int
	Code    string
	Message string
	// Field is the JSON pointer the refusal names (for example
	// "/branch_prefix"), or empty.
	Field      string
	RetryAfter time.Duration
	Err        error

	// Preconditioned records that the failed request carried If-Match.
	Preconditioned bool

	// EarlierSendUnanswered records that an earlier attempt at this same
	// request may have reached the server but got no usable response back (a
	// timeout, a dropped connection, a gateway 5xx) before the attempt that
	// produced this error. AutoPilot answers the repeat of a change it just
	// made with 200; on one that answers stale_object instead, a PATCH that
	// meets stale_object after such an attempt may be meeting its own change
	// (see patchRecord).
	EarlierSendUnanswered bool
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: ", e.Method, e.Path)
	if e.Status > 0 {
		fmt.Fprintf(&b, "HTTP %d", e.Status)
		if e.Code != "" {
			fmt.Fprintf(&b, " (%s)", e.Code)
		}
		if e.Field != "" {
			fmt.Fprintf(&b, " at %s", e.Field)
		}
		if e.Message != "" {
			fmt.Fprintf(&b, ": %s", e.Message)
		}
		return b.String()
	}
	b.WriteString(e.Message)
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether waiting and trying again could succeed: a 429 or
// a gateway-class 5xx (including AutoPilot's own 503 unavailable).
func (e *Error) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || isGatewayStatus(e.Status)
}

// IsNotFound reports AutoPilot's own answer for a missing record: a 404 whose
// refusal carries the not_found code. A retired sender reads that way too, so
// it means "gone" and never "try again". Any other 404 (an HTML page, a proxy,
// an endpoint with the wrong path) is not AutoPilot saying a record is gone,
// and is left to be reported.
func IsNotFound(err error) bool {
	e, ok := asError(err)
	return ok && e.Status == http.StatusNotFound && e.Code == CodeNotFound
}

// IsStale reports a lost optimistic-locking race: 409 stale_object.
func IsStale(err error) bool { return HasCode(err, CodeStaleObject) }

// IsUnauthorized reports a 401.
func IsUnauthorized(err error) bool { return hasStatus(err, http.StatusUnauthorized) }

// IsValidation reports a 422: the request is wrong, not the timing.
func IsValidation(err error) bool { return hasStatus(err, http.StatusUnprocessableEntity) }

func hasStatus(err error, status int) bool {
	e, ok := asError(err)
	return ok && e.Status == status
}

func asError(err error) (*Error, bool) {
	for err != nil {
		if e, ok := err.(*Error); ok {
			return e, true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return nil, false
		}
		err = u.Unwrap()
	}
	return nil, false
}

// AsError returns the *Error in err's chain, if any.
func AsError(err error) (*Error, bool) { return asError(err) }

// errorEnvelope is the API's refusal. `error` and `code` carry the same slug;
// `message` is prose; `field` is a JSON pointer to what is wrong.
type errorEnvelope struct {
	Error   string `json:"error"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field"`
}

func newError(method, path string, resp *http.Response, body []byte) *Error {
	e := &Error{Method: method, Path: path, Status: resp.StatusCode}
	var env errorEnvelope
	if json.Unmarshal(body, &env) == nil && (env.Code != "" || env.Error != "" || env.Message != "") {
		e.Code = env.Code
		if e.Code == "" {
			e.Code = env.Error
		}
		e.Message = env.Message
		e.Field = env.Field
		if e.Message == "" {
			e.Message = http.StatusText(resp.StatusCode)
		}
	} else if msg := strings.TrimSpace(string(body)); msg != "" {
		if len(msg) > maxErrorBody {
			cut := maxErrorBody
			for cut > 0 && !utf8.RuneStart(msg[cut]) {
				cut--
			}
			msg = msg[:cut] + "... (truncated)"
		}
		e.Message = msg
	} else {
		e.Message = http.StatusText(resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
			e.RetryAfter = time.Duration(secs) * time.Second
		} else if when, err := http.ParseTime(ra); err == nil {
			if d := time.Until(when); d > 0 {
				e.RetryAfter = d
			}
		}
	}
	return e
}
