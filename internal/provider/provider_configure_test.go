package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-autopilot/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// configure drives Configure directly with the given attribute values (nil
// means null, unknown means unknown) so the environment fallback and the
// validation can be tested without a resource to hang a configuration on.
func configure(t *testing.T, endpoint, token *string) provider.ConfigureResponse {
	t.Helper()
	ctx := context.Background()
	p := New("test")()
	var schemaResp provider.SchemaResponse
	p.Schema(ctx, provider.SchemaRequest{}, &schemaResp)

	str := func(v *string) tftypes.Value {
		switch {
		case v == nil:
			return tftypes.NewValue(tftypes.String, nil)
		case *v == unknown:
			return tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
		}
		return tftypes.NewValue(tftypes.String, *v)
	}
	raw := tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), map[string]tftypes.Value{
		"endpoint": str(endpoint),
		"token":    str(token),
	})
	var resp provider.ConfigureResponse
	p.Configure(ctx, provider.ConfigureRequest{Config: tfsdk.Config{Raw: raw, Schema: schemaResp.Schema}}, &resp)
	return resp
}

const unknown = "\x00unknown"

func ptr(s string) *string { return &s }

func summaries(resp provider.ConfigureResponse) string {
	var out []string
	for _, d := range resp.Diagnostics.Errors() {
		out = append(out, d.Summary())
	}
	return strings.Join(out, "; ")
}

func TestConfigure_MissingValuesAreAttributeErrors(t *testing.T) {
	t.Setenv(envEndpoint, "")
	t.Setenv(envToken, "")
	resp := configure(t, nil, nil)
	got := summaries(resp)
	if !strings.Contains(got, "Missing AutoPilot endpoint") || !strings.Contains(got, "Missing AutoPilot token") {
		t.Errorf("diagnostics = %q", got)
	}
}

func TestConfigure_EnvironmentFallback(t *testing.T) {
	t.Setenv(envEndpoint, "https://autopilot.example.com")
	t.Setenv(envToken, "admin-token-from-env")
	resp := configure(t, nil, nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	c, ok := resp.ResourceData.(*client.Client)
	if !ok || c == nil {
		t.Fatalf("ResourceData = %T, want *client.Client", resp.ResourceData)
	}
	if got := c.BaseURL(); got != "https://autopilot.example.com/v1/admin" {
		t.Errorf("BaseURL = %q", got)
	}
}

func TestConfigure_ConfigWinsOverEnvironment(t *testing.T) {
	t.Setenv(envEndpoint, "https://env.example.com")
	t.Setenv(envToken, "admin-token-from-env")
	resp := configure(t, ptr("https://config.example.com/"), ptr("admin-token-from-config"))
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	c, ok := resp.ResourceData.(*client.Client)
	if !ok {
		t.Fatalf("ResourceData = %T, want *client.Client", resp.ResourceData)
	}
	if got := c.BaseURL(); got != "https://config.example.com/v1/admin" {
		t.Errorf("BaseURL = %q, want the configured endpoint", got)
	}
}

func TestConfigure_InvalidEndpoint(t *testing.T) {
	t.Setenv(envEndpoint, "")
	t.Setenv(envToken, "")
	for _, endpoint := range []string{"autopilot.example.com", "ftp://autopilot.example.com", "https://",
		"https://user:pw@autopilot.example.com", "http://autopilot.example.com",
		"https://autopilot.example.com/v1/admin/senders"} {
		resp := configure(t, ptr(endpoint), ptr("admin-token"))
		if got := summaries(resp); got != "Invalid AutoPilot endpoint" {
			t.Errorf("endpoint %q: diagnostics = %q", endpoint, got)
		}
	}
}

func TestConfigure_InvalidEndpointFromEnvironmentSaysSo(t *testing.T) {
	t.Setenv(envEndpoint, "autopilot.example.com")
	t.Setenv(envToken, "admin-token")
	resp := configure(t, nil, nil)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error")
	}
	if detail := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(detail, envEndpoint) {
		t.Errorf("detail %q does not say the value came from %s", detail, envEndpoint)
	}
}

func TestConfigure_InvalidTokenIsRefusedWithoutRepeatingIt(t *testing.T) {
	t.Setenv(envEndpoint, "")
	t.Setenv(envToken, "")
	resp := configure(t, ptr("https://autopilot.example.com"), ptr("two words"))
	if got := summaries(resp); got != "Invalid AutoPilot token" {
		t.Fatalf("diagnostics = %q", got)
	}
	if detail := resp.Diagnostics.Errors()[0].Detail(); strings.Contains(detail, "two words") {
		t.Errorf("the error repeats the token: %q", detail)
	}
}

func TestConfigure_TokenWithTrailingNewlineIsAccepted(t *testing.T) {
	t.Setenv(envEndpoint, "")
	t.Setenv(envToken, "admin-token-pasted\n")
	resp := configure(t, ptr("https://autopilot.example.com"), nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
}

func TestConfigure_UnknownValues(t *testing.T) {
	resp := configure(t, ptr(unknown), ptr(unknown))
	got := summaries(resp)
	if !strings.Contains(got, "Unknown AutoPilot endpoint") || !strings.Contains(got, "Unknown AutoPilot token") {
		t.Errorf("diagnostics = %q", got)
	}
}

// Plain http carries the admin token in the clear, so it is only for a server
// on this machine.
func TestConfigure_PlainHTTPOnlyForLoopback(t *testing.T) {
	t.Setenv(envEndpoint, "")
	t.Setenv(envToken, "")
	for _, endpoint := range []string{"http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		if resp := configure(t, ptr(endpoint), ptr("admin-token")); resp.Diagnostics.HasError() {
			t.Errorf("%s: %v", endpoint, resp.Diagnostics)
		}
	}
	resp := configure(t, ptr("http://autopilot.example.com"), ptr("admin-token"))
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "loopback") {
		t.Errorf("plain http to another host: %v", resp.Diagnostics)
	}
}
