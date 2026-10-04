package provider

import (
	"context"
	"fmt"

	"github.com/CruGlobal/terraform-provider-autopilot/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = &appResource{}
	_ resource.ResourceWithConfigure   = &appResource{}
	_ resource.ResourceWithImportState = &appResource{}
)

// NewAppResource returns the autopilot_app resource.
func NewAppResource() resource.Resource { return &appResource{} }

type appResource struct {
	client *client.Client
}

type appModel struct {
	Name        types.String `tfsdk:"name"`
	Repos       types.Set    `tfsdk:"repos"`
	Accepts     types.Set    `tfsdk:"accepts"`
	Developers  types.Set    `tfsdk:"developers"`
	LockVersion types.Int64  `tfsdk:"lock_version"`
}

type acceptModel struct {
	Sender    types.String `tfsdk:"sender"`
	Kinds     types.Set    `tfsdk:"kinds"`
	CanDecide types.Bool   `tfsdk:"can_decide"`
}

var acceptAttrTypes = map[string]attr.Type{
	"sender":     types.StringType,
	"kinds":      types.SetType{ElemType: types.StringType},
	"can_decide": types.BoolType,
}

var acceptObjectType = types.ObjectType{AttrTypes: acceptAttrTypes}

// appAPIFields maps the API's field names to this resource's attributes.
var appAPIFields = map[string]string{
	"name":       "name",
	"repos":      "repos",
	"accepts":    "accepts",
	"developers": "developers",
}

// appToModel maps an app. Every list is optional and left out of the
// configuration is null, while AutoPilot answers [] for it, so an empty
// answer stays null where prior (the plan, or the state) was null.
func appToModel(a *client.App, prior appModel) appModel {
	return appModel{
		Name:        types.StringValue(a.Name),
		Repos:       setOrNull(a.Repos, prior.Repos),
		Accepts:     acceptsToSet(a.Accepts, prior.Accepts),
		Developers:  setOrNull(a.Developers, prior.Developers),
		LockVersion: types.Int64Value(a.LockVersion),
	}
}

func acceptsToSet(accepts []client.Accept, prior types.Set) types.Set {
	if len(accepts) == 0 && prior.IsNull() {
		return types.SetNull(acceptObjectType)
	}
	elems := make([]attr.Value, 0, len(accepts))
	for _, a := range accepts {
		elems = append(elems, types.ObjectValueMust(acceptAttrTypes, map[string]attr.Value{
			"sender":     types.StringValue(a.Sender),
			"kinds":      setFromStrings(a.Kinds),
			"can_decide": types.BoolValue(a.CanDecide),
		}))
	}
	return types.SetValueMust(acceptObjectType, elems)
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
		out = append(out, client.Accept{
			Sender:    m.Sender.ValueString(),
			Kinds:     stringsFromSet(ctx, m.Kinds, "accepts", diags),
			CanDecide: m.CanDecide.ValueBool(),
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
			"themselves.\n\n" +
			"AutoPilot takes a sender's task only when the app the task is billed to, and the app that owns each " +
			"repository the task touches, accept that sender for that kind. An app may accept a sender that " +
			"doesn't exist yet, so the app and the sender can be applied in either order.\n\n" +
			"Every argument but `name` is optional and sent only when set. Removing one from the configuration " +
			"empties it at AutoPilot: an app without `accepts` accepts no sender. Narrowing or deleting an app takes " +
			"effect at once: AutoPilot stops the queued and running tasks the app no longer accepts.\n\n" +
			"A repository belongs to the first app that claims it. AutoPilot can't tell which app should own one, " +
			"so review an app's `repos` as you would its access.\n\n" +
			"An app with this name that already exists, made by anything but this resource, is never taken over, " +
			"even when its values match: the create fails and says to import it. `create_before_destroy` doesn't " +
			"fit an app: the name is its key.\n\n" +
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
								"`kinds`. Defaults to `false`.",
							Optional: true,
							Computed: true,
							Default:  booldefault.StaticBool(false),
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
					"server: their sign-in logins, which are email addresses, written in lowercase ASCII (AutoPilot refuses " +
					"anything else). At most 100.",
				ElementType: types.StringType,
				Optional:    true,
				Validators: []validator.Set{
					setvalidator.SizeAtMost(100),
					setvalidator.ValueStringsAre(stringvalidator.RegexMatches(loginPattern, "must be an email address in lowercase ASCII")),
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

func (r *appResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan appModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := plan.Name.ValueString()
	// Only what is set is sent; the rest takes AutoPilot's defaults (empty).
	spec := client.AppSpec{
		Repos:      stringsFromSet(ctx, plan.Repos, "repos", &resp.Diagnostics),
		Accepts:    acceptsFromSet(ctx, plan.Accepts, &resp.Diagnostics),
		Developers: stringsFromSet(ctx, plan.Developers, "developers", &resp.Diagnostics),
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

// Update sends only what changed. An argument removed from the configuration
// is sent as [], so AutoPilot empties it as the plan says.
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
		Repos:      types.SetNull(types.StringType),
		Accepts:    types.SetNull(acceptObjectType),
		Developers: types.SetNull(types.StringType),
	})
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
