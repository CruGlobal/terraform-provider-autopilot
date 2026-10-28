package provider

import (
	"context"
	"fmt"
	"regexp"
	"slices"
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
	// The characters of a developer's login: lowercase ASCII letters, digits
	// and ._%+@- (see validEmail).
	emailCharacters = regexp.MustCompile(`^[a-z0-9._%+@-]+$`)
	// A GitHub login, as AutoPilot takes one: in lowercase ASCII, a letter or
	// digit, then letters, digits, - and _ (an enterprise's managed users
	// have _), at most 39 characters.
	gitHubLoginPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,38}$`)
	// A workflow that may ask for an app's automatic reviews, as GitHub's
	// OIDC token names the workflow a job ran (job_workflow_ref), in
	// AutoPilot's own pattern: owner/repo, a file straight in
	// .github/workflows, and a branch. parseWorkflowRef checks the rest.
	workflowRefPattern = regexp.MustCompile(`^([A-Za-z0-9-]{1,39}/[A-Za-z0-9._-]{1,100})/(\.github/workflows/[A-Za-z0-9._-]{1,200}\.ya?ml)@refs/heads/([A-Za-z0-9._/-]{1,200})$`)
)

// workflowRef is a workflow an app's automatic reviews may take asks from.
type workflowRef struct {
	// repo is owner/repo, the repository the workflow's file is in.
	repo string
	// path is .github/workflows/<file>.yml (or .yaml).
	path string
	// branch is the branch, without refs/heads/.
	branch string
}

// key is what AutoPilot tells two workflows apart by: the repository without
// regard to case, as GitHub names repositories, and the file and branch
// exactly, as git does.
func (w workflowRef) key() string {
	return strings.ToLower(w.repo) + "/" + w.path + "@refs/heads/" + w.branch
}

// parseWorkflowRef is AutoPilot's own rule for a workflow an app lists in
// auto_review.workflows, step for step: workflowRefPattern, a repository
// name that isn't dots alone, and a branch named as git allows. It returns
// the workflow, or what is wrong with it.
func parseWorkflowRef(text string) (workflowRef, string) {
	m := workflowRefPattern.FindStringSubmatch(text)
	if m == nil {
		switch {
		case strings.Contains(text, "@refs/pull/"):
			return workflowRef{}, "is a pull request's ref, and only a branch's workflow may ask (refs/heads/<branch>)"
		case strings.Contains(text, "@refs/tags/"):
			return workflowRef{}, "is a tag's ref, and only a branch's workflow may ask (refs/heads/<branch>)"
		}
		return workflowRef{}, "isn't owner/repo/.github/workflows/<file>.yml@refs/heads/<branch>, in the characters " +
			"and lengths GitHub allows"
	}
	w := workflowRef{repo: m[1], path: m[2], branch: m[3]}
	b := w.branch
	switch {
	case strings.Trim(w.repo[strings.IndexByte(w.repo, '/')+1:], ".") == "":
		return workflowRef{}, "names a repository of dots alone"
	case strings.HasPrefix(b, ".") || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/"):
		return workflowRef{}, "names a branch that starts with ., - or /, which git doesn't allow"
	case strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".") || strings.HasSuffix(b, ".lock"):
		return workflowRef{}, "names a branch that ends with /, . or .lock, which git doesn't allow"
	case strings.Contains(b, "..") || strings.Contains(b, "//") || strings.Contains(b, "/."):
		return workflowRef{}, "names a branch with .., // or /. in it, which git doesn't allow"
	}
	return w, ""
}

// workflowRefValidator refuses a workflow AutoPilot would refuse in
// auto_review.workflows.
type workflowRefValidator struct{}

func (workflowRefValidator) Description(context.Context) string {
	return "a GitHub Actions workflow on a branch: owner/repo/.github/workflows/<file>.yml@refs/heads/<branch>"
}

func (v workflowRefValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (workflowRefValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	value := req.ConfigValue.ValueString()
	if _, problem := parseWorkflowRef(value); problem != "" {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid workflow",
			fmt.Sprintf("%q %s. AutoPilot takes a workflow as GitHub's OIDC token names the workflow a job ran "+
				"(job_workflow_ref), on a branch, such as "+
				"example-org/billing-api/.github/workflows/autopilot-review.yml@refs/heads/main.", value, problem))
	}
}

// builtInSenders are the senders built into AutoPilot, whose reviews run for
// a person: autopilot-mcp, for the one who asked on AutoPilot's MCP server,
// and autopilot-github, for the developer who started a pull request review
// from GitHub. Only their entries may set can_decide_own_pr, since only for
// them can AutoPilot tell whether a pull request may be the asker's own.
var builtInSenders = []string{"autopilot-mcp", "autopilot-github"}

// validEmail is AutoPilot's own rule for a developer's login, an email
// address in lowercase ASCII, step for step: at most 254 characters; a local
// part of 1 to 64 before the first @; a domain after it with a dot, that
// doesn't start or end with one, has no two in a row and no other @; and
// only the characters emailCharacters allows.
func validEmail(text string) bool {
	at := strings.IndexByte(text, '@')
	if at < 0 {
		return false
	}
	local, domain := text[:at], text[at+1:]
	return len(text) <= 254 && len(local) >= 1 && len(local) <= 64 &&
		strings.Contains(domain, ".") && !strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".") &&
		!strings.Contains(domain, "..") && !strings.Contains(domain, "@") &&
		emailCharacters.MatchString(text)
}

// emailValidator refuses a developer's login AutoPilot would refuse.
type emailValidator struct{}

func (emailValidator) Description(context.Context) string {
	return "an email address in lowercase ASCII"
}

func (v emailValidator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (emailValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if value := req.ConfigValue.ValueString(); !validEmail(value) {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid developer",
			fmt.Sprintf("%q must be an email address in lowercase ASCII, which is all AutoPilot takes: letters, "+
				"digits and ._%%+- before the @, and a domain with a dot after it.", value))
	}
}

// The worker images an app's tasks may run in.
var workerImages = []string{"node", "ruby", "ruby-browser", "python", "go"}

// workerSizes is the memory, in MiB, that AWS Fargate allows with each CPU
// size, in CPU units, an app's workers may have: at most 4 vCPU and 16 GB.
var workerSizes = map[int64][]int64{
	256:  {512, 1024, 2048},
	512:  gigabytes(1024, 4096),
	1024: gigabytes(2048, 8192),
	2048: gigabytes(4096, 16384),
	4096: gigabytes(8192, 16384),
}

// gigabytes is from to to MiB, in steps of 1 GB.
func gigabytes(from, to int64) []int64 {
	var out []int64
	for m := from; m <= to; m += 1024 {
		out = append(out, m)
	}
	return out
}

// memoryRule says which memory goes with which CPU, for docs and errors.
const memoryRule = "`512`, `1024` or `2048` with `256`; `1024` to `4096` with `512`; `2048` to `8192` with " +
	"`1024`; `4096` to `16384` with `2048`; `8192` to `16384` with `4096`; each range in steps of `1024`"

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

// gitHubLoginsValidator refuses a GitHub login listed with two developers.
// A set already keeps an entry listed twice exactly once.
type gitHubLoginsValidator struct{}

func (gitHubLoginsValidator) Description(context.Context) string {
	return "each GitHub login at most once"
}

func (v gitHubLoginsValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (gitHubLoginsValidator) ValidateSet(ctx context.Context, req validator.SetRequest, resp *validator.SetResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	seen := map[string]bool{}
	for _, elem := range req.ConfigValue.Elements() {
		obj, ok := elem.(types.Object)
		if !ok || obj.IsNull() || obj.IsUnknown() {
			continue
		}
		var m gitHubLoginModel
		if diags := obj.As(ctx, &m, basetypes.ObjectAsOptions{}); diags.HasError() || !known(m.Login) {
			continue
		}
		login := m.Login.ValueString()
		if seen[login] {
			resp.Diagnostics.AddAttributeError(req.Path, "GitHub login listed twice",
				fmt.Sprintf("The GitHub login %q is paired with more than one developer. Each login belongs to one "+
					"developer; keep one of its entries.", login))
		}
		seen[login] = true
	}
}

// acceptsValidator checks the rules that span an accepts entry or the whole
// set: each sender at most once, can_decide only with review-pr,
// can_decide_own_pr only with can_decide and only for a sender built into
// AutoPilot, approve_up_to only with can_decide, and can_auto_merge only
// with a kind that opens a pull request.
type acceptsValidator struct{}

// pullRequestKinds are the kinds whose tasks open a pull request, the only
// ones auto-merge applies to.
var pullRequestKinds = []string{"implement-work-item", "fix-error"}

func (acceptsValidator) Description(context.Context) string {
	return "each sender at most once; can_decide only with review-pr in kinds; can_decide_own_pr only with " +
		"can_decide, and only for autopilot-mcp and autopilot-github; approve_up_to only with can_decide; " +
		"can_auto_merge only with implement-work-item or fix-error in kinds"
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
		if isTrue(m.CanDecide) && lacksKind(m.Kinds, "review-pr") {
			resp.Diagnostics.AddAttributeError(req.Path, "can_decide without review-pr",
				fmt.Sprintf("The entry for %q sets can_decide, which only applies to review-pr tasks, but its kinds "+
					"don't include review-pr. Add review-pr to its kinds, or leave can_decide out.", m.Sender.ValueString()))
		}
		// can_decide left out is false (its default); one not known yet waits.
		if isTrue(m.CanDecideOwnPR) && !m.CanDecide.IsUnknown() && !m.CanDecide.ValueBool() {
			resp.Diagnostics.AddAttributeError(req.Path, "can_decide_own_pr without can_decide",
				fmt.Sprintf("The entry for %q sets can_decide_own_pr, which only widens can_decide, but can_decide "+
					"isn't true. Set can_decide = true too, or leave can_decide_own_pr out.", m.Sender.ValueString()))
		}
		if isTrue(m.CanDecideOwnPR) && known(m.Sender) && !slices.Contains(builtInSenders, m.Sender.ValueString()) {
			resp.Diagnostics.AddAttributeError(req.Path, "can_decide_own_pr for a sender not built in",
				fmt.Sprintf("The entry for %q sets can_decide_own_pr, which AutoPilot takes only for autopilot-mcp "+
					"and autopilot-github, whose reviews run for a person. Leave can_decide_own_pr out of that entry.",
					m.Sender.ValueString()))
		}
		// The ceiling narrows can_decide, so it needs it, as AutoPilot says.
		if known(m.ApproveUpTo) && !m.CanDecide.IsUnknown() && !m.CanDecide.ValueBool() {
			resp.Diagnostics.AddAttributeError(req.Path, "approve_up_to without can_decide",
				fmt.Sprintf("The entry for %q sets approve_up_to, the most risk its reviews may approve, but "+
					"can_decide isn't true, so its reviews approve nothing. Set can_decide = true too, or leave "+
					"approve_up_to out.", m.Sender.ValueString()))
		}
		if isTrue(m.CanAutoMerge) && lacksKind(m.Kinds, pullRequestKinds...) {
			resp.Diagnostics.AddAttributeError(req.Path, "can_auto_merge without a pull request kind",
				fmt.Sprintf("The entry for %q sets can_auto_merge, which only applies to the pull requests that "+
					"implement-work-item and fix-error tasks open, but its kinds include neither. Add one of them to its "+
					"kinds, or leave can_auto_merge out.", m.Sender.ValueString()))
		}
	}
}

// isTrue reports whether a flag is known and true.
func isTrue(b types.Bool) bool { return !b.IsNull() && !b.IsUnknown() && b.ValueBool() }

// lacksKind reports whether a known set of kinds holds none of wanted. A kind
// not known yet may turn out to be one of them, so the answer is false until
// every kind is known: Terraform validates again at apply, with the values
// the plan left unknown.
func lacksKind(kinds types.Set, wanted ...string) bool {
	if kinds.IsNull() || kinds.IsUnknown() {
		return false
	}
	for _, k := range kinds.Elements() {
		s, ok := k.(types.String)
		if !ok || s.IsUnknown() || slices.Contains(wanted, s.ValueString()) {
			return false
		}
	}
	return true
}
