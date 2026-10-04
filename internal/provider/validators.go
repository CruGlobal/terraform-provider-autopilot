package provider

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// The API's rules, checked at plan time so a mistake fails `terraform plan`
// instead of the apply. Each pattern is no stricter than the rule it mirrors,
// so it never refuses what AutoPilot would take.
var (
	// A sender's name: a lowercase letter, then lowercase letters, digits and
	// -, at most 40 characters.
	senderNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	// A branch prefix: a lowercase word and a /.
	branchPrefixPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*/$`)
	// A kind of task, such as implement-work-item. Which kinds exist is
	// AutoPilot's to say, so only the shape is checked here.
	kindPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	// One label of a DNS name, in lowercase.
	hostLabelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// minCharactersValidator is stringvalidator.LengthAtLeast counted in
// characters, as AutoPilot counts them, rather than bytes. It never repeats
// the value, which is a secret.
type minCharactersValidator struct{ min int }

func (v minCharactersValidator) Description(context.Context) string {
	return fmt.Sprintf("at least %d characters", v.min)
}

func (v minCharactersValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v minCharactersValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if n := utf8.RuneCountInString(req.ConfigValue.ValueString()); n < v.min {
		resp.Diagnostics.AddAttributeError(req.Path, "Too short",
			fmt.Sprintf("must be at least %d characters, got %d", v.min, n))
	}
}

// callbackHostReason is AutoPilot's own rule for a callback host.
const callbackHostReason = "AutoPilot takes a callback host as a lowercase DNS name with at least one dot, " +
	"not an IP address and not localhost (callbacks go to it over https on port 443)"

// callbackHostValidator refuses a callback host AutoPilot would refuse, and
// says so in AutoPilot's words. An uppercase host is refused rather than
// folded, because AutoPilot keeps hosts as sent and a folded value would
// differ from the configuration on every plan.
type callbackHostValidator struct{}

func (callbackHostValidator) Description(context.Context) string {
	return "a lowercase DNS name with at least one dot, not an IP address and not localhost"
}

func (v callbackHostValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (callbackHostValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	host := req.ConfigValue.ValueString()
	if problem := callbackHostProblem(host); problem != "" {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid callback host",
			fmt.Sprintf("%q %s. %s.", host, problem, callbackHostReason))
	}
}

// callbackHostProblem says what is wrong with a callback host, or "".
func callbackHostProblem(host string) string {
	switch {
	case host != strings.ToLower(host):
		return fmt.Sprintf("has uppercase letters (write it as %q)", strings.ToLower(host))
	case host == "localhost":
		return "is localhost"
	case net.ParseIP(host) != nil:
		return "is an IP address"
	case strings.Contains(host, "://") || strings.ContainsAny(host, "/:"):
		return "is not a bare host name (leave out the scheme, port and path)"
	case !strings.Contains(host, "."):
		return "has no dot"
	case len(host) > 253:
		return "is longer than a DNS name can be"
	}
	for _, label := range strings.Split(host, ".") {
		if !hostLabelPattern.MatchString(label) {
			return "is not a DNS name"
		}
	}
	return ""
}
