package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// withTestRandom is the provider plus a test-only ephemeral resource,
// autopilot_test_random, whose value is new every time Terraform opens it:
// once at plan and again at apply. It stands in for a secret that changes
// between the two. The released provider has no such resource.
type withTestRandom struct{ provider.Provider }

var _ provider.ProviderWithEphemeralResources = withTestRandom{}

func (withTestRandom) EphemeralResources(context.Context) []func() ephemeral.EphemeralResource {
	return []func() ephemeral.EphemeralResource{func() ephemeral.EphemeralResource { return testRandom{} }}
}

type testRandom struct{}

func (testRandom) Metadata(_ context.Context, req ephemeral.MetadataRequest, resp *ephemeral.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_test_random"
}

func (testRandom) Schema(_ context.Context, _ ephemeral.SchemaRequest, resp *ephemeral.SchemaResponse) {
	resp.Schema = schema.Schema{Attributes: map[string]schema.Attribute{
		"value": schema.StringAttribute{Computed: true, Sensitive: true},
	}}
}

func (testRandom) Open(ctx context.Context, _ ephemeral.OpenRequest, resp *ephemeral.OpenResponse) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		resp.Diagnostics.AddError("random", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.Result.SetAttribute(ctx, path.Root("value"), types.StringValue(hex.EncodeToString(b[:])))...)
}
