package provider

import (
	"context"
	"os"
	"strings"

	"github.com/CruGlobal/terraform-provider-autopilot/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Environment variables the provider reads when the provider block sets
// neither endpoint nor token. It never takes one from the block and the
// other from the environment: see endpointAndToken.
const (
	envEndpoint = "AUTOPILOT_ENDPOINT"
	envToken    = "AUTOPILOT_TOKEN"
)

// fromBlock names the provider block as where a value came from, in an
// error message.
const fromBlock = "the provider block"

// oneSource is the rule the provider holds the endpoint and the token to,
// said the same way in every error that refuses a configuration for it.
const oneSource = "The provider takes the endpoint and the token from one place, so that a token meant for one " +
	"AutoPilot is never sent to another: either both from the provider block, or both from the " +
	envEndpoint + " and " + envToken + " environment variables (as a runner such as Atlantis sets them) " +
	"with neither in the block."

var _ provider.Provider = &autopilotProvider{}

type autopilotProvider struct {
	// version is the provider version on release, "dev" for a local build, and
	// "test" under the test harness.
	version string
}

type autopilotProviderModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	Token    types.String `tfsdk:"token"`
}

// New returns the provider constructor the plugin server and the test harness
// both use.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &autopilotProvider{version: version}
	}
}

func (p *autopilotProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "autopilot"
	resp.Version = p.version
}

func (p *autopilotProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The `autopilot` provider manages the records that say who may send work to an " +
			"AutoPilot, and what each app allows, through AutoPilot's admin API. AutoPilot runs AI agents on " +
			"work that senders such as issue trackers send it, and opens pull requests a person reviews.\n\n" +
			"- A **sender** (`autopilot_sender`) is a system that sends tasks: the hosts its callbacks may go to, " +
			"the kinds of task it may send, the prefix its branches start with, and its two secrets.\n" +
			"- An **app** (`autopilot_app`) is a unit of consent: the repositories it owns, the senders it accepts " +
			"work from and the kinds it accepts from each, and the people who may start work for it themselves, " +
			"with their GitHub logins.\n\n" +
			"## Authentication\n\n" +
			"The provider authenticates with an AutoPilot **admin token**, sent as a bearer token. AutoPilot keeps " +
			"only the token's SHA-256, so it cannot show you the token; whoever set it up holds it. To keep it out " +
			"of configuration, set both `AUTOPILOT_ENDPOINT` and `AUTOPILOT_TOKEN` in the environment and leave " +
			"both out of the provider block.\n\n" +
			"The provider takes the endpoint and the token from one place, so that a token meant for one " +
			"AutoPilot is never sent to another:\n\n" +
			"- **Both from the environment:** leave `endpoint` and `token` out of the provider block (or leave " +
			"the block out), and set both `AUTOPILOT_ENDPOINT` and `AUTOPILOT_TOKEN`.\n" +
			"- **Both from the provider block:** set both `endpoint` and `token`. The environment variables are " +
			"then not read.\n\n" +
			"Anything else fails when the provider is configured, before any request is sent: a provider block " +
			"that sets one of the two but not the other (even when the environment has the other), or, with " +
			"neither in the block, an environment that has only one of them. A value that is empty or only " +
			"spaces counts as missing, in the block and in the environment alike, and one in the block is " +
			"never filled in from the environment. A value in the block that is unknown when the provider is " +
			"configured (one that comes from a resource not yet applied) counts as set, and is refused too: " +
			"apply its source first with `-target`, or set the value statically. No error ever shows the " +
			"token, or the endpoint, in case a token was put there by mistake.\n\n" +
			"## Terraform version\n\n" +
			"`autopilot_sender` takes its secrets as write-only arguments, which need **Terraform 1.11 or later**.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				MarkdownDescription: "Base URL of the AutoPilot, for example `https://autopilot.example.com`. " +
					"The `/v1/admin` path is appended automatically; a URL with a route after `/v1/admin` is refused. " +
					"It must be https; plain `http://` is allowed only for a loopback host (`localhost`, `127.0.0.1`, " +
					"`::1`), because the admin token travels in every request. Set it together with `token`, or " +
					"leave both out to use the `AUTOPILOT_ENDPOINT` and `AUTOPILOT_TOKEN` environment variables.",
				Optional: true,
			},
			"token": schema.StringAttribute{
				MarkdownDescription: "Admin token, sent as the bearer token on every request. Set it together with " +
					"`endpoint`, or leave both out to use the `AUTOPILOT_ENDPOINT` and `AUTOPILOT_TOKEN` environment " +
					"variables.",
				Optional:  true,
				Sensitive: true,
			},
		},
	}
}

func (p *autopilotProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg autopilotProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	endpoint, endpointFrom, token, tokenFrom := endpointAndToken(cfg, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := client.ValidateEndpoint(endpoint); err != nil {
		resp.Diagnostics.AddAttributeError(
			path.Root("endpoint"),
			"Invalid AutoPilot endpoint",
			"The endpoint (from "+endpointFrom+") must be the AutoPilot's base URL, an https URL with a host, "+
				"such as https://autopilot.example.com: "+err.Error()+". The value is not shown, in case it is a "+
				"token put there by mistake (say, with the endpoint and the token swapped).",
		)
	}
	if err := client.ValidateToken(token); err != nil {
		// The error never repeats the token.
		resp.Diagnostics.AddAttributeError(
			path.Root("token"),
			"Invalid AutoPilot token",
			"The token (from "+tokenFrom+") is not usable: "+err.Error()+". An admin token is a single word; "+
				"check that nothing else was pasted with it.",
		)
	}
	if resp.Diagnostics.HasError() {
		return
	}

	c, err := client.New(endpoint, token, client.WithUserAgent(client.DefaultUserAgent+"/"+p.version))
	if err != nil {
		resp.Diagnostics.AddError("Could not create the AutoPilot API client", err.Error())
		return
	}

	resp.DataSourceData = c
	resp.ResourceData = c
}

func (p *autopilotProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewSenderResource,
		NewAppResource,
	}
}

func (p *autopilotProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return nil
}

// endpointAndToken finds the endpoint and the token, and says where each came
// from, for an error message. Both come from one place. When the provider
// block sets either of them, both come from the block and the environment is
// not read; otherwise both come from the environment. That way a block aimed
// at one AutoPilot can never pick up another's token from the environment,
// as it would if it set only endpoint where a runner has both variables set.
//
// An attribute counts as set in the block whenever it isn't null: an unknown
// value or a blank one is set, and is refused rather than filled in from the
// environment. A blank value (empty, or only spaces) is missing wherever it
// is. Every refusal names what is missing and where it can come from. None
// repeats the token, or says more of an environment variable than whether it
// is set.
func endpointAndToken(cfg autopilotProviderModel, diags *diag.Diagnostics) (endpoint, endpointFrom, token, tokenFrom string) {
	endpointInBlock, tokenInBlock := !cfg.Endpoint.IsNull(), !cfg.Token.IsNull()

	if cfg.Endpoint.IsUnknown() {
		diags.AddAttributeError(
			path.Root("endpoint"),
			"Unknown AutoPilot endpoint",
			"The provider cannot create the AutoPilot API client because `endpoint` is unknown at configure time. "+
				"Either target-apply the source of the value first, set it statically, or take both `endpoint` and "+
				"`token` out of the provider block to use the "+envEndpoint+" and "+envToken+" environment variables.",
		)
	}
	if cfg.Token.IsUnknown() {
		diags.AddAttributeError(
			path.Root("token"),
			"Unknown AutoPilot token",
			"The provider cannot create the AutoPilot API client because `token` is unknown at configure time. "+
				"Either target-apply the source of the value first, set it statically, or take both `endpoint` and "+
				"`token` out of the provider block to use the "+envEndpoint+" and "+envToken+" environment variables.",
		)
	}
	switch {
	case endpointInBlock && !tokenInBlock:
		diags.AddAttributeError(path.Root("token"), incompleteBlock, halfBlock("endpoint", "token", envToken))
	case tokenInBlock && !endpointInBlock:
		diags.AddAttributeError(path.Root("endpoint"), incompleteBlock, halfBlock("token", "endpoint", envEndpoint))
	}
	if diags.HasError() {
		return "", "", "", ""
	}

	if endpointInBlock {
		// The token is in the block too: a block with only one of them was
		// refused above.
		endpoint, token = cfg.Endpoint.ValueString(), cfg.Token.ValueString()
		if blank(endpoint) {
			diags.AddAttributeError(path.Root("endpoint"), "Missing AutoPilot endpoint", emptyInBlock("endpoint",
				"the base URL of the AutoPilot to manage, for example https://autopilot.example.com"))
		}
		if blank(token) {
			diags.AddAttributeError(path.Root("token"), "Missing AutoPilot token", emptyInBlock("token",
				"an AutoPilot admin token"))
		}
		return endpoint, fromBlock, token, fromBlock
	}

	endpoint, token = os.Getenv(envEndpoint), os.Getenv(envToken)
	switch {
	case blank(endpoint) && blank(token):
		diags.AddAttributeError(path.Root("endpoint"), "Missing AutoPilot endpoint", neitherSet+
			" The endpoint is the base URL of the AutoPilot to manage, for example https://autopilot.example.com.")
		diags.AddAttributeError(path.Root("token"), "Missing AutoPilot token", neitherSet+
			" The token is an AutoPilot admin token.")
	case blank(endpoint):
		diags.AddAttributeError(path.Root("endpoint"), "Missing AutoPilot endpoint", halfEnvironment(envToken, envEndpoint))
	case blank(token):
		diags.AddAttributeError(path.Root("token"), "Missing AutoPilot token", halfEnvironment(envEndpoint, envToken))
	}
	return endpoint, "the " + envEndpoint + " environment variable", token, "the " + envToken + " environment variable"
}

// incompleteBlock is the summary for a provider block that sets one of
// endpoint and token but not the other.
const incompleteBlock = "Incomplete AutoPilot provider block"

// neitherSet explains a provider that has neither the endpoint nor the token
// from anywhere.
const neitherSet = "The provider block sets neither `endpoint` nor `token`, and neither " + envEndpoint + " nor " +
	envToken + " is set in the environment (or each is blank). Set both `endpoint` and `token` in the provider " +
	"block, or set both environment variables."

// blank reports whether a value is missing: empty, or only spaces.
func blank(s string) bool { return strings.TrimSpace(s) == "" }

// halfBlock explains a provider block that sets one of endpoint and token but
// not the other. It says whether the missing one's environment variable is
// set, since that is the value the provider declined to use, but never what
// it holds.
func halfBlock(set, missing, missingEnv string) string {
	detail := "The provider block sets `" + set + "` but not `" + missing + "`. " + oneSource +
		" Set `" + missing + "` in the provider block too, or take `" + set + "` out of it."
	if !blank(os.Getenv(missingEnv)) {
		detail += " " + missingEnv + " is set in this environment, but it is never used together with a " +
			"provider block that sets `" + set + "`."
	}
	return detail
}

// halfEnvironment explains an environment that has one of the two variables
// but not the other, when the provider block sets neither attribute.
func halfEnvironment(setEnv, missingEnv string) string {
	return "The provider block sets neither `endpoint` nor `token`, so both come from the environment, where " +
		setEnv + " is set but " + missingEnv + " is not (or is blank). " + oneSource +
		" Set " + missingEnv + " too, or set both `endpoint` and `token` in the provider block."
}

// emptyInBlock explains an attribute the provider block sets to a blank
// value.
func emptyInBlock(attr, want string) string {
	return "The provider block sets `" + attr + "` to an empty value (or only spaces). Set it to " + want + ", or take both " +
		"`endpoint` and `token` out of the block to use the " + envEndpoint + " and " + envToken +
		" environment variables. A blank value in the block is never filled in from the environment."
}
