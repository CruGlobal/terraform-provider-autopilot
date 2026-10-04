package provider

import (
	"context"
	"fmt"

	"github.com/CruGlobal/terraform-provider-autopilot/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// A sender's two secrets are the design problem here. AutoPilot never sends
// them back; it reports a fingerprint of each instead (the first 16 hex
// characters of the SHA-256 of "autopilot-fingerprint:" and the secret).
//
//   - Both secrets are WRITE-ONLY arguments (Terraform 1.11 or later):
//     Terraform sends them on apply and never puts them in the plan or the
//     state file.
//   - A write-only value is null in the plan, so changing it cannot by itself
//     produce a diff. ModifyPlan works out the fingerprints of the configured
//     secrets and plans them as the new fingerprints. When they differ from
//     the stored ones, that is a planned update, and the update sends both
//     secrets. That is also how a secret changed outside Terraform is put
//     back.
//   - A secret that is unknown at plan time (made by another resource in the
//     same apply) cannot be fingerprinted yet, so an update is planned and
//     the apply decides.
//   - `secrets_wo_version` is the usual companion of write-only arguments:
//     changing it sends the secrets whatever the fingerprints say.

var (
	_ resource.Resource                   = &senderResource{}
	_ resource.ResourceWithConfigure      = &senderResource{}
	_ resource.ResourceWithImportState    = &senderResource{}
	_ resource.ResourceWithModifyPlan     = &senderResource{}
	_ resource.ResourceWithValidateConfig = &senderResource{}
)

// NewSenderResource returns the autopilot_sender resource.
func NewSenderResource() resource.Resource { return &senderResource{} }

type senderResource struct {
	client *client.Client
}

type senderModel struct {
	Name                      types.String `tfsdk:"name"`
	CallbackHosts             types.Set    `tfsdk:"callback_hosts"`
	Kinds                     types.Set    `tfsdk:"kinds"`
	BranchPrefix              types.String `tfsdk:"branch_prefix"`
	RequestSecretWO           types.String `tfsdk:"request_secret_wo"`
	CallbackSecretWO          types.String `tfsdk:"callback_secret_wo"`
	SecretsWOVersion          types.Int64  `tfsdk:"secrets_wo_version"`
	RequestSecretFingerprint  types.String `tfsdk:"request_secret_fingerprint"`
	CallbackSecretFingerprint types.String `tfsdk:"callback_secret_fingerprint"`
	SecretsChangedAt          types.String `tfsdk:"secrets_changed_at"`
	PreviousSecretsUntil      types.String `tfsdk:"previous_secrets_until"`
	LockVersion               types.Int64  `tfsdk:"lock_version"`
}

// senderAPIFields maps the API's field names to this resource's attributes,
// so a refusal that names a field points at the attribute.
var senderAPIFields = map[string]string{
	"name":            "name",
	"callback_hosts":  "callback_hosts",
	"kinds":           "kinds",
	"branch_prefix":   "branch_prefix",
	"request_secret":  "request_secret_wo",
	"callback_secret": "callback_secret_wo",
}

// senderToModel maps a sender. The secrets are write-only and never in state,
// so they are always null here; secrets_wo_version is carried by the caller.
func senderToModel(s *client.Sender, secretsVersion types.Int64) senderModel {
	return senderModel{
		Name:                      types.StringValue(s.Name),
		CallbackHosts:             setFromStrings(s.CallbackHosts),
		Kinds:                     setFromStrings(s.Kinds),
		BranchPrefix:              types.StringValue(s.BranchPrefix),
		RequestSecretWO:           types.StringNull(),
		CallbackSecretWO:          types.StringNull(),
		SecretsWOVersion:          secretsVersion,
		RequestSecretFingerprint:  types.StringValue(s.RequestSecretFingerprint),
		CallbackSecretFingerprint: types.StringValue(s.CallbackSecretFingerprint),
		SecretsChangedAt:          types.StringValue(s.SecretsChangedAt),
		PreviousSecretsUntil:      types.StringPointerValue(s.PreviousSecretsUntil),
		LockVersion:               types.Int64Value(s.LockVersion),
	}
}

func (r *senderResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_sender"
}

func (r *senderResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *senderResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A sender: a system that sends AutoPilot tasks, such as an issue tracker. It names the " +
			"hosts its callbacks may go to, the kinds of task it may send, the prefix its branches start with, and " +
			"its two secrets. The sender signs its calls to AutoPilot with the request secret, and AutoPilot signs " +
			"its callbacks to the sender with the callback secret.\n\n" +
			"An app must also accept the sender (see `autopilot_app`) before AutoPilot takes its tasks for that " +
			"app. The two can be applied in either order.\n\n" +
			"## The secrets\n\n" +
			"`request_secret_wo` and `callback_secret_wo` are [write-only arguments](https://developer.hashicorp.com/terraform/language/resources/ephemeral#write-only-arguments): " +
			"Terraform sends them on apply and never writes them to the plan or the state file. They need " +
			"**Terraform 1.11 or later**. Make them in the sender's own configuration with a plain `random_password` " +
			"(not an ephemeral value, which changes on every run), pass them here, and store them where the sender " +
			"reads its secrets.\n\n" +
			"This resource never stores the secrets, but the `random_password` resources that make them keep them in " +
			"that configuration's state, so protect that state as you would the secrets. And make them random: a " +
			"fingerprint is an unsalted SHA-256, quick to compute and shown in plans and state, so a secret someone " +
			"could guess could be found from its fingerprint.\n\n" +
			"AutoPilot never sends a secret back. It reports a fingerprint of each one instead, and the provider " +
			"works out the same fingerprints from the configuration. So:\n\n" +
			"- **Rotating.** Change a secret in configuration. Its fingerprint changes, so the provider plans an " +
			"update that sends both secrets. AutoPilot keeps the old pair for 24 hours (`previous_secrets_until`), " +
			"taking requests signed with either request secret and signing callbacks with both callback secrets, so " +
			"the sender can take up the new pair at its own pace.\n" +
			"- **Changed outside Terraform.** The next refresh reads a different fingerprint, and the next apply puts " +
			"the configured secrets back.\n" +
			"- **`secrets_wo_version`.** Change it to send the secrets whatever the fingerprints say. Sending the " +
			"secrets AutoPilot already holds changes nothing.\n" +
			"- **A secret that changes between plan and apply** (an ephemeral value, say) is not sent. Terraform " +
			"stops the apply with \"Provider produced inconsistent final plan\", naming the secret's fingerprint. Use " +
			"a stable value.\n\n" +
			"## Deleting\n\n" +
			"Deleting a sender retires it, and it reads as gone from then on. It can send no new tasks, and its " +
			"queued tasks are refused, while its running tasks run on and their events are still delivered. To stop " +
			"its running tasks too, as for a sender whose secrets have leaked, also take it out of the apps' " +
			"`accepts`.\n\n" +
			"A deleted sender's **name and branch prefix are never freed**, so no other system can take them over, " +
			"or whatever the apps accept from that name (once it owes nothing, AutoPilot keeps a tombstone of it). " +
			"Only the same sender can come back: a create with the same name, the same two secrets and the same " +
			"branch prefix revives it. So:\n\n" +
			"- **`terraform apply -replace`** deletes the sender and creates it again with the same secrets and " +
			"prefix, which brings it back at once, even while its work is still running.\n" +
			"- **Renaming** a sender gives it a new name, and the old one stays taken. Its old branch prefix stays " +
			"taken too, so a renamed sender that sets `branch_prefix` needs a new prefix (one left to its default, " +
			"the new name and a `/`, already has one).\n" +
			"- **`create_before_destroy`** doesn't fit a sender: the name is its key, so there can't be two at once.\n\n" +
			"## Import\n\n" +
			"Import by name: `terraform import autopilot_sender.tracker tracker`. The secrets can't be imported. Put " +
			"them in configuration: when their fingerprints match the stored ones the next plan is empty, and when " +
			"they don't, the next apply sends them. A `secrets_wo_version` in the configuration shows as a change on that " +
			"first plan too; applying it records the number and sends the secrets, which changes nothing when they match.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				MarkdownDescription: "The sender's name, which is its key and the `sender.system` of its tasks: a lowercase " +
					"letter, then lowercase letters, digits and `-`, at most 40 characters. AutoPilot reserves some names " +
					"(such as `autopilot`). Changing it replaces the sender; see Deleting, above.",
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators: []validator.String{stringvalidator.RegexMatches(senderNamePattern,
					"must be a lowercase letter, then lowercase letters, digits and -, at most 40 characters")},
			},
			"callback_hosts": schema.SetAttribute{
				MarkdownDescription: "Host names the sender's callbacks may go to (over https on port 443): lowercase DNS " +
					"names with at least one dot, not IP addresses and not `localhost`. At most 10. Empty (the default) " +
					"for a sender that only polls.",
				ElementType: types.StringType,
				Optional:    true,
				Computed:    true,
				Default:     setdefault.StaticValue(setFromStrings(nil)),
				Validators: []validator.Set{
					setvalidator.SizeAtMost(10),
					setvalidator.ValueStringsAre(callbackHostValidator{}),
				},
			},
			"kinds": schema.SetAttribute{
				MarkdownDescription: "The kinds of task the sender may send, such as `implement-work-item`, `fix-error`, " +
					"`review-pr` or `research`. Each must be one AutoPilot knows. At least one. Defaults to " +
					"`[\"implement-work-item\"]`, AutoPilot's own default, so removing it from the configuration puts " +
					"that back, and a change made outside Terraform shows in the plan.",
				ElementType: types.StringType,
				Optional:    true,
				Computed:    true,
				Default:     setdefault.StaticValue(setFromStrings([]string{"implement-work-item"})),
				Validators: []validator.Set{
					setvalidator.SizeAtLeast(1),
					setvalidator.ValueStringsAre(stringvalidator.RegexMatches(kindPattern, "must be a kind of task, such as implement-work-item")),
				},
			},
			"branch_prefix": schema.StringAttribute{
				MarkdownDescription: "What the sender's branches start with: a lowercase word and a `/` (`^[a-z][a-z0-9-]*/$`), " +
					"at most 41 characters. No two senders share one. Defaults to the name and a `/`, AutoPilot's own " +
					"default.\n\n" +
					"It is set when the sender is created and **never changes**. A deleted sender's name and prefix " +
					"stay taken for good, so a different prefix means a new sender with a new `name`: the plan refuses a " +
					"new prefix under the same name (keep it, or change `name` too, which replaces the sender). After an " +
					"import, set it to the sender's current prefix unless that is the default.",
				Optional: true,
				Computed: true,
				Validators: []validator.String{
					stringvalidator.RegexMatches(branchPrefixPattern, "must be a lowercase word and a /, such as tracker/"),
					stringvalidator.LengthAtMost(41),
				},
			},
			"request_secret_wo": schema.StringAttribute{
				MarkdownDescription: "The secret the sender signs its calls to AutoPilot with. At least 32 characters, " +
					"and different from `callback_secret_wo`. **Write-only**: sent on apply, never stored in the plan " +
					"or state.",
				Required:   true,
				WriteOnly:  true,
				Sensitive:  true,
				Validators: []validator.String{minCharactersValidator{min: 32}},
			},
			"callback_secret_wo": schema.StringAttribute{
				MarkdownDescription: "The secret AutoPilot signs its callbacks to the sender with. At least 32 " +
					"characters, and different from `request_secret_wo`. **Write-only**: sent on apply, never stored in " +
					"the plan or state.",
				Required:   true,
				WriteOnly:  true,
				Sensitive:  true,
				Validators: []validator.String{minCharactersValidator{min: 32}},
			},
			"secrets_wo_version": schema.Int64Attribute{
				MarkdownDescription: "A number you change to send both secrets again, whatever their fingerprints say. " +
					"Rarely needed, since a changed secret already shows as a changed fingerprint.",
				Optional: true,
			},
			"request_secret_fingerprint": schema.StringAttribute{
				MarkdownDescription: "Fingerprint of the stored request secret: the first 16 hex characters of the " +
					"SHA-256 of `autopilot-fingerprint:` followed by the secret.",
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"callback_secret_fingerprint": schema.StringAttribute{
				MarkdownDescription: "Fingerprint of the stored callback secret, worked out the same way.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"secrets_changed_at": schema.StringAttribute{
				MarkdownDescription: "When the secrets were last set (RFC 3339).",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"previous_secrets_until": schema.StringAttribute{
				MarkdownDescription: "While a rotation's overlap lasts, when it ends (RFC 3339): until then AutoPilot " +
					"still takes the old request secret and signs callbacks with both callback secrets. Null otherwise.",
				Computed: true,
			},
			"lock_version": schema.Int64Attribute{
				MarkdownDescription: "The record's version, which AutoPilot raises on every change. Sent as `If-Match` " +
					"on updates and deletes.",
				Computed: true,
			},
		},
	}
}

// ValidateConfig refuses two equal secrets, which AutoPilot would refuse at
// apply.
func (r *senderResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var request, callback types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("request_secret_wo"), &request)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("callback_secret_wo"), &callback)...)
	if resp.Diagnostics.HasError() || !known(request) || !known(callback) {
		return
	}
	if request.ValueString() == callback.ValueString() {
		resp.Diagnostics.AddAttributeError(path.Root("callback_secret_wo"), "The two secrets are the same",
			"AutoPilot needs the request secret and the callback secret to differ. Make each from its own random_password.")
	}
}

func known(v types.String) bool { return !v.IsNull() && !v.IsUnknown() }

// ModifyPlan plans the fingerprints of the configured secrets. When they
// differ from the stored ones, or a secret is not known yet, the update that
// follows is what sends the secrets, and what it changes is marked unknown.
func (r *senderResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroying
	}
	var config, plan senderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// branch_prefix left out of the configuration is AutoPilot's default,
	// which depends on the name, so it can't be a static Default.
	if config.BranchPrefix.IsNull() {
		if known(plan.Name) {
			plan.BranchPrefix = types.StringValue(plan.Name.ValueString() + "/")
		} else {
			plan.BranchPrefix = types.StringUnknown()
		}
	}
	secretsKnown := known(config.RequestSecretWO) && known(config.CallbackSecretWO)
	if secretsKnown {
		plan.RequestSecretFingerprint = types.StringValue(client.Fingerprint(config.RequestSecretWO.ValueString()))
		plan.CallbackSecretFingerprint = types.StringValue(client.Fingerprint(config.CallbackSecretWO.ValueString()))
	} else {
		plan.RequestSecretFingerprint = types.StringUnknown()
		plan.CallbackSecretFingerprint = types.StringUnknown()
	}
	if !req.State.Raw.IsNull() {
		var state senderModel
		resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
		if resp.Diagnostics.HasError() {
			return
		}
		// No value may ever serve both directions: AutoPilot refuses a new
		// secret that is the sender's current secret for the other one.
		// Fingerprints show it here, before anything is sent.
		if secretsKnown {
			if plan.RequestSecretFingerprint.Equal(state.CallbackSecretFingerprint) {
				resp.Diagnostics.AddAttributeError(path.Root("request_secret_wo"), "A secret can't serve both directions",
					"request_secret_wo is the sender's current callback secret (their fingerprints match). AutoPilot "+
						"never lets one value serve both directions. Make each secret from its own random_password.")
			}
			if plan.CallbackSecretFingerprint.Equal(state.RequestSecretFingerprint) {
				resp.Diagnostics.AddAttributeError(path.Root("callback_secret_wo"), "A secret can't serve both directions",
					"callback_secret_wo is the sender's current request secret (their fingerprints match). AutoPilot "+
						"never lets one value serve both directions. Make each secret from its own random_password.")
			}
			if resp.Diagnostics.HasError() {
				return
			}
		}
		// A branch prefix never changes. A new one means a new sender, which
		// needs a new name too: replacing a sender under its own name with
		// another prefix would retire it, and its name and prefix stay taken,
		// so the create that follows could only fail.
		if known(plan.BranchPrefix) && !plan.BranchPrefix.Equal(state.BranchPrefix) {
			if plan.Name.Equal(state.Name) {
				current := state.BranchPrefix.ValueString()
				resp.Diagnostics.AddAttributeError(path.Root("branch_prefix"), "A sender's branch prefix never changes",
					fmt.Sprintf("Sender %q has the branch prefix %q, and AutoPilot never changes a sender's prefix. A "+
						"deleted sender's name and prefix stay taken for good, so a new prefix means a new sender with a "+
						"new name. Set branch_prefix = %q to keep it (left out, it means the default, the name and a /), "+
						"or change name as well.", state.Name.ValueString(), current, current))
				return
			}
			resp.RequiresReplace = append(resp.RequiresReplace, path.Root("branch_prefix"))
		}
		rotating := !secretsKnown || !plan.RequestSecretFingerprint.Equal(state.RequestSecretFingerprint) ||
			!plan.CallbackSecretFingerprint.Equal(state.CallbackSecretFingerprint)
		// A rotation is a change the framework could not see when it planned.
		// Marking what it moves as unknown is what turns it into a planned
		// update; a computed attribute left at its prior value would make the
		// apply an inconsistent result.
		if rotating {
			plan.SecretsChangedAt = types.StringUnknown()
			plan.PreviousSecretsUntil = types.StringUnknown()
			plan.LockVersion = types.Int64Unknown()
		}
	}
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

func (r *senderResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan, config senderModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	// A write-only argument is null in the plan; the configuration is the only
	// place its value can be read.
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := plan.Name.ValueString()
	if !secretsMatchPlan(&resp.Diagnostics, plan, config) {
		return
	}
	// kinds and branch_prefix are always planned (their defaults are
	// AutoPilot's), so they are always sent, and what AutoPilot holds is what
	// the plan said.
	spec := client.SenderSpec{
		Secrets:      &client.SenderSecrets{Request: config.RequestSecretWO.ValueString(), Callback: config.CallbackSecretWO.ValueString()},
		Kinds:        stringsFromSet(ctx, plan.Kinds, "kinds", &resp.Diagnostics),
		BranchPrefix: plan.BranchPrefix.ValueStringPointer(),
	}
	// callback_hosts defaults to empty, which is AutoPilot's default too, so
	// it is sent only when it holds something.
	if hosts := stringsFromSet(ctx, plan.CallbackHosts, "callback_hosts", &resp.Diagnostics); len(hosts) > 0 {
		spec.CallbackHosts = hosts
	}
	if resp.Diagnostics.HasError() {
		return
	}

	created, outcome, err := r.client.CreateSender(ctx, name, spec)
	if err != nil {
		addSenderCreateError(&resp.Diagnostics, name, err)
		return
	}
	// An identical create matches the secrets too, and only a configuration
	// holding these very secrets can make that match: in practice this one,
	// after an earlier apply whose state was lost. So taking the sender over is
	// safe, and a warning is enough. (An app has no such proof; see
	// appResource.Create.)
	if outcome == client.Adopted {
		resp.Diagnostics.AddWarning("Sender already existed",
			fmt.Sprintf("AutoPilot already had a sender named %q with exactly these values, so Terraform now manages "+
				"that sender. If another configuration also declares it, keep it in one of them only.", name))
	}
	state := senderToModel(created, plan.SecretsWOVersion)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// secretsMatchPlan stops an apply whose secrets are not the ones the plan was
// made with, before anything is sent. A plan made with known secrets shows
// their fingerprints; were different secrets sent, AutoPilot would hold them
// and the apply would end in an inconsistent result. That happens when a
// secret comes from a value that changes on every run, such as an ephemeral
// resource.
func secretsMatchPlan(diags *diag.Diagnostics, plan, config senderModel) bool {
	ok := true
	for _, s := range []struct {
		attr    string
		planned types.String
		secret  types.String
	}{
		{"request_secret_wo", plan.RequestSecretFingerprint, config.RequestSecretWO},
		{"callback_secret_wo", plan.CallbackSecretFingerprint, config.CallbackSecretWO},
	} {
		if !known(s.planned) || !known(s.secret) {
			continue
		}
		if got := client.Fingerprint(s.secret.ValueString()); got != s.planned.ValueString() {
			diags.AddAttributeError(path.Root(s.attr), "The secret changed after the plan",
				fmt.Sprintf("The plan was made with a secret whose fingerprint is %s, but at apply the configuration "+
					"gives one whose fingerprint is %s. Nothing was sent to AutoPilot. This happens when a secret comes "+
					"from a value that changes on every run, such as an ephemeral resource. Use a stable value, such as "+
					"a random_password, then plan and apply again.", s.planned.ValueString(), got))
			ok = false
		}
	}
	return ok
}

func addSenderCreateError(diags *diag.Diagnostics, name string, err error) {
	summary := "Error creating AutoPilot sender"
	switch {
	case client.HasCode(err, client.CodeNameTaken):
		diags.AddAttributeError(path.Root("name"), "A sender with this name already exists",
			fmt.Sprintf("AutoPilot already has a sender named %q, with values that differ from this configuration. "+
				"To manage it here, import it (terraform import autopilot_sender.<resource name> %s) and apply. "+
				"Be careful if another configuration manages it: applying here sends this configuration's secrets, "+
				"which replaces that sender's secrets and breaks it until it takes up the new ones. Otherwise "+
				"choose another name.\n\nAutoPilot said: %s", name, name, apiMessage(err)))
	case client.HasCode(err, client.CodeNameRetired):
		diags.AddAttributeError(path.Root("name"), "This sender name is retired",
			fmt.Sprintf("A sender named %q was deleted, and AutoPilot keeps a deleted sender's name and branch prefix "+
				"for good. Only a create with the same two secrets and the same branch prefix brings it back (which "+
				"is what `terraform apply -replace` sends), and this one's differ. Use the old secrets and prefix, or "+
				"choose another name.\n\nAutoPilot said: %s", name, apiMessage(err)))
	case client.HasCode(err, client.CodeNameReserved):
		diags.AddAttributeError(path.Root("name"), "This sender name is reserved",
			fmt.Sprintf("AutoPilot keeps the name %q for itself. Choose another name.\n\nAutoPilot said: %s", name, apiMessage(err)))
	case client.HasCode(err, client.CodeBranchPrefixTaken):
		// AutoPilot points at /name when the prefix was the default, the
		// name's.
		at, which := path.Root("branch_prefix"), "This branch prefix"
		if e, ok := client.AsError(err); ok && e.Field == "/name" {
			at, which = path.Root("name"), "This sender's default branch prefix, its name and a /,"
		}
		diags.AddAttributeError(at, "Another sender has this branch prefix",
			which+" is taken by another sender, or was by a sender since deleted: AutoPilot never frees a "+
				"branch prefix, so one sender's task can never build on another's pull request. Set `branch_prefix` "+
				"to one of this sender's own.\n\nAutoPilot said: "+apiMessage(err))
	default:
		addAPIError(diags, summary, err, senderAPIFields)
	}
}

func (r *senderResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state senderModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	s, err := r.client.GetSender(ctx, state.Name.ValueString())
	if err != nil {
		if client.IsNotFound(err) {
			// Deleted, or retired, outside of Terraform.
			resp.State.RemoveResource(ctx)
			return
		}
		addAPIError(&resp.Diagnostics, "Error reading AutoPilot sender", err, senderAPIFields)
		return
	}
	newState := senderToModel(s, state.SecretsWOVersion)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Update sends only what changed. The secrets are sent when the configured
// ones' fingerprints differ from the stored ones, or secrets_wo_version
// changed.
func (r *senderResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state, config senderModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	if !secretsMatchPlan(&resp.Diagnostics, plan, config) {
		return
	}

	var spec client.SenderSpec
	if !plan.CallbackHosts.Equal(state.CallbackHosts) {
		spec.CallbackHosts = stringsFromSet(ctx, plan.CallbackHosts, "callback_hosts", &resp.Diagnostics)
	}
	if !plan.Kinds.IsUnknown() && !plan.Kinds.IsNull() && !plan.Kinds.Equal(state.Kinds) {
		spec.Kinds = stringsFromSet(ctx, plan.Kinds, "kinds", &resp.Diagnostics)
	}
	requestSecret, callbackSecret := config.RequestSecretWO.ValueString(), config.CallbackSecretWO.ValueString()
	if client.Fingerprint(requestSecret) != state.RequestSecretFingerprint.ValueString() ||
		client.Fingerprint(callbackSecret) != state.CallbackSecretFingerprint.ValueString() ||
		!plan.SecretsWOVersion.Equal(state.SecretsWOVersion) {
		spec.Secrets = &client.SenderSecrets{Request: requestSecret, Callback: callbackSecret}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	var updated *client.Sender
	var err error
	if spec.Empty() {
		// A secret that was unknown at plan time turned out to match: there is
		// nothing to send, only the record to read.
		updated, err = r.client.GetSender(ctx, name)
	} else {
		updated, err = r.client.UpdateSender(ctx, name, spec, state.LockVersion.ValueInt64())
	}
	if err != nil {
		if client.IsStale(err) {
			var current *int64
			if fresh, rerr := r.client.GetSender(ctx, name); rerr == nil {
				current = &fresh.LockVersion
			}
			addStaleError(&resp.Diagnostics, fmt.Sprintf("Sender %q", name), state.LockVersion.ValueInt64(), current, err)
			return
		}
		if client.HasCode(err, client.CodeBranchPrefixTaken) {
			addSenderCreateError(&resp.Diagnostics, name, err)
			return
		}
		addAPIError(&resp.Diagnostics, "Error updating AutoPilot sender", err, senderAPIFields)
		return
	}
	newState := senderToModel(updated, plan.SecretsWOVersion)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *senderResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state senderModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	err := deleteWithIfMatch(ctx, state.LockVersion.ValueInt64(),
		func(ctx context.Context, lv int64) error { return r.client.DeleteSender(ctx, name, lv) },
		func(ctx context.Context) (int64, error) {
			fresh, err := r.client.GetSender(ctx, name)
			if err != nil {
				return 0, err
			}
			return fresh.LockVersion, nil
		})
	if err != nil {
		if client.IsStale(err) {
			addStaleError(&resp.Diagnostics, fmt.Sprintf("Sender %q", name), state.LockVersion.ValueInt64(), nil, err)
			return
		}
		addAPIError(&resp.Diagnostics, "Error deleting AutoPilot sender", err, senderAPIFields)
	}
}

// ImportState imports by name. The secrets can't be read back, so the
// configuration supplies them, and the next plan compares fingerprints.
func (r *senderResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	name := req.ID
	if !senderNamePattern.MatchString(name) {
		resp.Diagnostics.AddError("Invalid import id",
			fmt.Sprintf("Import a sender by its name (for example `tracker`), got %q.", name))
		return
	}
	s, err := r.client.GetSender(ctx, name)
	if err != nil {
		if client.IsNotFound(err) {
			resp.Diagnostics.AddError("No such sender",
				fmt.Sprintf("AutoPilot has no sender named %q (a retired sender reads as missing too).", name))
			return
		}
		addAPIError(&resp.Diagnostics, "Error importing AutoPilot sender", err, senderAPIFields)
		return
	}
	state := senderToModel(s, types.Int64Null())
	resp.Diagnostics.AddWarning("Imported sender has no secrets in Terraform",
		"AutoPilot never sends a sender's secrets back, and they are write-only here, so they are not in state. "+
			"Put them in configuration: the provider compares their fingerprints with the stored ones, so matching "+
			"secrets plan nothing and different ones are sent on the next apply.")
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
