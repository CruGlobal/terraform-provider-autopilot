package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-autopilot/internal/autopilottest"
	"github.com/CruGlobal/terraform-provider-autopilot/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// configure drives Configure directly with the given attribute values (nil
// means null, unknown means unknown) so where the endpoint and token come
// from, and their validation, can be tested without a resource to hang a
// configuration on.
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

func TestConfigure_BothFromEnvironment(t *testing.T) {
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
	t.Setenv(envEndpoint, "https://autopilot.example.com")
	t.Setenv(envToken, "admin-token-pasted\n")
	resp := configure(t, nil, nil)
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

// The endpoint and the token come from one place: both from the provider
// block, or both from the environment with neither in the block. Anything
// else is refused, so that a block aimed at one AutoPilot never picks up
// another's token from the environment, where a runner may hold it.
func TestConfigure_EndpointAndTokenComeFromOnePlace(t *testing.T) {
	const (
		blockEndpoint = "https://block.example.com"
		envURL        = "https://env.example.com"
	)
	blockToken, envSecret := randSecret(t), randSecret(t)
	for _, tc := range []struct {
		name                  string
		endpoint, token       *string // in the provider block: nil leaves it out
		envEndpoint, envToken string  // "" leaves the variable unset
		// want is the errors' summaries, or "" when the provider is
		// configured, against wantBaseURL.
		want        string
		wantBaseURL string
		// wantDetail holds words some error's detail must have, and
		// notDetail words none may have.
		wantDetail, notDetail []string
	}{
		{
			name:     "both from the block",
			endpoint: ptr(blockEndpoint), token: ptr(blockToken),
			wantBaseURL: blockEndpoint + "/v1/admin",
		},
		{
			name:     "both from the block, with both in the environment too",
			endpoint: ptr(blockEndpoint), token: ptr(blockToken),
			envEndpoint: envURL, envToken: envSecret,
			wantBaseURL: blockEndpoint + "/v1/admin",
		},
		{
			name:        "both from the environment",
			envEndpoint: envURL, envToken: envSecret,
			wantBaseURL: envURL + "/v1/admin",
		},
		{
			name:     "endpoint in the block, token in the environment",
			endpoint: ptr(blockEndpoint), envToken: envSecret,
			want:       incompleteBlock,
			wantDetail: []string{"sets `endpoint` but not `token`", envToken + " is set in this environment, but it is never used"},
		},
		{
			// A runner that holds another AutoPilot's endpoint and token, and
			// a configuration that sets only its own endpoint.
			name:     "endpoint in the block, both in the environment",
			endpoint: ptr(blockEndpoint), envEndpoint: envURL, envToken: envSecret,
			want:       incompleteBlock,
			wantDetail: []string{"sets `endpoint` but not `token`", envToken + " is set in this environment, but it is never used"},
		},
		{
			name:       "endpoint in the block, nothing in the environment",
			endpoint:   ptr(blockEndpoint),
			want:       incompleteBlock,
			wantDetail: []string{"sets `endpoint` but not `token`"},
			notDetail:  []string{"is set in this environment"},
		},
		{
			name:  "token in the block, endpoint in the environment",
			token: ptr(blockToken), envEndpoint: envURL,
			want:       incompleteBlock,
			wantDetail: []string{"sets `token` but not `endpoint`", envEndpoint + " is set in this environment, but it is never used"},
		},
		{
			name:  "token in the block, both in the environment",
			token: ptr(blockToken), envEndpoint: envURL, envToken: envSecret,
			want:       incompleteBlock,
			wantDetail: []string{"sets `token` but not `endpoint`"},
		},
		{
			name:       "token in the block, nothing in the environment",
			token:      ptr(blockToken),
			want:       incompleteBlock,
			wantDetail: []string{"sets `token` but not `endpoint`"},
			notDetail:  []string{"is set in this environment"},
		},
		{
			name:        "only the endpoint in the environment",
			envEndpoint: envURL,
			want:        "Missing AutoPilot token",
			wantDetail:  []string{envEndpoint + " is set but " + envToken + " is not"},
		},
		{
			name:       "only the token in the environment",
			envToken:   envSecret,
			want:       "Missing AutoPilot endpoint",
			wantDetail: []string{envToken + " is set but " + envEndpoint + " is not"},
		},
		{
			name:       "nothing anywhere",
			want:       "Missing AutoPilot endpoint; Missing AutoPilot token",
			wantDetail: []string{"neither " + envEndpoint + " nor " + envToken + " is set"},
		},
		{
			name:     "an empty token in the block, with a token in the environment",
			endpoint: ptr(blockEndpoint), token: ptr(""), envToken: envSecret,
			want:       "Missing AutoPilot token",
			wantDetail: []string{"sets `token` to an empty value", "never filled in from the environment"},
		},
		{
			name:     "an empty endpoint in the block, with an endpoint in the environment",
			endpoint: ptr(""), token: ptr(blockToken), envEndpoint: envURL,
			want:       "Missing AutoPilot endpoint",
			wantDetail: []string{"sets `endpoint` to an empty value"},
		},
		{
			name:     "both empty in the block, with both in the environment",
			endpoint: ptr(""), token: ptr(""), envEndpoint: envURL, envToken: envSecret,
			want: "Missing AutoPilot endpoint; Missing AutoPilot token",
		},
		{
			name:     "an unknown endpoint in the block, with a token in the environment",
			endpoint: ptr(unknown), envToken: envSecret,
			want:       "Unknown AutoPilot endpoint; " + incompleteBlock,
			wantDetail: []string{"sets `endpoint` but not `token`"},
		},
		{
			name:     "a token of only spaces in the block, with a token in the environment",
			endpoint: ptr(blockEndpoint), token: ptr("  \n"), envToken: envSecret,
			want:       "Missing AutoPilot token",
			wantDetail: []string{"sets `token` to an empty value (or only spaces)", "never filled in from the environment"},
		},
		{
			name:     "an endpoint of only spaces in the block, with an endpoint in the environment",
			endpoint: ptr(" "), token: ptr(blockToken), envEndpoint: envURL,
			want:       "Missing AutoPilot endpoint",
			wantDetail: []string{"sets `endpoint` to an empty value (or only spaces)"},
		},
		{
			name:        "a token of only spaces in the environment",
			envEndpoint: envURL, envToken: " \t",
			want:       "Missing AutoPilot token",
			wantDetail: []string{envEndpoint + " is set but " + envToken + " is not (or is blank)"},
		},
		{
			name:        "an endpoint of only spaces in the environment",
			envEndpoint: "\n", envToken: envSecret,
			want:       "Missing AutoPilot endpoint",
			wantDetail: []string{envToken + " is set but " + envEndpoint + " is not (or is blank)"},
		},
		{
			name:        "both of only spaces in the environment",
			envEndpoint: " ", envToken: " ",
			want: "Missing AutoPilot endpoint; Missing AutoPilot token",
		},
		{
			// A blank variable is not "set", so the error doesn't say it was
			// passed over.
			name:     "endpoint in the block, a token of only spaces in the environment",
			endpoint: ptr(blockEndpoint), envToken: "  ",
			want:      incompleteBlock,
			notDetail: []string{"is set in this environment"},
		},
		{
			// The two values swapped: the token, where the endpoint should be,
			// is never shown.
			name:        "the endpoint and the token swapped in the environment",
			envEndpoint: envSecret, envToken: envURL,
			want:       "Invalid AutoPilot endpoint",
			wantDetail: []string{"from the " + envEndpoint + " environment variable", "The value is not shown"},
		},
		{
			name:     "the endpoint and the token swapped in the block",
			endpoint: ptr(blockToken), token: ptr(blockEndpoint),
			want:       "Invalid AutoPilot endpoint",
			wantDetail: []string{"from the provider block", "The value is not shown"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envEndpoint, tc.envEndpoint)
			t.Setenv(envToken, tc.envToken)
			resp := configure(t, tc.endpoint, tc.token)
			if got := summaries(resp); got != tc.want {
				t.Fatalf("diagnostics = %q, want %q (%v)", got, tc.want, resp.Diagnostics)
			}
			if tc.want == "" {
				c, ok := resp.ResourceData.(*client.Client)
				if !ok || c == nil {
					t.Fatalf("ResourceData = %T, want *client.Client", resp.ResourceData)
				}
				if got := c.BaseURL(); got != tc.wantBaseURL {
					t.Errorf("BaseURL = %q, want %q", got, tc.wantBaseURL)
				}
				return
			}
			if resp.ResourceData != nil || resp.DataSourceData != nil {
				t.Errorf("a refused configuration still made a client")
			}
			var details []string
			for _, d := range resp.Diagnostics {
				text := d.Summary() + "\n" + d.Detail()
				// Never the token, from wherever it came.
				if strings.Contains(text, blockToken) || strings.Contains(text, envSecret) {
					t.Errorf("a diagnostic repeats a token: %q", text)
				}
				// Each error about where the values come from says where
				// they can come from.
				for _, where := range []string{"provider block", envEndpoint, envToken} {
					if !strings.HasPrefix(d.Summary(), "Invalid") && !strings.Contains(d.Detail(), where) {
						t.Errorf("%q does not mention %s: %q", d.Summary(), where, d.Detail())
					}
				}
				details = append(details, d.Detail())
			}
			all := strings.Join(details, "\n")
			for _, w := range tc.wantDetail {
				if !strings.Contains(all, w) {
					t.Errorf("no detail says %q: %q", w, all)
				}
			}
			for _, w := range tc.notDetail {
				if strings.Contains(all, w) {
					t.Errorf("a detail says %q: %q", w, all)
				}
			}
		})
	}
}

// Through Terraform: a runner holds one AutoPilot's endpoint and token in its
// environment, and a configuration aimed at another sets only its endpoint.
// The plan fails before any request is sent, so the first AutoPilot's token
// never reaches the second.
func TestProvider_blockWithOnlyEndpointNeverTakesTheEnvironmentsToken(t *testing.T) {
	runner, elsewhere := autopilottest.New(t), autopilottest.New(t)
	t.Setenv(envEndpoint, runner.URL)
	t.Setenv(envToken, runner.Token())
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
provider "autopilot" {
  endpoint = %q
}

resource "autopilot_app" "test" {
  name = %q
}
`, elsewhere.URL, randName()),
				ExpectError: expectErr("Incomplete AutoPilot provider block ... sets `endpoint` but not `token`"),
			},
		},
	})
	for name, fake := range map[string]*autopilottest.Server{"the runner's AutoPilot": runner, "the other AutoPilot": elsewhere} {
		if n := len(fake.Requests()); n != 0 {
			t.Errorf("%s got %d request(s)", name, n)
		}
	}
}
