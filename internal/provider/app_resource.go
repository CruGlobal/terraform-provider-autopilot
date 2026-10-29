package provider

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/CruGlobal/terraform-provider-autopilot/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

var (
	_ resource.Resource                   = &appResource{}
	_ resource.ResourceWithConfigure      = &appResource{}
	_ resource.ResourceWithImportState    = &appResource{}
	_ resource.ResourceWithValidateConfig = &appResource{}
)

// NewAppResource returns the autopilot_app resource.
func NewAppResource() resource.Resource { return &appResource{} }

type appResource struct {
	client *client.Client
}

type appModel struct {
	Name            types.String `tfsdk:"name"`
	Repos           types.Set    `tfsdk:"repos"`
	Accepts         types.Set    `tfsdk:"accepts"`
	Developers      types.Set    `tfsdk:"developers"`
	GitHubLogins    types.Set    `tfsdk:"github_logins"`
	Credential      types.String `tfsdk:"credential"`
	WhenSeatsFull   types.String `tfsdk:"when_seats_full"`
	WorkerImage     types.String `tfsdk:"worker_image"`
	WorkerCPU       types.Int64  `tfsdk:"worker_cpu"`
	WorkerMemory    types.Int64  `tfsdk:"worker_memory"`
	AutoReview      types.Object `tfsdk:"auto_review"`
	ReviewAlsoReads types.Set    `tfsdk:"review_also_reads"`
	LockVersion     types.Int64  `tfsdk:"lock_version"`
}

type autoReviewModel struct {
	Events         types.Set    `tfsdk:"events"`
	Workflows      types.Set    `tfsdk:"workflows"`
	MaxPerDay      types.Int64  `tfsdk:"max_per_day"`
	CanDecide      types.Bool   `tfsdk:"can_decide"`
	CanDecideOwnPR types.Bool   `tfsdk:"can_decide_own_pr"`
	ApproveUpTo    types.String `tfsdk:"approve_up_to"`
}

var autoReviewAttrTypes = map[string]attr.Type{
	"events":            types.SetType{ElemType: types.StringType},
	"workflows":         types.SetType{ElemType: types.StringType},
	"max_per_day":       types.Int64Type,
	"can_decide":        types.BoolType,
	"can_decide_own_pr": types.BoolType,
	"approve_up_to":     types.StringType,
}

// The events and the cap an app's automatic reviews have unless it says,
// the most the cap may be, and the most workflows it may list, as AutoPilot
// has them.
var (
	defaultAutoEvents       = []string{"opened", "ready_for_review"}
	defaultAutoPerDay int64 = 50
	maxAutoPerDay     int64 = 200
	maxAutoWorkflows        = 20
)

type acceptModel struct {
	Sender         types.String `tfsdk:"sender"`
	Kinds          types.Set    `tfsdk:"kinds"`
	CanDecide      types.Bool   `tfsdk:"can_decide"`
	CanAutoMerge   types.Bool   `tfsdk:"can_auto_merge"`
	CanDecideOwnPR types.Bool   `tfsdk:"can_decide_own_pr"`
	ApproveUpTo    types.String `tfsdk:"approve_up_to"`
}

var acceptAttrTypes = map[string]attr.Type{
	"sender":            types.StringType,
	"kinds":             types.SetType{ElemType: types.StringType},
	"can_decide":        types.BoolType,
	"can_auto_merge":    types.BoolType,
	"can_decide_own_pr": types.BoolType,
	"approve_up_to":     types.StringType,
}

var acceptObjectType = types.ObjectType{AttrTypes: acceptAttrTypes}

type gitHubLoginModel struct {
	Login     types.String `tfsdk:"login"`
	Developer types.String `tfsdk:"developer"`
}

var gitHubLoginAttrTypes = map[string]attr.Type{
	"login":     types.StringType,
	"developer": types.StringType,
}

var gitHubLoginObjectType = types.ObjectType{AttrTypes: gitHubLoginAttrTypes}

// appAPIFields maps the API's field names to this resource's attributes.
var appAPIFields = map[string]string{
	"name":              "name",
	"repos":             "repos",
	"accepts":           "accepts",
	"developers":        "developers",
	"github_logins":     "github_logins",
	"credential":        "credential",
	"when_seats_full":   "when_seats_full",
	"worker_image":      "worker_image",
	"worker_cpu":        "worker_cpu",
	"worker_memory":     "worker_memory",
	"auto_review":       "auto_review",
	"review_also_reads": "review_also_reads",
}

// appToModel maps an app. Every list is optional and left out of the
// configuration is null, while AutoPilot answers [] for it, so an empty
// answer stays null where prior (the plan, or the state) was null. The
// credential policy, the worker image and the worker size are null when
// AutoPilot answers null, as it does for an app without them.
func appToModel(a *client.App, prior appModel) appModel {
	return appModel{
		Name:            types.StringValue(a.Name),
		Repos:           setOrNull(a.Repos, prior.Repos),
		Accepts:         acceptsToSet(a.Accepts, prior.Accepts),
		Developers:      setOrNull(a.Developers, prior.Developers),
		GitHubLogins:    gitHubLoginsToSet(a.GitHubLogins, prior.GitHubLogins),
		Credential:      types.StringPointerValue(a.Credential),
		WhenSeatsFull:   types.StringPointerValue(a.WhenSeatsFull),
		WorkerImage:     types.StringPointerValue(a.WorkerImage),
		WorkerCPU:       types.Int64PointerValue(a.WorkerCPU),
		WorkerMemory:    types.Int64PointerValue(a.WorkerMemory),
		AutoReview:      autoReviewToObject(a.AutoReview),
		ReviewAlsoReads: alsoReadsToSet(a.ReviewAlsoReads, prior.ReviewAlsoReads),
		LockVersion:     types.Int64Value(a.LockVersion),
	}
}

// alsoReadsToSet maps the repositories AutoPilot answered that the app's
// reviews also read to state, as setOrNull does, except that an answer
// without the field is none: null, or [] where prior was []. Only an
// AutoPilot that predates the field, or was rolled back past it, answers
// so, and it ignores a list it kept, so no review reads one. Read as none, a
// configuration that takes the list out plans nothing, where [] would send
// the field, which that AutoPilot refuses.
func alsoReadsToSet(values []string, prior types.Set) types.Set {
	if values == nil && !emptySet(prior) {
		return types.SetNull(types.StringType)
	}
	return setOrNull(values, prior)
}

// emptySet reports whether a set is known and holds no value.
func emptySet(s types.Set) bool { return !s.IsNull() && !s.IsUnknown() && len(s.Elements()) == 0 }

// noneSet reports whether a set is null or known and empty: none, to
// AutoPilot, for a list whose default is none.
func noneSet(s types.Set) bool { return s.IsNull() || emptySet(s) }

// autoReviewToObject maps the automatic reviews AutoPilot answered with to
// state: null for none (or an AutoPilot that predates the field), and every
// field otherwise, the ceiling null for none and the workflows empty for
// none (or an AutoPilot that predates them). The plan fills each field it
// leaves out with AutoPilot's default, so the two agree.
func autoReviewToObject(a *client.AutoReview) types.Object {
	if a == nil {
		return types.ObjectNull(autoReviewAttrTypes)
	}
	return types.ObjectValueMust(autoReviewAttrTypes, map[string]attr.Value{
		"events":            setFromStrings(a.Events),
		"workflows":         setFromStrings(a.Workflows),
		"max_per_day":       types.Int64Value(a.MaxPerDay),
		"can_decide":        types.BoolValue(a.CanDecide),
		"can_decide_own_pr": types.BoolValue(a.CanDecideOwnPR),
		"approve_up_to":     optionalString(a.ApproveUpTo),
	})
}

// autoReviewFromModel is the automatic reviews the plan holds, as the
// client sends them: a nil On for none, which takes them away. A value still
// unknown at apply is an error, never read as none or as a default: none
// would turn them off, and a default could widen what they may do.
func autoReviewFromModel(ctx context.Context, m appModel, diags *diag.Diagnostics) *client.AutoReviewSetting {
	if m.AutoReview.IsUnknown() {
		unknownAtApply(diags, "auto_review")
		return nil
	}
	if m.AutoReview.IsNull() {
		return &client.AutoReviewSetting{}
	}
	var auto autoReviewModel
	diags.Append(m.AutoReview.As(ctx, &auto, basetypes.ObjectAsOptions{})...)
	if diags.HasError() {
		return nil
	}
	if auto.Events.IsUnknown() || auto.Workflows.IsUnknown() || auto.MaxPerDay.IsUnknown() ||
		auto.CanDecide.IsUnknown() || auto.CanDecideOwnPR.IsUnknown() || auto.ApproveUpTo.IsUnknown() {
		unknownAtApply(diags, "auto_review")
		return nil
	}
	return &client.AutoReviewSetting{On: &client.AutoReview{
		Events:         stringsFromSet(ctx, auto.Events, "auto_review", diags),
		Workflows:      stringsFromSet(ctx, auto.Workflows, "auto_review", diags),
		MaxPerDay:      auto.MaxPerDay.ValueInt64(),
		CanDecide:      auto.CanDecide.ValueBool(),
		CanDecideOwnPR: auto.CanDecideOwnPR.ValueBool(),
		ApproveUpTo:    auto.ApproveUpTo.ValueString(),
	}}
}

// imageFromModel is the worker image the plan holds, as the client sends
// it: "" for none, which takes the app back to node. A value still unknown
// at apply is an error, never read as none.
func imageFromModel(m appModel, diags *diag.Diagnostics) *string {
	if m.WorkerImage.IsUnknown() {
		unknownAtApply(diags, "worker_image")
		return nil
	}
	image := m.WorkerImage.ValueString()
	return &image
}

// sizeFromModel is the worker size the plan holds, as the client sends it:
// zero for none, which takes the app's size away. A value still unknown at
// apply is an error, never read as none.
func sizeFromModel(m appModel, diags *diag.Diagnostics) *client.WorkerSize {
	if m.WorkerCPU.IsUnknown() {
		unknownAtApply(diags, "worker_cpu")
		return nil
	}
	if m.WorkerMemory.IsUnknown() {
		unknownAtApply(diags, "worker_memory")
		return nil
	}
	return &client.WorkerSize{CPU: m.WorkerCPU.ValueInt64(), Memory: m.WorkerMemory.ValueInt64()}
}

// policyFromModel is the credential policy the plan holds, as the client
// sends it: credential "" for none, which takes the app's own policy away.
// A value still unknown at apply is an error, never read as none.
func policyFromModel(m appModel, diags *diag.Diagnostics) *client.CredentialPolicy {
	if m.Credential.IsUnknown() {
		unknownAtApply(diags, "credential")
		return nil
	}
	if m.WhenSeatsFull.IsUnknown() {
		unknownAtApply(diags, "when_seats_full")
		return nil
	}
	return &client.CredentialPolicy{Credential: m.Credential.ValueString(), WhenSeatsFull: m.WhenSeatsFull.ValueString()}
}

func acceptsToSet(accepts []client.Accept, prior types.Set) types.Set {
	if len(accepts) == 0 && prior.IsNull() {
		return types.SetNull(acceptObjectType)
	}
	autoMergeFalse := flagSetFalse(prior, "can_auto_merge")
	ownPRFalse := flagSetFalse(prior, "can_decide_own_pr")
	elems := make([]attr.Value, 0, len(accepts))
	for _, a := range accepts {
		elems = append(elems, types.ObjectValueMust(acceptAttrTypes, map[string]attr.Value{
			"sender":            types.StringValue(a.Sender),
			"kinds":             setFromStrings(a.Kinds),
			"can_decide":        types.BoolValue(a.CanDecide),
			"can_auto_merge":    optionalFlag(a.CanAutoMerge, autoMergeFalse[a.Sender]),
			"can_decide_own_pr": optionalFlag(a.CanDecideOwnPR, ownPRFalse[a.Sender]),
			"approve_up_to":     optionalString(a.ApproveUpTo),
		}))
	}
	return types.SetValueMust(acceptObjectType, elems)
}

// optionalFlag maps an optional flag with no default: off stays null unless
// prior set it to false.
func optionalFlag(on, priorSaysFalse bool) types.Bool {
	if !on && !priorSaysFalse {
		return types.BoolNull()
	}
	return types.BoolValue(on)
}

// optionalString maps an optional string with no default: "" (AutoPilot's
// null) is null.
func optionalString(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}

// flagSetFalse names the senders whose entry in prior (the plan, or the
// state) sets the flag name to false. can_auto_merge and can_decide_own_pr
// are optional and have no default, so off is null unless the configuration
// says false. A second default in a set's element would be wrong: the
// framework finds an element's configuration by the element's whole value,
// which a computed value taken from the state changes, and then it would
// apply can_decide's default over a can_decide the configuration sets.
func flagSetFalse(prior types.Set, name string) map[string]bool {
	out := map[string]bool{}
	if prior.IsNull() || prior.IsUnknown() {
		return out
	}
	for _, elem := range prior.Elements() {
		obj, ok := elem.(types.Object)
		if !ok || obj.IsNull() || obj.IsUnknown() {
			continue
		}
		sender, _ := obj.Attributes()["sender"].(types.String)
		flag, _ := obj.Attributes()[name].(types.Bool)
		if known(sender) && !flag.IsNull() && !flag.IsUnknown() && !flag.ValueBool() {
			out[sender.ValueString()] = true
		}
	}
	return out
}

// gitHubLoginsToSet maps the GitHub logins AutoPilot answered with to
// state. Left out of the configuration is null, while AutoPilot answers []
// (or nothing, when it predates the field), so an empty answer stays null
// where prior was null.
func gitHubLoginsToSet(logins []client.GitHubLogin, prior types.Set) types.Set {
	if len(logins) == 0 && prior.IsNull() {
		return types.SetNull(gitHubLoginObjectType)
	}
	elems := make([]attr.Value, 0, len(logins))
	for _, l := range logins {
		elems = append(elems, types.ObjectValueMust(gitHubLoginAttrTypes, map[string]attr.Value{
			"login":     types.StringValue(l.Login),
			"developer": types.StringValue(l.Developer),
		}))
	}
	return types.SetValueMust(gitHubLoginObjectType, elems)
}

// gitHubLoginsFromSet returns a known set's entries. A null set gives nil
// (not sent); a known empty set gives an empty, non-nil slice, which the
// client sends as []. An unknown set is an error, never read as empty.
func gitHubLoginsFromSet(ctx context.Context, set types.Set, diags *diag.Diagnostics) []client.GitHubLogin {
	if set.IsUnknown() {
		unknownAtApply(diags, "github_logins")
		return nil
	}
	if set.IsNull() {
		return nil
	}
	var models []gitHubLoginModel
	diags.Append(set.ElementsAs(ctx, &models, false)...)
	out := make([]client.GitHubLogin, 0, len(models))
	for _, m := range models {
		if !known(m.Login) || !known(m.Developer) {
			unknownAtApply(diags, "github_logins")
			return nil
		}
		out = append(out, client.GitHubLogin{Login: m.Login.ValueString(), Developer: m.Developer.ValueString()})
	}
	return out
}

// acceptsFromSet returns a known set's entries. A null set gives nil (not
// sent); a known empty set gives an empty, non-nil slice, which the client
// sends as []. An unknown set is an error, never read as empty: an empty
// accepts sent by mistake would withdraw the app's consent.
func acceptsFromSet(ctx context.Context, set types.Set, diags *diag.Diagnostics) []client.Accept {
	if set.IsUnknown() {
		unknownAtApply(diags, "accepts")
		return nil
	}
	if set.IsNull() {
		return nil
	}
	var models []acceptModel
	diags.Append(set.ElementsAs(ctx, &models, false)...)
	out := make([]client.Accept, 0, len(models))
	for _, m := range models {
		// A ceiling still unknown at apply is an error, never read as none:
		// none is the widest setting there is.
		if m.ApproveUpTo.IsUnknown() {
			unknownAtApply(diags, "accepts")
			return nil
		}
		out = append(out, client.Accept{
			Sender:         m.Sender.ValueString(),
			Kinds:          stringsFromSet(ctx, m.Kinds, "accepts", diags),
			CanDecide:      m.CanDecide.ValueBool(),
			CanAutoMerge:   m.CanAutoMerge.ValueBool(),
			CanDecideOwnPR: m.CanDecideOwnPR.ValueBool(),
			ApproveUpTo:    m.ApproveUpTo.ValueString(),
		})
	}
	return out
}

func (r *appResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_app"
}

func (r *appResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *appResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An app: a unit of consent. It owns repositories, names the senders it accepts work " +
			"from with the kinds of task it accepts from each, and names the people who may start work for it " +
			"themselves, with their GitHub logins.\n\n" +
			"AutoPilot takes a sender's task only when the app the task is billed to, and the app that owns each " +
			"repository the task touches, accept that sender for that kind. An app may accept a sender that " +
			"doesn't exist yet, so the app and the sender can be applied in either order.\n\n" +
			"Every argument but `name` is optional and sent only when set. Removing one from the configuration " +
			"empties it at AutoPilot: an app without `accepts` accepts no sender. Narrowing or deleting an app takes " +
			"effect at once: AutoPilot stops the queued and running tasks the app no longer accepts.\n\n" +
			"A repository belongs to the first app that claims it. AutoPilot can't tell which app should own one, " +
			"so review an app's `repos` as you would its access.\n\n" +
			"`credential` and `when_seats_full` are the app's credential policy: what the model calls of the tasks " +
			"billed to it run on. A change to either sends both.\n\n" +
			"`worker_image`, `worker_cpu` and `worker_memory` choose the worker the tasks billed to the app run in: " +
			"its image, and the size of the AWS Fargate task it runs as. `worker_cpu` and `worker_memory` are one " +
			"size, set together, and a change to either sends both.\n\n" +
			"`auto_review` has AutoPilot review the app's pull requests on its own, with no label, as the app's own " +
			"service identity, and lets the GitHub Actions workflows it lists ask for those reviews. It is one " +
			"setting, and a change to any of its fields sends all of them.\n\n" +
			"`review_also_reads` lets the reviews of the app's repositories read other repositories of the same " +
			"owner, read-only. Anyone who can push a branch to one of the app's repositories, and anyone who can read " +
			"its reviews, can see what those repositories hold.\n\n" +
			"An app with this name that already exists is not taken over, even when its values match: the create " +
			"fails and says to import it. The one exception is a create whose earlier attempt went unanswered (a " +
			"gateway's 502, say) before an identical app answered: that attempt may have made the app, and the " +
			"provider can't tell, so it takes the app as its own. `create_before_destroy` doesn't fit an app: the " +
			"name is its key.\n\n" +
			"Import by name: `terraform import autopilot_app.billing billing`.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				MarkdownDescription: "The app's name, which is its key and the `billing.app` of the work it pays for: a " +
					"lowercase letter or digit, then lowercase letters, digits, `_` and `-`, at most 64 characters. Changing it " +
					"replaces the app: Terraform deletes the old one, which stops its queued and running work at once, then " +
					"creates the new one.",
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators: []validator.String{
					stringvalidator.RegexMatches(appNamePattern, "must be a lowercase letter or digit, then lowercase letters, digits, _ and -"),
					stringvalidator.LengthAtMost(64),
				},
			},
			"repos": schema.SetAttribute{
				MarkdownDescription: "The repositories this app **owns**, as `owner/name`. A repository belongs to at most " +
					"one app. AutoPilot matches them without regard to case, so no two may differ only in case. At most 50.",
				ElementType: types.StringType,
				Optional:    true,
				Validators: []validator.Set{
					setvalidator.SizeAtMost(50),
					setvalidator.ValueStringsAre(stringvalidator.RegexMatches(repoPattern, "must be owner/name")),
					noCaseRepeatsValidator{what: "repositories"},
				},
			},
			"accepts": schema.SetNestedAttribute{
				MarkdownDescription: "The senders this app accepts work from, each at most once, with the kinds of task " +
					"it accepts from each. At most 20.",
				Optional: true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"sender": schema.StringAttribute{
							MarkdownDescription: "The sender's name. It need not exist yet.",
							Required:            true,
							Validators: []validator.String{stringvalidator.RegexMatches(senderNamePattern,
								"must be a sender's name: a lowercase letter, then lowercase letters, digits and -, at most 40 characters")},
						},
						"kinds": schema.SetAttribute{
							MarkdownDescription: "The kinds of task accepted from that sender, such as `implement-work-item` " +
								"or `review-pr`. At least one.",
							ElementType: types.StringType,
							Required:    true,
							Validators: []validator.Set{
								setvalidator.SizeAtLeast(1),
								setvalidator.ValueStringsAre(stringvalidator.RegexMatches(kindPattern, "must be a kind of task, such as implement-work-item")),
							},
						},
						"can_decide": schema.BoolAttribute{
							MarkdownDescription: "Whether that sender's `review-pr` tasks may approve or request changes on " +
								"this app's repositories, which can count as a required review. Only with `review-pr` in " +
								"`kinds`. For `autopilot-mcp` and `autopilot-github`, whose reviews run for the person who " +
								"asked, a review of a pull request that may be that person's own only comments, unless " +
								"`can_decide_own_pr` is set too. That check goes by the pull request's author alone, so it " +
								"doesn't stop a developer who pushes their own commits onto someone else's pull request, or " +
								"a bot's, and then asks: the review may approve their code. GitHub's \"require approval of " +
								"the most recent reviewable push\" doesn't stop that either, since AutoPilot isn't the pusher, " +
								"and GitHub keeps an approval through later pushes unless the branch dismisses stale " +
								"approvals. On a repository where one approval alone lets a pull request merge, keep that in " +
								"mind before turning this on. Defaults to `false`.",
							Optional: true,
							Computed: true,
							Default:  booldefault.StaticBool(false),
						},
						"can_auto_merge": schema.BoolAttribute{
							MarkdownDescription: "Whether GitHub's auto-merge may be turned on for the pull requests that " +
								"sender's `implement-work-item` and `fix-error` tasks open on this app's repositories, when " +
								"the task asks for it. AutoPilot never merges: GitHub merges each pull request once the " +
								"repository's own settings and ruleset allow it, which means the repository allows " +
								"auto-merge and its required checks and reviews pass. Off, a person merges. Only with " +
								"`implement-work-item` or `fix-error` in `kinds`. Off unless set: leaving it out is the " +
								"same as `false`. Turning it off, or removing the entry or the app, leaves auto-merge on " +
								"for a pull request that already has it, and GitHub still merges that pull request: turn " +
								"auto-merge off on the pull request in GitHub.",
							Optional: true,
						},
						"can_decide_own_pr": schema.BoolAttribute{
							MarkdownDescription: "Whether, with `can_decide`, a review that sender makes for a person may " +
								"also approve or request changes on a pull request that may be that person's own. Off, such " +
								"a review only comments. Only for `autopilot-mcp` and `autopilot-github`, whose reviews run " +
								"for a person (the one who asked on AutoPilot's MCP server, or the developer who asked on " +
								"GitHub, with the label or the command), and only with `can_decide = true`. A pull request may be " +
								"the asker's own when its author is a GitHub login that `github_logins` pairs with them, " +
								"compared without regard to case, or when AutoPilot can't tell: GitHub names no author, or " +
								"`github_logins` pairs no login with them. So with this off, a review decides only for an " +
								"asker who has a login in `github_logins`, and only on a pull request whose author isn't one " +
								"of their logins there. An author no login is paired with reads as someone else's, even when " +
								"it is another account of the asker's, so pair every login a developer uses. It goes by the " +
								"author alone, which leaves open what `can_decide` describes: commits pushed onto someone " +
								"else's pull request. Turning it on lets a developer get an approval of their own pull " +
								"request that can count as a required review, so set it only where that is acceptable. Off " +
								"unless set: leaving it out is the same as `false`. When it is turned off, a deciding review " +
								"of what may be the asker's own pull request that is still queued or running is refused or " +
								"stopped; ask for the review again.",
							Optional: true,
						},
						"approve_up_to": schema.StringAttribute{
							MarkdownDescription: "With `can_decide`, the most risk a pull request may carry for that " +
								"sender's reviews to approve it: `low`, `medium` or `high`. Every review states its pull " +
								"request's risk. The agent rates it from reading the pull request, and AutoPilot then " +
								"applies rules of its own that never ask the model and can only raise the rating. For " +
								"example, a change to CI workflows or to the instructions agents read, or a large diff, is " +
								"never `low`, and a repository can name its own never-low paths in an " +
								"`autopilot-never-low-paths` block on its base branch (AutoPilot's admin API documentation " +
								"lists the rules). An approval rated above this is posted as a comment, with its findings and " +
								"its level, saying why. So is one whose rating AutoPilot couldn't read, or whose rules it " +
								"couldn't check, whatever the ceiling, `high` included. A request for changes isn't limited " +
								"by it. Only with `can_decide = true`, for any sender whose reviews may decide. Left out, " +
								"every level may be approved, as with `can_decide` alone. The rating is the model's " +
								"judgment, held up by those rules: a change outside them that a pull request talks its " +
								"review into rating `low` can still be approved under `low`. AutoPilot asks for the " +
								"ceiling just before each approval, so a change reaches reviews already running. An " +
								"AutoPilot release older than this field ignores a stored ceiling and refuses a write that " +
								"sends one, so if AutoPilot is ever rolled back that far, its reviews approve at any level " +
								"until it is rolled forward: take `can_decide` off first where a ceiling matters.",
							Optional:   true,
							Validators: []validator.String{stringvalidator.OneOf("low", "medium", "high")},
						},
					},
				},
				Validators: []validator.Set{
					setvalidator.SizeAtMost(20),
					acceptsValidator{},
				},
			},
			"developers": schema.SetAttribute{
				MarkdownDescription: "The people who may start work for this app themselves, through AutoPilot's MCP " +
					"server, or from GitHub with a login in `github_logins`: their sign-in logins, which are email " +
					"addresses, written in lowercase ASCII (AutoPilot refuses anything else). At most 100.",
				ElementType: types.StringType,
				Optional:    true,
				Validators: []validator.Set{
					setvalidator.SizeAtMost(100),
					setvalidator.ValueStringsAre(emailValidator{}),
				},
			},
			"github_logins": schema.SetNestedAttribute{
				MarkdownDescription: "The GitHub logins of this app's developers, for pull request reviews started from " +
					"GitHub: each pairs a GitHub login with one of `developers`, and a review started from GitHub by that " +
					"login runs in that developer's name. This decides who can start reviews in a developer's name, so " +
					"review each pair as you would `developers`. Every `developer` here must also be in `developers`, so " +
					"take someone's logins out in the same change that takes them out of `developers`. A developer may " +
					"have more than one login, and each login belongs to one developer. At most 100. It also decides " +
					"which pull requests count as a developer's own, for reviews from the MCP and from GitHub alike " +
					"(`can_decide_own_pr`): one whose author isn't among a developer's logins here reads as someone " +
					"else's, unless that developer has no login here at all. So a missing, wrong or stale pair can let " +
					"a review approve someone's own work: pair every login a developer uses. Keep it up to date when " +
					"someone renames their GitHub account: the old login no longer matches them, their pull requests " +
					"under the new login read as someone else's, and someone else may take the old login. Reviews from " +
					"GitHub also need the app to accept the `autopilot-github` sender for `review-pr`.",
				Optional: true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"login": schema.StringAttribute{
							MarkdownDescription: "A GitHub login, written in lowercase (AutoPilot refuses uppercase): a " +
								"letter or digit, then letters, digits, `-` and `_`, at most 39 characters.",
							Required: true,
							Validators: []validator.String{stringvalidator.RegexMatches(gitHubLoginPattern,
								"must be a GitHub login in lowercase: a letter or digit, then letters, digits, - and _, at most 39 characters")},
						},
						"developer": schema.StringAttribute{
							MarkdownDescription: "The developer that login belongs to: their sign-in login, an email " +
								"address in lowercase ASCII, as it is written in `developers`.",
							Required:   true,
							Validators: []validator.String{emailValidator{}},
						},
					},
				},
				Validators: []validator.Set{
					setvalidator.SizeAtMost(100),
					gitHubLoginsValidator{},
				},
			},
			"credential": schema.StringAttribute{
				MarkdownDescription: "What the model calls of the tasks billed to this app run on: `bedrock` (Amazon " +
					"Bedrock, billed per token) or `seat_first` (one of the organization's Claude seats, when one has room). " +
					"Left out, the app has no policy of its own, and AutoPilot's default applies: `bedrock`. AutoPilot " +
					"uses seats only while its seat mode is on; with it off, every task runs on Bedrock. Removing it from " +
					"the configuration takes the app's own policy away. " +
					"The credential is chosen when a task starts, so a change reaches only the tasks that start after it.",
				Optional:   true,
				Validators: []validator.String{stringvalidator.OneOf("bedrock", "seat_first")},
			},
			"when_seats_full": schema.StringAttribute{
				MarkdownDescription: "With `credential = \"seat_first\"`, what a task does when every seat is full: " +
					"`wait` until one has room, or `fall_back` to Bedrock. Required with `seat_first`, and only allowed with it.",
				Optional:   true,
				Validators: []validator.String{stringvalidator.OneOf("wait", "fall_back")},
			},
			"worker_image": schema.StringAttribute{
				MarkdownDescription: "The worker image the tasks billed to this app run in: `node`, `ruby`, " +
					"`ruby-browser`, `python` or `go`. Each is the same worker with that language's tools; `ruby` has " +
					"Node too, for Rails assets. Choose the app's main language. `ruby-browser` is an opt-in image for " +
					"Rails system tests: the `ruby` image with Chromium and ChromeDriver, about 500 MB bigger, so " +
					"choose it only for an app whose tests drive a browser. It needs an AutoPilot that has that image; " +
					"an older AutoPilot refuses it with a 422. Left out, the app's tasks run in `node`, and removing it " +
					"from the configuration takes the app back to `node`. The image is chosen when a task starts, so a " +
					"change reaches only the tasks that start after it.",
				Optional:   true,
				Validators: []validator.String{stringvalidator.OneOf(workerImages...)},
			},
			"worker_cpu": schema.Int64Attribute{
				MarkdownDescription: "The CPU of the AWS Fargate task each of this app's tasks runs in, in CPU units, " +
					"1024 to a vCPU: `256`, `512`, `1024`, `2048` or `4096`. Set together with `worker_memory`. Left out " +
					"with it, a task gets AutoPilot's default size, 2 vCPU and 8 GB, and removing both from the " +
					"configuration takes the app back to it. A bigger task counts against AutoPilot's limits on running " +
					"tasks and daily spend like any other. The size is chosen when a task starts, so a change reaches " +
					"only the tasks that start after it.",
				Optional:   true,
				Validators: []validator.Int64{int64validator.OneOf(256, 512, 1024, 2048, 4096)},
			},
			"worker_memory": schema.Int64Attribute{
				MarkdownDescription: "The memory of that Fargate task, in MiB, at most 16 GB: one that Fargate allows " +
					"with `worker_cpu`, which is " + memoryRule + ". Set together with `worker_cpu`.",
				Optional:   true,
				Validators: []validator.Int64{int64validator.Between(512, 16384)},
			},
			"auto_review": schema.SingleNestedAttribute{
				MarkdownDescription: "Automatic reviews: AutoPilot reviews each pull request into the default branch of " +
					"this app's repositories on its own, with no label or comment, when it is opened, marked ready or " +
					"pushed to, as `events` says, or when a GitHub Actions workflow listed in `workflows` asks. It reviews only a person's pull request, never a bot's (Dependabot's, or " +
					"any account GitHub calls a bot), and never a draft until it is marked ready. It reviews only a pull " +
					"request from a branch of the repository itself, never a fork's, so no one outside the repository " +
					"can have code run with no person asking; a developer's `autopilot-review` label still reviews a " +
					"fork's pull request, only as a comment. The review runs as the app's own service identity, " +
					"`auto-review:<name>`, never a person: that identity asks for it, the app pays, its limits count it, " +
					"and the review's footer, its check run (`AutoPilot review (automatic)`) and AutoPilot's logs name it. " +
					"It posts no other comment. Setting this is the app's consent; it needs no `accepts` entry and no " +
					"developer, and an `accepts` entry for `autopilot-github` doesn't turn it on. A head and base gets " +
					"one review from GitHub, so a later `autopilot-review` label on the same head gets the automatic " +
					"review, even one that only commented. Automatic reviews count only against `max_per_day`, never " +
					"against a repository's daily cap on the reviews people ask for, so they never get a developer's " +
					"label refused, though they share AutoPilot's one running review from GitHub with labels. Left " +
					"out, the app has no " +
					"automatic reviews, and removing it from the configuration turns them off, which also stops the " +
					"ones queued or running; so does taking `can_decide` or `can_decide_own_pr` away, while fewer " +
					"`events`, fewer `workflows` or a lower `max_per_day` reach only what comes after. It needs an AutoPilot that knows the " +
					"field; an older one refuses it with a 422. An AutoPilot rolled back past the field ignores a stored " +
					"setting and refuses every write that sends one, but keeps it, so it is on again once AutoPilot is " +
					"rolled forward, even if it was taken out of the configuration meanwhile (Terraform reads it as " +
					"none, so sends nothing): take it off before such a rollback where that matters.",
				Optional: true,
				Attributes: map[string]schema.Attribute{
					"events": schema.SetAttribute{
						MarkdownDescription: "The pull request actions that start an automatic review on its own: " +
							"`opened`, `ready_for_review` (a draft marked ready) and `synchronize` (each push to it). " +
							"Defaults to `opened` and `ready_for_review`. With `synchronize`, every push is a new head and " +
							"so a new review, within `max_per_day`. At least one, unless `workflows` lists one: `[]` with " +
							"`workflows` means only those workflows ask, and AutoPilot reviews nothing on its own. With " +
							"`workflows` listed it must be set, even to the default, so the configuration says on purpose " +
							"whether pull requests are also reviewed when they open.",
						ElementType: types.StringType,
						Optional:    true,
						Computed:    true,
						Default:     setdefault.StaticValue(setFromStrings(defaultAutoEvents)),
						Validators: []validator.Set{
							setvalidator.ValueStringsAre(stringvalidator.OneOf(client.AutoEvents...)),
						},
					},
					"workflows": schema.SetAttribute{
						MarkdownDescription: "The GitHub Actions workflows that may ask for one of this app's automatic " +
							"reviews, for a repository that wants a review when no event can say, such as once its CI " +
							"passes. A job of a listed workflow asks AutoPilot with the OIDC token GitHub gives a job " +
							"that has `id-token: write`, so no secret is stored anywhere. AutoPilot's own documentation on " +
							"reviews a workflow asks for has a workflow to copy and the checks it makes on each ask. Each " +
							"entry is the workflow as GitHub names it in that token (`job_workflow_ref`): " +
							"`owner/repo/.github/workflows/<file>.yml@refs/heads/<branch>` (or `.yaml`), such as " +
							"`example-org/billing-api/.github/workflows/autopilot-review.yml@refs/heads/main`. Only a " +
							"branch: a pull request's ref, a tag or a commit is refused, so a pull request that changes " +
							"the workflow can't ask with its own copy. `owner` is letters, digits and `-`, at most 39 " +
							"characters; `repo` is letters, digits, `.`, `_` and `-`, at most 100, and not dots alone; the " +
							"file is straight in `.github/workflows`, letters, digits, `.`, `_` and `-`, at most 200, then " +
							"`.yml` or `.yaml`; and the branch is letters, digits, `.`, `_`, `/` and `-`, at most 200, as " +
							"git allows it: not starting with `.`, `-` or `/`, not ending with `/`, `.` or `.lock`, and " +
							"with no `..`, `//` or `/.`. AutoPilot compares `owner/repo` without regard to case and the " +
							"rest exactly, so no two entries may differ only in the case of `owner/repo`. At most 20. " +
							"Defaults to none. The repository the job runs in must be one this app owns. The workflow " +
							"file may be in another repository, called as a reusable workflow, and then whoever can push " +
							"to that branch there decides what asks. An ask is one of this app's automatic reviews in " +
							"every other way: the same identity, `max_per_day`, deciding knobs and check run, and the same " +
							"rules on which pull requests get one. With any listed, `events` must be set. It needs an " +
							"AutoPilot that knows the field; an older one refuses it with a 422, and with `events = []` " +
							"names `events` in the refusal, since it takes no empty `events`. An AutoPilot rolled back " +
							"past the field refuses every write that sends it, reads an app that lists workflows and no " +
							"event as having no automatic reviews, and drops the workflows on any write it takes to the " +
							"app; the next apply after AutoPilot is rolled forward sends them again. Take them off before " +
							"such a rollback where that matters.",
						ElementType: types.StringType,
						Optional:    true,
						Computed:    true,
						Default:     setdefault.StaticValue(setFromStrings(nil)),
						Validators: []validator.Set{
							setvalidator.SizeAtMost(maxAutoWorkflows),
							setvalidator.ValueStringsAre(workflowRefValidator{}),
						},
					},
					"max_per_day": schema.Int64Attribute{
						MarkdownDescription: "The most automatic reviews of this app's repositories in 24 hours, every " +
							"one AutoPilot recorded counting, however it ended: `1` to `200`. Defaults to `50`. It is their " +
							"only daily cap: they don't count against a repository's cap on the reviews people ask for, and " +
							"those don't count against it. Each runs only on one of the organization's Claude seats, one " +
							"review from GitHub at a time.",
						Optional:   true,
						Computed:   true,
						Default:    int64default.StaticInt64(defaultAutoPerDay),
						Validators: []validator.Int64{int64validator.Between(1, maxAutoPerDay)},
					},
					"can_decide": schema.BoolAttribute{
						MarkdownDescription: "Whether automatic reviews may approve or request changes, which can count " +
							"as a required review. Independent of any `accepts` entry's `can_decide`, `autopilot-github`'s " +
							"included. A review decides only on a pull request into the default branch, and requests " +
							"changes for any finding it marks Important. Defaults to `false`.",
						Optional: true,
						Computed: true,
						Default:  booldefault.StaticBool(false),
					},
					"can_decide_own_pr": schema.BoolAttribute{
						MarkdownDescription: "Whether, with `can_decide = true`, an automatic review may also decide on a " +
							"pull request that may be one of this app's developers' own. Off, such a review only comments. " +
							"A pull request may be a developer's when its author is a GitHub login that `github_logins` " +
							"pairs with any developer, compared without regard to case, or when AutoPilot can't tell: " +
							"GitHub names no author, or one of `developers` has no login in `github_logins`, and so could " +
							"be any author. So with this off, automatic reviews decide only once every developer's GitHub " +
							"login is paired, and only on a pull request whose author isn't one of those logins. It goes " +
							"by the author alone, which leaves open what `accepts`' `can_decide` describes: commits pushed " +
							"onto someone else's pull request. Turning it on lets a developer's own pull request get an " +
							"approval no other person gave, which can count as a required review: set it only where that " +
							"is acceptable. Defaults to `false`.",
						Optional: true,
						Computed: true,
						Default:  booldefault.StaticBool(false),
					},
					"approve_up_to": schema.StringAttribute{
						MarkdownDescription: "With `can_decide = true`, the most risk a pull request may carry for an " +
							"automatic review to approve it: `low`, `medium` or `high`, rated as for `accepts`' " +
							"`approve_up_to`. An approval rated above it is posted as a comment. Left out, every level may " +
							"be approved.",
						Optional:   true,
						Validators: []validator.String{stringvalidator.OneOf("low", "medium", "high")},
					},
				},
			},
			"review_also_reads": schema.SetAttribute{
				MarkdownDescription: "Other repositories that the pull request reviews of this app's own repositories " +
					"also read, read-only, as `owner/name`: such as the Terraform modules repository whose modules this " +
					"app's roots use, so a review can read a module at the ref a `source` pins. Only the reviews of this " +
					"app's repositories read them, whoever asked for the review, and only of a pull request from a branch " +
					"of the repository itself, never a fork's. No other kind of task, and no other app's review, ever " +
					"does. Each review reads only those of its own repository's owner. Before the review starts, " +
					"AutoPilot clones each, every branch and tag, with GitHub tokens of their own that can only read " +
					"them, never the review's own token, and the review reads them as data, never as instructions: their " +
					"files for agents, such as `AGENTS.md`, don't apply to it. **This app's " +
					"repositories' writers and the readers of its reviews can see what these repositories hold:** a " +
					"review runs the pull request's own code, so anyone who can push a branch to one of this app's " +
					"repositories and have it reviewed can read everything they hold, every branch and tag, and a review, " +
					"posted on this app's repository, may quote them. List only repositories those people may read, and " +
					"review this list as you would `repos`. Each must have the same owner as one of `repos`, compared " +
					"without regard to case, since a review reads only repositories of its own repository's owner, and " +
					"none may be one of `repos`, which a review already reads. So an app with no `repos` can list none, " +
					"and a change that takes a repository out of `repos` must take out the ones only its owner allowed. " +
					"Each is `owner/name`: the owner letters, digits and `-`, at most 39 characters, and the name " +
					"letters, digits, `.`, `_` and `-`, at most 100, and not dots alone. AutoPilot compares them without " +
					"regard to case, so no two may differ only in case. At most 5. An AutoPilot that limits which " +
					"repositories its apps may reach refuses one outside that limit when it is applied. A repository the " +
					"GitHub App isn't installed on, that GitHub doesn't know, or that is too large or too slow to clone, " +
					"is left out of that review, which goes on and says so. Left out, reviews read only their own " +
					"repository, and removing it from the configuration takes every one away. A change reaches each " +
					"review that starts after it. Listing some needs an AutoPilot that knows the field; an older one " +
					"refuses it with a 422. Listing none, left out or `[]`, never sends it. An AutoPilot rolled back past " +
					"the field ignores the stored list, so reviews read only their own repository, and refuses every " +
					"write that sends it, but keeps it. Meanwhile the provider reads the app as listing none, so a " +
					"configuration that lists some plans a change on every run and its applies fail: take them out of " +
					"the configuration until AutoPilot is rolled forward. Then the next plan shows the list AutoPilot " +
					"kept, and the apply takes it away unless the configuration lists it again.",
				ElementType: types.StringType,
				Optional:    true,
				Validators: []validator.Set{
					setvalidator.SizeAtMost(maxAlsoReads),
					setvalidator.ValueStringsAre(alsoReadsRepoValidator{}),
					noCaseRepeatsValidator{what: "repositories"},
				},
			},
			"lock_version": schema.Int64Attribute{
				MarkdownDescription: "The record's version, which AutoPilot raises on every change. Sent as `If-Match` " +
					"on updates and deletes.",
				Computed: true,
			},
		},
	}
}

// ValidateConfig checks the rules that span two arguments: the credential
// policy's, the worker size's, that each GitHub login's developer is one of
// the developers, automatic reviews' deciding knobs, events and workflows,
// and what reviews also read beside the app's own repositories. A value not
// known yet waits: Terraform validates again at apply.
func (r *appResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	validatePolicy(ctx, req, resp)
	validateSize(ctx, req, resp)
	validateGitHubLogins(ctx, req, resp)
	validateAutoReview(ctx, req, resp)
	validateReviewAlsoReads(ctx, req, resp)
}

// validateReviewAlsoReads checks each repository the app's reviews also read
// against the app's own repositories, as AutoPilot does on a create and on
// any change that sends either: of the same owner as one of them, without
// regard to case, and never one of them. The configuration is what AutoPilot
// will hold, since a list left out is sent as [] once it was set, so repos
// left out is no repository at all. It waits while repos, or one of them, is
// not known yet; a repository to read that isn't known yet, or that its own
// validator refuses, is skipped.
func validateReviewAlsoReads(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var repos, alsoReads types.Set
	var diags diag.Diagnostics
	diags.Append(req.Config.GetAttribute(ctx, path.Root("repos"), &repos)...)
	diags.Append(req.Config.GetAttribute(ctx, path.Root("review_also_reads"), &alsoReads)...)
	resp.Diagnostics.Append(diags...)
	if diags.HasError() || alsoReads.IsNull() || alsoReads.IsUnknown() || repos.IsUnknown() {
		return
	}
	own, owners := map[string]bool{}, map[string]bool{}
	for _, elem := range repos.Elements() {
		s, ok := elem.(types.String)
		if !ok || s.IsUnknown() {
			return
		}
		if s.IsNull() {
			continue
		}
		own[strings.ToLower(s.ValueString())], owners[repoOwner(s.ValueString())] = true, true
	}
	for _, elem := range alsoReads.Elements() {
		s, ok := elem.(types.String)
		if !ok || !known(s) || !validRepo(s.ValueString()) {
			continue
		}
		if summary, detail := alsoReadsProblem(s.ValueString(), own, owners); summary != "" {
			resp.Diagnostics.AddAttributeError(path.Root("review_also_reads"), summary, detail)
		}
	}
}

// validateAutoReview checks automatic reviews' fields that depend on each
// other as AutoPilot does: the deciding knobs, the events beside the
// workflows, and the workflows' repositories' case.
func validateAutoReview(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var auto types.Object
	diags := req.Config.GetAttribute(ctx, path.Root("auto_review"), &auto)
	resp.Diagnostics.Append(diags...)
	if diags.HasError() || auto.IsNull() || auto.IsUnknown() {
		return
	}
	var m autoReviewModel
	if d := auto.As(ctx, &m, basetypes.ObjectAsOptions{}); d.HasError() {
		return
	}
	validateAutoReviewKnobs(m, resp)
	validateAutoReviewEvents(m, resp)
	validateAutoReviewWorkflows(m, resp)
}

// validateAutoReviewEvents checks events beside workflows, as AutoPilot
// does: no event is allowed only with a workflow that may ask, and with one,
// events must be set rather than left to its default, which would review
// every pull request when it opens, before any workflow asks. AutoPilot
// refuses workflows sent without events for the same reason, and the
// default in the plan would hide that the configuration left them out.
// Workflows not known yet wait.
func validateAutoReviewEvents(m autoReviewModel, resp *resource.ValidateConfigResponse) {
	if m.Workflows.IsUnknown() {
		return
	}
	// A set with a value not known yet still has at least one value.
	listsWorkflows := !m.Workflows.IsNull() && len(m.Workflows.Elements()) > 0
	switch {
	case listsWorkflows && m.Events.IsNull():
		resp.Diagnostics.AddAttributeError(path.Root("auto_review").AtName("events"), "events is required with workflows",
			"auto_review lists workflows, so it must also say which events start a review on their own: "+
				"events = [] for only the workflows to ask, or the events, such as [\"opened\", \"ready_for_review\"]. "+
				"AutoPilot refuses workflows without events, so that its default never reviews every pull request "+
				"when it opens unasked.")
	case !listsWorkflows && !m.Events.IsNull() && !m.Events.IsUnknown() && len(m.Events.Elements()) == 0:
		resp.Diagnostics.AddAttributeError(path.Root("auto_review").AtName("events"), "No event and no workflow",
			"auto_review's events is empty and it lists no workflows, so nothing could start a review. Name at least "+
				"one event, list the workflows that may ask in workflows, or take auto_review out to turn automatic "+
				"reviews off.")
	}
}

// validateAutoReviewWorkflows refuses two workflows that differ only in the
// case of owner/repo, as AutoPilot does: it compares the repository without
// regard to case and the file and branch exactly. A set already keeps an
// entry listed twice exactly once. Values not known yet, or that their own
// validator refuses, are skipped.
func validateAutoReviewWorkflows(m autoReviewModel, resp *resource.ValidateConfigResponse) {
	if m.Workflows.IsNull() || m.Workflows.IsUnknown() {
		return
	}
	seen := map[string]string{}
	for _, elem := range m.Workflows.Elements() {
		s, ok := elem.(types.String)
		if !ok || !known(s) {
			continue
		}
		ref, problem := parseWorkflowRef(s.ValueString())
		if problem != "" {
			continue
		}
		if first, dup := seen[ref.key()]; dup {
			resp.Diagnostics.AddAttributeError(path.Root("auto_review").AtName("workflows"), "Repeated workflow",
				fmt.Sprintf("%q and %q name the same workflow to AutoPilot, which compares owner/repo without regard "+
					"to case. Keep one of them.", first, s.ValueString()))
			return
		}
		seen[ref.key()] = s.ValueString()
	}
}

// validateAutoReviewKnobs checks automatic reviews' knobs as AutoPilot does:
// can_decide_own_pr and approve_up_to each narrow can_decide, so each needs
// it. can_decide left out is false (its default); one not known yet waits.
func validateAutoReviewKnobs(m autoReviewModel, resp *resource.ValidateConfigResponse) {
	if m.CanDecide.IsUnknown() || isTrue(m.CanDecide) {
		return
	}
	if isTrue(m.CanDecideOwnPR) {
		resp.Diagnostics.AddAttributeError(path.Root("auto_review").AtName("can_decide_own_pr"),
			"can_decide_own_pr without can_decide",
			"auto_review sets can_decide_own_pr, which only widens can_decide, but can_decide isn't true. Set "+
				"can_decide = true too, or leave can_decide_own_pr out.")
	}
	if known(m.ApproveUpTo) {
		resp.Diagnostics.AddAttributeError(path.Root("auto_review").AtName("approve_up_to"),
			"approve_up_to without can_decide",
			"auto_review sets approve_up_to, the most risk its reviews may approve, but can_decide isn't true, so "+
				"they approve nothing. Set can_decide = true too, or leave approve_up_to out.")
	}
}

// validateGitHubLogins checks that every developer github_logins names is in
// developers, as AutoPilot does on each write that sends either. It waits
// while developers, or a developer it would check, is not known yet.
func validateGitHubLogins(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var developers, logins types.Set
	var diags diag.Diagnostics
	diags.Append(req.Config.GetAttribute(ctx, path.Root("developers"), &developers)...)
	diags.Append(req.Config.GetAttribute(ctx, path.Root("github_logins"), &logins)...)
	resp.Diagnostics.Append(diags...)
	if diags.HasError() || developers.IsUnknown() || logins.IsNull() || logins.IsUnknown() {
		return
	}
	listed := map[string]bool{}
	for _, elem := range developers.Elements() {
		s, ok := elem.(types.String)
		if !ok || s.IsUnknown() {
			return
		}
		listed[s.ValueString()] = true
	}
	for _, elem := range logins.Elements() {
		obj, ok := elem.(types.Object)
		if !ok || obj.IsNull() || obj.IsUnknown() {
			continue
		}
		var m gitHubLoginModel
		if d := obj.As(ctx, &m, basetypes.ObjectAsOptions{}); d.HasError() || !known(m.Developer) {
			continue
		}
		if developer := m.Developer.ValueString(); !listed[developer] {
			resp.Diagnostics.AddAttributeError(path.Root("github_logins"), "GitHub login for someone not in developers",
				fmt.Sprintf("The GitHub login %q is paired with %q, who isn't in developers. Each login's developer "+
					"must be one of the app's developers: add %q to developers, or take the login out. To take "+
					"someone out of developers, take their GitHub logins out in the same change.",
					m.Login.ValueString(), developer, developer))
		}
	}
}

// validateSize checks the worker size: worker_cpu and worker_memory both or
// neither, and a pair Fargate runs. A worker_cpu that is none of the sizes
// is left to its own validator.
func validateSize(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cpu, memory types.Int64
	var diags diag.Diagnostics
	diags.Append(req.Config.GetAttribute(ctx, path.Root("worker_cpu"), &cpu)...)
	diags.Append(req.Config.GetAttribute(ctx, path.Root("worker_memory"), &memory)...)
	resp.Diagnostics.Append(diags...)
	if diags.HasError() || cpu.IsUnknown() || memory.IsUnknown() {
		return
	}
	allowed, knownCPU := workerSizes[cpu.ValueInt64()]
	switch {
	case cpu.IsNull() && memory.IsNull():
	case cpu.IsNull():
		resp.Diagnostics.AddAttributeError(path.Root("worker_cpu"), "worker_cpu is required with worker_memory",
			"worker_cpu and worker_memory are one size: set both, or leave both out for AutoPilot's default size.")
	case memory.IsNull():
		resp.Diagnostics.AddAttributeError(path.Root("worker_memory"), "worker_memory is required with worker_cpu",
			"worker_cpu and worker_memory are one size: set both, or leave both out for AutoPilot's default size.")
	case knownCPU && !slices.Contains(allowed, memory.ValueInt64()):
		resp.Diagnostics.AddAttributeError(path.Root("worker_memory"), "worker_memory doesn't go with worker_cpu",
			"AWS Fargate allows only some memory with each CPU, at most 16 GB here: "+
				strings.ReplaceAll(memoryRule, "`", "")+".")
	}
}

// validatePolicy checks the rule that spans the credential policy's two
// arguments: when_seats_full with seat_first, and only with it.
func validatePolicy(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var credential, whenFull types.String
	var diags diag.Diagnostics
	diags.Append(req.Config.GetAttribute(ctx, path.Root("credential"), &credential)...)
	diags.Append(req.Config.GetAttribute(ctx, path.Root("when_seats_full"), &whenFull)...)
	resp.Diagnostics.Append(diags...)
	if diags.HasError() || credential.IsUnknown() || whenFull.IsUnknown() {
		return
	}
	seatFirst := credential.ValueString() == "seat_first"
	switch {
	case seatFirst && whenFull.IsNull():
		resp.Diagnostics.AddAttributeError(path.Root("when_seats_full"), "when_seats_full is required with seat_first",
			"credential = \"seat_first\" needs when_seats_full: \"wait\" for a seat to have room, or \"fall_back\" to "+
				"Bedrock when every seat is full.")
	case !seatFirst && !whenFull.IsNull():
		resp.Diagnostics.AddAttributeError(path.Root("when_seats_full"), "when_seats_full only with seat_first",
			"when_seats_full applies only to credential = \"seat_first\". Set credential to \"seat_first\", or leave "+
				"when_seats_full out.")
	}
}

func (r *appResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan appModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := plan.Name.ValueString()
	// Only what is set is sent; the rest takes AutoPilot's defaults (empty,
	// no credential policy of its own, the node worker at its own size).
	spec := client.AppSpec{
		Repos:        stringsFromSet(ctx, plan.Repos, "repos", &resp.Diagnostics),
		Accepts:      acceptsFromSet(ctx, plan.Accepts, &resp.Diagnostics),
		Developers:   stringsFromSet(ctx, plan.Developers, "developers", &resp.Diagnostics),
		GitHubLogins: gitHubLoginsFromSet(ctx, plan.GitHubLogins, &resp.Diagnostics),
	}
	// Sent only when it lists some: none is AutoPilot's default, and an
	// AutoPilot that predates the field refuses it, [] included, so an app
	// that lists none still works with it.
	if !noneSet(plan.ReviewAlsoReads) {
		spec.ReviewAlsoReads = stringsFromSet(ctx, plan.ReviewAlsoReads, "review_also_reads", &resp.Diagnostics)
	}
	if !plan.Credential.IsNull() {
		spec.Policy = policyFromModel(plan, &resp.Diagnostics)
	}
	if !plan.WorkerImage.IsNull() {
		spec.WorkerImage = imageFromModel(plan, &resp.Diagnostics)
	}
	if !plan.WorkerCPU.IsNull() || !plan.WorkerMemory.IsNull() {
		spec.Size = sizeFromModel(plan, &resp.Diagnostics)
	}
	if !plan.AutoReview.IsNull() {
		spec.AutoReview = autoReviewFromModel(ctx, plan, &resp.Diagnostics)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	created, outcome, err := r.client.CreateApp(ctx, name, spec)
	if err != nil {
		addAppWriteError(&resp.Diagnostics, "Error creating AutoPilot app", name, err)
		return
	}
	// An app is a unit of consent, and nothing in an identical create proves
	// whose it is: another configuration may declare the same app. So an app
	// this create didn't make is not taken over; the user imports it if this
	// configuration should own it. (A sender's create proves more; see
	// senderResource.Create.)
	if outcome == client.Adopted {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "An app with this name already exists",
			fmt.Sprintf("AutoPilot already has an app named %q, with exactly these values, which this apply did not "+
				"make: another configuration may manage it, or an earlier apply's state was lost. Terraform doesn't "+
				"take over an app's consent unasked, so nothing was changed. If this configuration should manage "+
				"it, import it (terraform import autopilot_app.<resource name> %s) and apply.", name, name))
		return
	}
	state := appToModel(created, plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func addAppWriteError(diags *diag.Diagnostics, summary, name string, err error) {
	switch {
	case client.HasCode(err, client.CodeNameTaken):
		diags.AddAttributeError(path.Root("name"), "An app with this name already exists",
			fmt.Sprintf("AutoPilot already has an app named %q, with values that differ from this configuration. "+
				"To manage it here, import it (terraform import autopilot_app.<resource name> %s) and apply. "+
				"Otherwise choose another name.\n\nAutoPilot said: %s", name, name, apiMessage(err)))
	case client.HasCode(err, client.CodeRepoTaken):
		diags.AddAttributeError(path.Root("repos"), "Another app owns this repository",
			"A repository belongs to at most one app. Remove it from the other app first, or from this one.\n\n"+
				"AutoPilot said: "+apiMessage(err))
	default:
		addAPIError(diags, summary, err, appAPIFields)
	}
}

func (r *appResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state appModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	a, err := r.client.GetApp(ctx, state.Name.ValueString())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		addAPIError(&resp.Diagnostics, "Error reading AutoPilot app", err, appAPIFields)
		return
	}
	newState := appToModel(a, state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Update sends only what changed. A list removed from the configuration is
// sent as [], so AutoPilot empties it as the plan says; a credential policy
// removed is sent as credential null, which takes it away, and so are a
// worker image, a worker size and automatic reviews removed.
func (r *appResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state appModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()

	var spec client.AppSpec
	if !plan.Repos.Equal(state.Repos) {
		spec.Repos = orEmptyStrings(stringsFromSet(ctx, plan.Repos, "repos", &resp.Diagnostics))
	}
	if !plan.Accepts.Equal(state.Accepts) {
		spec.Accepts = acceptsFromSet(ctx, plan.Accepts, &resp.Diagnostics)
		if spec.Accepts == nil {
			spec.Accepts = []client.Accept{}
		}
	}
	if !plan.Developers.Equal(state.Developers) {
		spec.Developers = orEmptyStrings(stringsFromSet(ctx, plan.Developers, "developers", &resp.Diagnostics))
	}
	// Sent with developers or apart: AutoPilot checks the pairs against what
	// the change leaves, so taking a developer and their logins out in one
	// change sends both in one write.
	if !plan.GitHubLogins.Equal(state.GitHubLogins) {
		spec.GitHubLogins = gitHubLoginsFromSet(ctx, plan.GitHubLogins, &resp.Diagnostics)
		if spec.GitHubLogins == nil {
			spec.GitHubLogins = []client.GitHubLogin{}
		}
	}
	// The two are one policy, so a change to either sends both.
	if !plan.Credential.Equal(state.Credential) || !plan.WhenSeatsFull.Equal(state.WhenSeatsFull) {
		spec.Policy = policyFromModel(plan, &resp.Diagnostics)
	}
	if !plan.WorkerImage.Equal(state.WorkerImage) {
		spec.WorkerImage = imageFromModel(plan, &resp.Diagnostics)
	}
	// The size is one too, so a change to either sends both.
	if !plan.WorkerCPU.Equal(state.WorkerCPU) || !plan.WorkerMemory.Equal(state.WorkerMemory) {
		spec.Size = sizeFromModel(plan, &resp.Diagnostics)
	}
	// Automatic reviews are one setting too, so a change to any field sends
	// them whole; removed, they are sent as null, which turns them off.
	if !plan.AutoReview.Equal(state.AutoReview) {
		spec.AutoReview = autoReviewFromModel(ctx, plan, &resp.Diagnostics)
	}
	// Sent with repos or apart: AutoPilot checks the two against what the
	// change leaves, so taking a repository and the ones only its owner
	// allowed out in one change sends both in one write. Removed, it is sent
	// as [], which takes them all away. It is never sent unless it changed,
	// nor from none to none (null and [] alike), so an app that lists none
	// still works with an AutoPilot that predates the field.
	noneToNone := noneSet(plan.ReviewAlsoReads) && noneSet(state.ReviewAlsoReads)
	if !plan.ReviewAlsoReads.Equal(state.ReviewAlsoReads) && !noneToNone {
		spec.ReviewAlsoReads = orEmptyStrings(stringsFromSet(ctx, plan.ReviewAlsoReads, "review_also_reads", &resp.Diagnostics))
	}
	if resp.Diagnostics.HasError() {
		return
	}

	var updated *client.App
	var err error
	if spec.Empty() {
		// Nothing AutoPilot holds would change; read the record as it is.
		updated, err = r.client.GetApp(ctx, name)
	} else {
		updated, err = r.client.UpdateApp(ctx, name, spec, state.LockVersion.ValueInt64())
	}
	if err != nil {
		if client.IsStale(err) {
			var current *int64
			if fresh, rerr := r.client.GetApp(ctx, name); rerr == nil {
				current = &fresh.LockVersion
			}
			addStaleError(&resp.Diagnostics, fmt.Sprintf("App %q", name), state.LockVersion.ValueInt64(), current, err)
			return
		}
		addAppWriteError(&resp.Diagnostics, "Error updating AutoPilot app", name, err)
		return
	}
	newState := appToModel(updated, plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func orEmptyStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func (r *appResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state appModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	err := deleteWithIfMatch(ctx, state.LockVersion.ValueInt64(),
		func(ctx context.Context, lv int64) error { return r.client.DeleteApp(ctx, name, lv) },
		func(ctx context.Context) (int64, error) {
			fresh, err := r.client.GetApp(ctx, name)
			if err != nil {
				return 0, err
			}
			return fresh.LockVersion, nil
		})
	if err != nil {
		if client.IsStale(err) {
			addStaleError(&resp.Diagnostics, fmt.Sprintf("App %q", name), state.LockVersion.ValueInt64(), nil, err)
			return
		}
		addAPIError(&resp.Diagnostics, "Error deleting AutoPilot app", err, appAPIFields)
	}
}

// ImportState imports by name. An empty list reads as unset; set it to [] in
// configuration and the next apply only records that.
func (r *appResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	name := req.ID
	if !appNamePattern.MatchString(name) {
		resp.Diagnostics.AddError("Invalid import id",
			fmt.Sprintf("Import an app by its name (for example `billing`), got %q.", name))
		return
	}
	a, err := r.client.GetApp(ctx, name)
	if err != nil {
		if client.IsNotFound(err) {
			resp.Diagnostics.AddError("No such app", fmt.Sprintf("AutoPilot has no app named %q.", name))
			return
		}
		addAPIError(&resp.Diagnostics, "Error importing AutoPilot app", err, appAPIFields)
		return
	}
	state := appToModel(a, appModel{
		Repos:           types.SetNull(types.StringType),
		Accepts:         types.SetNull(acceptObjectType),
		Developers:      types.SetNull(types.StringType),
		GitHubLogins:    types.SetNull(gitHubLoginObjectType),
		ReviewAlsoReads: types.SetNull(types.StringType),
	})
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
