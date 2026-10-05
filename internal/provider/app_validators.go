package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

// The API's rules for an app, checked at plan time like the sender's (see
// validators.go).
var (
	// An app's name: a lowercase letter or digit, then lowercase letters,
	// digits, _ and -.
	appNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	// A repository: owner/name.
	repoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`)
	// A developer's login: an email address in lowercase ASCII, which is all
	// AutoPilot takes. Printable ASCII but uppercase, @ and space, then @,
	// then a domain with a dot.
	loginPattern = regexp.MustCompile(`^[\x21-\x3f\x5b-\x7e]+@[\x21-\x3f\x5b-\x7e]+\.[\x21-\x3f\x5b-\x7e]+$`)
)

// setOrNull maps a list AutoPilot answered with to state. An optional
// attribute left out of configuration is null in the plan, while AutoPilot
// answers [] for it, so an empty answer stays null where the prior value
// (plan or state) was null. Anything else is the answer.
func setOrNull(values []string, prior types.Set) types.Set {
	if len(values) == 0 && prior.IsNull() {
		return types.SetNull(types.StringType)
	}
	return setFromStrings(values)
}

// noCaseRepeatsValidator refuses a set holding two values that differ only in
// case. AutoPilot matches repositories without regard to case and keeps them
// unique that way, so it would keep only one of them and the configuration
// would differ from it on every plan.
type noCaseRepeatsValidator struct{ what string }

func (v noCaseRepeatsValidator) Description(context.Context) string {
	return fmt.Sprintf("no two %s may differ only in case", v.what)
}

func (v noCaseRepeatsValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v noCaseRepeatsValidator) ValidateSet(ctx context.Context, req validator.SetRequest, resp *validator.SetResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	seen := map[string]string{}
	for _, elem := range req.ConfigValue.Elements() {
		s, ok := elem.(types.String)
		if !ok || s.IsNull() || s.IsUnknown() {
			continue
		}
		folded := strings.ToLower(s.ValueString())
		if first, dup := seen[folded]; dup {
			resp.Diagnostics.AddAttributeError(req.Path, "Repeated "+v.what,
				fmt.Sprintf("%q and %q are the same to AutoPilot, which matches %s without regard to case. Keep one of them.",
					first, s.ValueString(), v.what))
			return
		}
		seen[folded] = s.ValueString()
	}
}

// acceptsValidator checks the rules that span an accepts entry or the whole
// set: each sender at most once, and can_decide only with review-pr.
type acceptsValidator struct{}

func (acceptsValidator) Description(context.Context) string {
	return "each sender at most once; can_decide only with review-pr in kinds"
}

func (v acceptsValidator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (acceptsValidator) ValidateSet(ctx context.Context, req validator.SetRequest, resp *validator.SetResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	seen := map[string]bool{}
	for _, elem := range req.ConfigValue.Elements() {
		obj, ok := elem.(types.Object)
		if !ok || obj.IsNull() || obj.IsUnknown() {
			continue
		}
		var m acceptModel
		if diags := obj.As(ctx, &m, basetypes.ObjectAsOptions{}); diags.HasError() {
			continue
		}
		if known(m.Sender) {
			sender := m.Sender.ValueString()
			if seen[sender] {
				resp.Diagnostics.AddAttributeError(req.Path, "Sender accepted twice",
					fmt.Sprintf("The sender %q appears in more than one `accepts` entry. An app accepts each sender at "+
						"most once; put all the kinds it accepts from %q in one entry.", sender, sender))
			}
			seen[sender] = true
		}
		if m.CanDecide.IsNull() || m.CanDecide.IsUnknown() || !m.CanDecide.ValueBool() || m.Kinds.IsUnknown() {
			continue
		}
		// A kind not known yet may turn out to be review-pr, so the check waits
		// until every kind is known: Terraform validates again at apply, with
		// the values the plan left unknown.
		hasReviewPR, unknownKind := false, false
		for _, k := range m.Kinds.Elements() {
			s, ok := k.(types.String)
			switch {
			case !ok || s.IsUnknown():
				unknownKind = true
			case s.ValueString() == "review-pr":
				hasReviewPR = true
			}
		}
		if !hasReviewPR && !unknownKind {
			resp.Diagnostics.AddAttributeError(req.Path, "can_decide without review-pr",
				fmt.Sprintf("The entry for %q sets can_decide, which only applies to review-pr tasks, but its kinds "+
					"don't include review-pr. Add review-pr to its kinds, or leave can_decide out.", m.Sender.ValueString()))
		}
	}
}
