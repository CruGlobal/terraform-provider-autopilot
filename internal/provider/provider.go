package provider

import (
	"context"
	"os"

	"github.com/CruGlobal/terraform-provider-autopilot/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Environment variables the provider falls back to when the corresponding
// provider-block attribute is unset.
const (
	envEndpoint = "AUTOPILOT_ENDPOINT"
	envToken    = "AUTOPILOT_TOKEN"
)

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
			"work from and the kinds it accepts from each, and the people who may start work for it themselves.\n\n" +
			"## Authentication\n\n" +
			"The provider authenticates with an AutoPilot **admin token**, sent as a bearer token. AutoPilot keeps " +
			"only the token's SHA-256, so it cannot show you the token; whoever set it up holds it. Keep it in the " +
			"`AUTOPILOT_TOKEN` environment variable rather than in configuration.\n\n" +
			"Both attributes fall back to environment variables (`AUTOPILOT_ENDPOINT`, `AUTOPILOT_TOKEN`).\n\n" +
			"## Terraform version\n\n" +
			"`autopilot_sender` takes its secrets as write-only arguments, which need **Terraform 1.11 or later**.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				MarkdownDescription: "Base URL of the AutoPilot, for example `https://autopilot.example.com`. " +
					"The `/v1/admin` path is appended automatically. Falls back to the `AUTOPILOT_ENDPOINT` " +
					"environment variable.",
				Optional: true,
			},
			"token": schema.StringAttribute{
				MarkdownDescription: "Admin token, sent as the bearer token on every request. Falls back to the " +
					"`AUTOPILOT_TOKEN` environment variable.",
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

	if cfg.Endpoint.IsUnknown() {
		resp.Diagnostics.AddAttributeError(
			path.Root("endpoint"),
			"Unknown AutoPilot endpoint",
			"The provider cannot create the AutoPilot API client because `endpoint` is unknown at configure time. "+
				"Either target-apply the source of the value first, set it statically, or use the "+envEndpoint+" environment variable.",
		)
	}
	if cfg.Token.IsUnknown() {
		resp.Diagnostics.AddAttributeError(
			path.Root("token"),
			"Unknown AutoPilot token",
			"The provider cannot create the AutoPilot API client because `token` is unknown at configure time. "+
				"Either target-apply the source of the value first, set it statically, or use the "+envToken+" environment variable.",
		)
	}
	if resp.Diagnostics.HasError() {
		return
	}

	endpoint, endpointFrom := stringValueOrEnv(cfg.Endpoint, envEndpoint)
	token, tokenFrom := stringValueOrEnv(cfg.Token, envToken)

	if endpoint == "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("endpoint"),
			"Missing AutoPilot endpoint",
			"Set `endpoint` in the provider configuration or the "+envEndpoint+" environment variable to the base URL "+
				"of the AutoPilot to manage (for example https://autopilot.example.com).",
		)
	} else if err := client.ValidateEndpoint(endpoint); err != nil {
		resp.Diagnostics.AddAttributeError(
			path.Root("endpoint"),
			"Invalid AutoPilot endpoint",
			"The endpoint (from "+endpointFrom+") must be an http or https URL with a host, such as "+
				"https://autopilot.example.com: "+err.Error(),
		)
	}
	if token == "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("token"),
			"Missing AutoPilot token",
			"Set `token` in the provider configuration or the "+envToken+" environment variable to an AutoPilot "+
				"admin token.",
		)
	} else if err := client.ValidateToken(token); err != nil {
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
	}
}

func (p *autopilotProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return nil
}

// stringValueOrEnv resolves the config-then-environment fallback: a non-empty
// configured value wins, otherwise the first non-empty environment variable.
// It also says where the value came from, for an error message.
func stringValueOrEnv(v types.String, envVars ...string) (value, from string) {
	if !v.IsNull() && !v.IsUnknown() && v.ValueString() != "" {
		return v.ValueString(), "the provider configuration"
	}
	for _, name := range envVars {
		if got := os.Getenv(name); got != "" {
			return got, "the " + name + " environment variable"
		}
	}
	return "", ""
}
