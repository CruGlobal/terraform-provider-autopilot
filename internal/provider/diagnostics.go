package provider

import (
	"errors"
	"fmt"
	"strings"

	"github.com/CruGlobal/terraform-provider-autopilot/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
)

// addAPIError turns a client error into a diagnostic. The summary names the
// operation; the detail carries the HTTP status, the code, the field and
// AutoPilot's message, so a user can tell a refused token from a refused
// value without guessing. When the refusal names a field that is one of the
// resource's attributes (fields maps API field names to attribute names),
// the diagnostic points at that attribute.
func addAPIError(diags *diag.Diagnostics, summary string, err error, fields map[string]string) {
	var apiErr *client.Error
	if !errors.As(err, &apiErr) {
		diags.AddError(summary, err.Error())
		return
	}
	detail := apiErr.Error()
	switch {
	case client.IsUnauthorized(err):
		detail += "\n\nAutoPilot refused the admin token. Check that the token (or AUTOPILOT_TOKEN) is the current " +
			"admin token of the AutoPilot at the endpoint. An AutoPilot with no admin token set up answers the same way."
	case apiErr.Status == 404:
		detail += "\n\nAutoPilot answers a missing record with a not_found refusal, and this 404 is not one. Check " +
			"that the endpoint is the AutoPilot itself, with no extra path, and that nothing in between (a proxy or " +
			"a sleeping environment's front page) answered instead."
	case apiErr.Code == client.CodeInvalidAttribute:
		detail += "\n\nAutoPilot refused the value; fix the configuration rather than retrying. A field AutoPilot " +
			"doesn't know at all means it is older than this provider."
	case apiErr.Code == client.CodePreconditionRequired:
		detail += "\n\nThe provider always sends If-Match with a change, so this is a bug in the provider or " +
			"something between it and AutoPilot dropped the header."
	case apiErr.Retryable():
		detail += "\n\nAutoPilot was still unavailable after the provider's retries. Apply again later."
	}
	if attr := attributeFor(apiErr.Field, fields); attr != "" {
		diags.AddAttributeError(path.Root(attr), summary, detail)
		return
	}
	diags.AddError(summary, detail)
}

// attributeFor maps a refusal's JSON pointer ("/branch_prefix",
// "/accepts/0/kinds") to the top-level attribute it is about, or "".
func attributeFor(pointer string, fields map[string]string) string {
	if !strings.HasPrefix(pointer, "/") {
		return ""
	}
	first, _, _ := strings.Cut(strings.TrimPrefix(pointer, "/"), "/")
	first = strings.ReplaceAll(strings.ReplaceAll(first, "~1", "/"), "~0", "~")
	return fields[first]
}

// addStaleError reports a lost optimistic-locking race. The provider never
// overwrites the other writer's change; the user plans again to see it.
func addStaleError(diags *diag.Diagnostics, what string, stateVersion int64, current *int64, err error) {
	detail := fmt.Sprintf("%s was changed outside of Terraform since the last refresh (state has lock_version %d", what, stateVersion)
	if current != nil {
		detail += fmt.Sprintf(", AutoPilot now has %d", *current)
	}
	detail += "). Nothing was overwritten. Run `terraform plan` again to see the current values, then apply."
	if apiErr, ok := client.AsError(err); ok && apiErr.Message != "" {
		detail += "\n\nAutoPilot said: " + apiErr.Message
	}
	diags.AddError(what+" was changed outside of Terraform", detail)
}

// apiMessage returns AutoPilot's message from an API error, or the error text.
func apiMessage(err error) string {
	if apiErr, ok := client.AsError(err); ok && apiErr.Message != "" {
		return apiErr.Message
	}
	return err.Error()
}
