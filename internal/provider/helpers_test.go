package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
)

// recordedDiagnostics collects the diagnostics every apply returned during a
// test. The testing framework only surfaces errors (via ExpectError), so this
// is how a test asserts a warning the provider produced.
type recordedDiagnostics struct {
	mu    sync.Mutex
	diags []*tfprotov6.Diagnostic
}

func (a *recordedDiagnostics) warnings() []*tfprotov6.Diagnostic {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*tfprotov6.Diagnostic
	for _, d := range a.diags {
		if d.Severity == tfprotov6.DiagnosticSeverityWarning {
			out = append(out, d)
		}
	}
	return out
}

// hasWarning reports whether any apply returned a warning with this summary.
func (a *recordedDiagnostics) hasWarning(summary string) bool {
	for _, d := range a.warnings() {
		if d.Summary == summary {
			return true
		}
	}
	return false
}

// diagnosticRecordingServer passes every call through to the provider and
// keeps a copy of what each apply and import returned.
type diagnosticRecordingServer struct {
	tfprotov6.ProviderServer
	into *recordedDiagnostics
}

func (s diagnosticRecordingServer) record(diags []*tfprotov6.Diagnostic) {
	s.into.mu.Lock()
	defer s.into.mu.Unlock()
	s.into.diags = append(s.into.diags, diags...)
}

func (s diagnosticRecordingServer) ApplyResourceChange(ctx context.Context, req *tfprotov6.ApplyResourceChangeRequest) (*tfprotov6.ApplyResourceChangeResponse, error) {
	resp, err := s.ProviderServer.ApplyResourceChange(ctx, req)
	if resp != nil {
		s.record(resp.Diagnostics)
	}
	return resp, err
}

func (s diagnosticRecordingServer) ImportResourceState(ctx context.Context, req *tfprotov6.ImportResourceStateRequest) (*tfprotov6.ImportResourceStateResponse, error) {
	resp, err := s.ProviderServer.ImportResourceState(ctx, req)
	if resp != nil {
		s.record(resp.Diagnostics)
	}
	return resp, err
}

// runTestRecordingDiagnostics is runTest with every apply's and import's
// diagnostics recorded, for tests that need to see a warning.
func runTestRecordingDiagnostics(t *testing.T, tc resource.TestCase) *recordedDiagnostics {
	t.Helper()
	recorded := &recordedDiagnostics{}
	inner := providerserver.NewProtocol6WithError(New("test")())
	tc.ProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
		"autopilot": func() (tfprotov6.ProviderServer, error) {
			server, err := inner()
			if err != nil {
				return nil, err
			}
			return diagnosticRecordingServer{ProviderServer: server, into: recorded}, nil
		},
	}
	tc.TerraformVersionChecks = append(tc.TerraformVersionChecks, tfversion.SkipBelow(tfversion.Version1_11_0))
	resource.UnitTest(t, tc)
	return recorded
}

// expectNoSecretsInPlan fails when any of the given values appears in what
// the plan holds about resources: planned values, changes, prior state and
// drift. Write-only arguments must not. The plan's "configuration" section is
// left out: it is Terraform's own copy of the configuration source, and these
// tests write the secrets into it as literals (real configurations pass them
// from another resource).
type expectNoSecretsInPlan []string

func (e expectNoSecretsInPlan) CheckPlan(_ context.Context, req plancheck.CheckPlanRequest, resp *plancheck.CheckPlanResponse) {
	raw, err := json.Marshal(req.Plan)
	if err != nil {
		resp.Error = err
		return
	}
	var top map[string]json.RawMessage
	_ = json.Unmarshal(raw, &top)
	for key, part := range top {
		if key == "configuration" {
			continue
		}
		for _, secret := range e {
			if strings.Contains(string(part), secret) {
				resp.Error = fmt.Errorf("a secret appears in the plan's %q", key)
				return
			}
		}
	}
}

// checkNoSecretsInState fails when any of the given values appears in any
// attribute of any resource in state.
func checkNoSecretsInState(secrets ...string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		for _, m := range s.Modules {
			for addr, rs := range m.Resources {
				if rs.Primary == nil {
					continue
				}
				for attr, v := range rs.Primary.Attributes {
					for _, secret := range secrets {
						if strings.Contains(v, secret) {
							return fmt.Errorf("%s.%s holds a secret", addr, attr)
						}
					}
				}
			}
		}
		return nil
	}
}

// expectErr matches an error that contains the given words in order.
// Terraform wraps long messages, so any run of whitespace between words
// matches, and "..." stands for anything at all.
func expectErr(words string) *regexp.Regexp {
	parts := strings.Fields(words)
	var b strings.Builder
	b.WriteString(`(?s)`)
	for i, p := range parts {
		switch {
		case p == "...":
			b.WriteString(`.*`)
			continue
		case i > 0 && parts[i-1] != "...":
			b.WriteString(`\s+`)
		}
		b.WriteString(regexp.QuoteMeta(p))
	}
	return regexp.MustCompile(b.String())
}
