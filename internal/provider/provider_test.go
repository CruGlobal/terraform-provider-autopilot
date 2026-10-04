package provider

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/CruGlobal/terraform-provider-autopilot/internal/autopilottest"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
)

// protoV6ProviderFactories is wired into every resource.TestCase.
var protoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"autopilot": providerserver.NewProtocol6WithError(New("test")()),
}

// testEnv is what a test needs to point the provider at a backend.
type testEnv struct {
	endpoint string
	token    string
	// fake is nil when running against a live AutoPilot.
	fake *autopilottest.Server
}

func (e *testEnv) live() bool { return e.fake == nil }

// newTestEnv picks the backend. With TF_ACC=1 and AUTOPILOT_ENDPOINT /
// AUTOPILOT_TOKEN set, the test bodies run against that live AutoPilot.
// Otherwise they run against the in-process fake.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	if os.Getenv(resource.EnvTfAcc) != "" && os.Getenv(envEndpoint) != "" {
		if os.Getenv(envToken) == "" {
			t.Fatalf("%s must be set when %s is set", envToken, envEndpoint)
		}
		liveTestsRun.Add(1)
		return &testEnv{endpoint: os.Getenv(envEndpoint), token: os.Getenv(envToken)}
	}
	fake := autopilottest.New(t)
	return &testEnv{endpoint: fake.URL, token: fake.Token(), fake: fake}
}

// requireFake skips a test that only makes sense against the fake (fault
// injection, request-log assertions, writes from outside Terraform).
func (e *testEnv) requireFake(t *testing.T) {
	t.Helper()
	if e.live() {
		t.Skip("exercises the fake's fault injection")
	}
}

// providerConfig renders a provider block pointing at the backend. Against a
// live AutoPilot the block is empty: the provider reads AUTOPILOT_ENDPOINT and
// AUTOPILOT_TOKEN from the environment, so the token is never written into
// the generated configuration on disk.
func (e *testEnv) providerConfig() string {
	if e.live() {
		return `
provider "autopilot" {}
`
	}
	return fmt.Sprintf(`
provider "autopilot" {
  endpoint = %q
  token    = %q
}
`, e.endpoint, e.token)
}

// runTest executes a TestCase. resource.UnitTest skips the TF_ACC gate, so the
// same test body runs against the fake in `go test` and against a live
// AutoPilot under `task testacc`; both need the terraform CLI on PATH, and
// Terraform 1.11 or later for write-only arguments.
func runTest(t *testing.T, tc resource.TestCase) {
	t.Helper()
	if tc.ProtoV6ProviderFactories == nil {
		tc.ProtoV6ProviderFactories = protoV6ProviderFactories
	}
	tc.TerraformVersionChecks = append(tc.TerraformVersionChecks, tfversion.SkipBelow(tfversion.Version1_11_0))
	resource.UnitTest(t, tc)
}

// randName returns a record name with a random suffix, so live runs never
// collide with each other or with real records.
func randName() string {
	return "tfacc-" + acctest.RandStringFromCharSet(10, "abcdefghijklmnopqrstuvwxyz0123456789")
}

// randSecret returns a fresh random secret for one test. Tests never use a
// fixed secret, so nothing in the repository works as one.
func randSecret(t *testing.T) string {
	t.Helper()
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return "tfacc-" + hex.EncodeToString(b[:])
}

// liveTestsRun counts tests that actually ran against a live AutoPilot.
var liveTestsRun atomic.Int32

// TestMain runs the sweepers when asked (-sweep, see sweep_test.go), and
// otherwise the tests. It makes an acceptance run that asserted nothing fail
// loudly, rather than report green.
func TestMain(m *testing.M) {
	resource.TestMain(liveCountingRun{m})
}

// liveCountingRun runs the tests and reports how many reached a live
// AutoPilot.
type liveCountingRun struct{ m *testing.M }

func (r liveCountingRun) Run() int {
	code := r.m.Run()
	if os.Getenv(resource.EnvTfAcc) != "" && os.Getenv(envEndpoint) != "" {
		n := liveTestsRun.Load()
		summary := fmt.Sprintf("Acceptance run: %d test(s) exercised the live API.", n)
		if n == 0 {
			summary += " Nothing was asserted, so this run is reported as a FAILURE."
			if code == 0 {
				code = 1
			}
		}
		fmt.Fprintln(os.Stderr, summary)
		if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
			if f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
				fmt.Fprintf(f, "### Live acceptance coverage\n\n%s\n", summary)
				_ = f.Close()
			}
		}
	}
	return code
}
