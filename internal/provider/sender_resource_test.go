package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/CruGlobal/terraform-provider-autopilot/internal/autopilottest"
	"github.com/CruGlobal/terraform-provider-autopilot/internal/client"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const senderRes = "autopilot_sender.test"

type secretPair struct{ request, callback string }

func newSecretPair(t *testing.T) secretPair {
	return secretPair{request: randSecret(t), callback: randSecret(t)}
}

func senderConfig(env *testEnv, name string, s secretPair, body string) string {
	return env.providerConfig() + fmt.Sprintf(`
resource "autopilot_sender" "test" {
  name               = %q
  request_secret_wo  = %q
  callback_secret_wo = %q
%s
}
`, name, s.request, s.callback, body)
}

// apiClient is a client for the test's backend, for checks made outside
// Terraform.
func (e *testEnv) apiClient(t *testing.T) *client.Client {
	t.Helper()
	c, err := client.New(e.endpoint, e.token)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// checkSendersGone verifies after the destroy that no sender in the prior
// state can still be read.
func checkSendersGone(t *testing.T, env *testEnv) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		c := env.apiClient(t)
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "autopilot_sender" {
				continue
			}
			name := rs.Primary.Attributes["name"]
			_, err := c.GetSender(context.Background(), name)
			if err == nil {
				return fmt.Errorf("sender %q still exists", name)
			}
			if !client.IsNotFound(err) {
				return err
			}
		}
		return nil
	}
}

// requestBody decodes a recorded request's JSON body.
func requestBody(t *testing.T, r autopilottest.RecordedRequest) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatalf("%s %s body is not JSON: %v", r.Method, r.Path, err)
	}
	return body
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func fingerprintIs(attr, secret string) plancheck.PlanCheck {
	return plancheck.ExpectKnownValue(senderRes, tfjsonpath.New(attr), knownvalue.StringExact(client.Fingerprint(secret)))
}

func TestSender_basicLifecycle(t *testing.T) {
	env := newTestEnv(t)
	name := randName()
	secrets := newSecretPair(t)
	path := "/v1/admin/senders/" + name
	runTest(t, resource.TestCase{
		CheckDestroy: checkSendersGone(t, env),
		Steps: []resource.TestStep{
			{
				Config: senderConfig(env, name, secrets, `
  callback_hosts = ["callbacks.example.com"]
  kinds          = ["implement-work-item", "research"]`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						expectNoSecretsInPlan{secrets.request, secrets.callback},
						// The plan already shows the fingerprints AutoPilot will report.
						fingerprintIs("request_secret_fingerprint", secrets.request),
						fingerprintIs("callback_secret_fingerprint", secrets.callback),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(senderRes, "name", name),
					resource.TestCheckResourceAttr(senderRes, "callback_hosts.#", "1"),
					resource.TestCheckTypeSetElemAttr(senderRes, "callback_hosts.*", "callbacks.example.com"),
					resource.TestCheckResourceAttr(senderRes, "kinds.#", "2"),
					resource.TestCheckTypeSetElemAttr(senderRes, "kinds.*", "research"),
					resource.TestCheckResourceAttr(senderRes, "branch_prefix", name+"/"),
					resource.TestCheckResourceAttr(senderRes, "request_secret_fingerprint", client.Fingerprint(secrets.request)),
					resource.TestCheckResourceAttr(senderRes, "callback_secret_fingerprint", client.Fingerprint(secrets.callback)),
					resource.TestCheckResourceAttrSet(senderRes, "secrets_changed_at"),
					resource.TestCheckNoResourceAttr(senderRes, "previous_secrets_until"),
					resource.TestCheckResourceAttr(senderRes, "lock_version", "1"),
					// Write-only arguments are never persisted.
					resource.TestCheckNoResourceAttr(senderRes, "request_secret_wo"),
					resource.TestCheckNoResourceAttr(senderRes, "callback_secret_wo"),
					checkNoSecretsInState(secrets.request, secrets.callback),
					func(*terraform.State) error {
						if env.live() {
							return nil
						}
						posts := env.fake.RequestsMatching(http.MethodPost, "/v1/admin/senders")
						if len(posts) != 1 {
							return fmt.Errorf("%d creates, want 1", len(posts))
						}
						if h := posts[0].Header.Get("Idempotency-Key"); h != "" {
							return fmt.Errorf("a create carried Idempotency-Key %q; the name is the key", h)
						}
						body := requestBody(t, posts[0])
						if body["request_secret"] != secrets.request || body["callback_secret"] != secrets.callback {
							return fmt.Errorf("the create did not send the configured secrets")
						}
						return nil
					},
				),
			},
			{
				// Applying the same configuration again plans nothing: the
				// fingerprints of the configured secrets match the stored ones.
				Config: senderConfig(env, name, secrets, `
  callback_hosts = ["callbacks.example.com"]
  kinds          = ["implement-work-item", "research"]`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				Config: senderConfig(env, name, secrets, fmt.Sprintf(`
  callback_hosts = ["callbacks.example.com", "hooks.example.com"]
  kinds          = ["implement-work-item", "review-pr"]
  branch_prefix  = %q`, name+"-bots/")),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(senderRes, plancheck.ResourceActionUpdate),
						expectNoSecretsInPlan{secrets.request, secrets.callback},
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(senderRes, "callback_hosts.#", "2"),
					resource.TestCheckTypeSetElemAttr(senderRes, "kinds.*", "review-pr"),
					resource.TestCheckResourceAttr(senderRes, "branch_prefix", name+"-bots/"),
					resource.TestCheckResourceAttr(senderRes, "lock_version", "2"),
					// An update that does not touch the secrets leaves them be.
					resource.TestCheckNoResourceAttr(senderRes, "previous_secrets_until"),
					checkNoSecretsInState(secrets.request, secrets.callback),
					func(*terraform.State) error {
						if env.live() {
							return nil
						}
						patches := env.fake.RequestsMatching(http.MethodPatch, path)
						if len(patches) != 1 {
							return fmt.Errorf("%d changes, want 1", len(patches))
						}
						if h := patches[0].Header.Get("If-Match"); h != `"1"` {
							return fmt.Errorf("If-Match = %q, want the state's lock_version, quoted", h)
						}
						body := requestBody(t, patches[0])
						if got := keysOf(body); !slices.Equal(got, []string{"branch_prefix", "callback_hosts", "kinds"}) {
							return fmt.Errorf("the change sent %v; it should send only what changed, and not the secrets", got)
						}
						return nil
					},
				),
			},
		},
	})
}

func TestSender_defaults(t *testing.T) {
	env := newTestEnv(t)
	name := randName()
	secrets := newSecretPair(t)
	runTest(t, resource.TestCase{
		CheckDestroy: checkSendersGone(t, env),
		Steps: []resource.TestStep{
			{
				Config: senderConfig(env, name, secrets, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(senderRes, "callback_hosts.#", "0"),
					resource.TestCheckResourceAttr(senderRes, "kinds.#", "1"),
					resource.TestCheckTypeSetElemAttr(senderRes, "kinds.*", "implement-work-item"),
					resource.TestCheckResourceAttr(senderRes, "branch_prefix", name+"/"),
					func(*terraform.State) error {
						if env.live() {
							return nil
						}
						post := env.fake.RequestsMatching(http.MethodPost, "/v1/admin/senders")[0]
						if got := keysOf(requestBody(t, post)); !slices.Equal(got, []string{"callback_secret", "name", "request_secret"}) {
							return fmt.Errorf("the create sent %v; unset fields take AutoPilot's defaults", got)
						}
						return nil
					},
				),
			},
			{
				Config: senderConfig(env, name, secrets, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// A create whose answer was lost is sent again, and AutoPilot answers the
// identical create with the record it made.
func TestSender_identicalRetryAfterLostAnswer(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	secrets := newSecretPair(t)
	diags := runTestRecordingDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				PreConfig: func() { env.fake.DropNextResponse(http.MethodPost, "/v1/admin/senders") },
				Config:    senderConfig(env, name, secrets, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(senderRes, "lock_version", "1"),
					func(*terraform.State) error {
						posts := env.fake.RequestsMatching(http.MethodPost, "/v1/admin/senders")
						if len(posts) != 2 || posts[0].Status != http.StatusCreated || posts[1].Status != http.StatusOK {
							return fmt.Errorf("want a 201 whose answer was lost, then a 200 for the same create; got %d creates", len(posts))
						}
						for _, p := range posts {
							if p.Header.Get("Idempotency-Key") != "" {
								return fmt.Errorf("a create carried an Idempotency-Key")
							}
						}
						return nil
					},
				),
			},
		},
	})
	if diags.hasWarning("Sender already existed") {
		t.Error("the provider's own lost create was reported as someone else's sender")
	}
}

func TestSender_adoptsAnIdenticalRecordWithAWarning(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	secrets := newSecretPair(t)
	env.fake.SeedSender(autopilottest.SenderSeed{Name: name, RequestSecret: secrets.request, CallbackSecret: secrets.callback})
	diags := runTestRecordingDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: senderConfig(env, name, secrets, ""),
				Check:  resource.TestCheckResourceAttr(senderRes, "lock_version", "1"),
			},
		},
	})
	if !diags.hasWarning("Sender already existed") {
		t.Error("adopting an existing sender should warn")
	}
}

func TestSender_nameTaken(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	secrets := newSecretPair(t)
	env.fake.SeedSender(autopilottest.SenderSeed{Name: name, Kinds: []string{"research"},
		RequestSecret: secrets.request, CallbackSecret: secrets.callback})
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      senderConfig(env, name, secrets, ""),
				ExpectError: expectErr("A sender with this name already exists ... terraform import autopilot_sender"),
			},
		},
	})
}

func TestSender_nameRetired(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	env.fake.RetiredSendersLinger(true)
	name := randName()
	secrets := newSecretPair(t)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: senderConfig(env, name, secrets, "")},
			{
				// Removing the resource retires the sender; it lingers while
				// its tasks finish.
				Config: env.providerConfig(),
				Check: func(*terraform.State) error {
					if v, ok := env.fake.Sender(name); !ok || !v.Retired {
						return fmt.Errorf("the sender should be retired, not removed")
					}
					return nil
				},
			},
			{
				Config:      senderConfig(env, name, secrets, ""),
				ExpectError: expectErr("This sender name is retired"),
			},
		},
	})
}

func TestSender_branchPrefixTaken(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	other := randName()
	env.fake.SeedSender(autopilottest.SenderSeed{Name: other, BranchPrefix: "shared/",
		RequestSecret: randSecret(t), CallbackSecret: randSecret(t)})
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      senderConfig(env, randName(), newSecretPair(t), `  branch_prefix = "shared/"`),
				ExpectError: expectErr("Another sender has this branch prefix"),
			},
		},
	})
}

// A kind AutoPilot doesn't know is refused by the API (the provider checks
// only a kind's shape), and the refusal points at the attribute.
func TestSender_refusalNamesTheAttribute(t *testing.T) {
	env := newTestEnv(t)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      senderConfig(env, randName(), newSecretPair(t), `  kinds = ["tfacc-no-such-kind"]`),
				ExpectError: expectErr("with autopilot_sender.test, ... kinds ... invalid_attribute"),
			},
		},
	})
}

// Changing a secret in configuration changes its fingerprint, which is how
// the provider plans a rotation although the secret itself is never stored.
func TestSender_rotationByFingerprint(t *testing.T) {
	env := newTestEnv(t)
	name := randName()
	first, second := newSecretPair(t), newSecretPair(t)
	runTest(t, resource.TestCase{
		CheckDestroy: checkSendersGone(t, env),
		Steps: []resource.TestStep{
			{Config: senderConfig(env, name, first, "")},
			{
				Config: senderConfig(env, name, second, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(senderRes, plancheck.ResourceActionUpdate),
						fingerprintIs("request_secret_fingerprint", second.request),
						fingerprintIs("callback_secret_fingerprint", second.callback),
						expectNoSecretsInPlan{first.request, first.callback, second.request, second.callback},
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(senderRes, "request_secret_fingerprint", client.Fingerprint(second.request)),
					resource.TestCheckResourceAttr(senderRes, "callback_secret_fingerprint", client.Fingerprint(second.callback)),
					// AutoPilot keeps the old pair for the overlap.
					resource.TestCheckResourceAttrSet(senderRes, "previous_secrets_until"),
					resource.TestCheckResourceAttr(senderRes, "lock_version", "2"),
					checkNoSecretsInState(first.request, first.callback, second.request, second.callback),
					func(*terraform.State) error {
						if env.live() {
							return nil
						}
						v, _ := env.fake.Sender(name)
						if v.RequestSecret != second.request || v.CallbackSecret != second.callback {
							return fmt.Errorf("AutoPilot does not hold the new secrets")
						}
						if v.PreviousRequestSecret != first.request || v.PreviousCallbackSecret != first.callback {
							return fmt.Errorf("AutoPilot does not hold the old secrets for the overlap")
						}
						return nil
					},
				),
			},
			{
				Config: senderConfig(env, name, second, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// Changing only one secret still sends both, as AutoPilot requires.
func TestSender_rotatingOneSecretSendsBoth(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	first := newSecretPair(t)
	second := secretPair{request: first.request, callback: randSecret(t)}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: senderConfig(env, name, first, "")},
			{
				Config: senderConfig(env, name, second, ""),
				Check: func(*terraform.State) error {
					patches := env.fake.RequestsMatching(http.MethodPatch, "/v1/admin/senders/"+name)
					if len(patches) != 1 {
						return fmt.Errorf("%d changes, want 1", len(patches))
					}
					if got := keysOf(requestBody(t, patches[0])); !slices.Equal(got, []string{"callback_secret", "request_secret"}) {
						return fmt.Errorf("the rotation sent %v, want both secrets", got)
					}
					return nil
				},
			},
		},
	})
}

// secrets_wo_version sends the secrets again whatever the fingerprints say.
// Sending the secrets AutoPilot already holds changes nothing, so the
// record's version stays.
func TestSender_secretsVersionSendsThemAgain(t *testing.T) {
	env := newTestEnv(t)
	name := randName()
	secrets := newSecretPair(t)
	runTest(t, resource.TestCase{
		CheckDestroy: checkSendersGone(t, env),
		Steps: []resource.TestStep{
			{
				Config: senderConfig(env, name, secrets, `  secrets_wo_version = 1`),
				Check:  resource.TestCheckResourceAttr(senderRes, "secrets_wo_version", "1"),
			},
			{
				Config: senderConfig(env, name, secrets, `  secrets_wo_version = 2`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(senderRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(senderRes, "secrets_wo_version", "2"),
					resource.TestCheckResourceAttr(senderRes, "lock_version", "1"),
					resource.TestCheckNoResourceAttr(senderRes, "previous_secrets_until"),
					func(*terraform.State) error {
						if env.live() {
							return nil
						}
						patches := env.fake.RequestsMatching(http.MethodPatch, "/v1/admin/senders/"+name)
						if len(patches) != 1 {
							return fmt.Errorf("%d changes, want 1", len(patches))
						}
						if got := keysOf(requestBody(t, patches[0])); !slices.Equal(got, []string{"callback_secret", "request_secret"}) {
							return fmt.Errorf("the change sent %v, want both secrets", got)
						}
						return nil
					},
				),
			},
			{
				Config: senderConfig(env, name, secrets, `  secrets_wo_version = 2`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// Somebody sets other secrets outside Terraform. The next refresh reads other
// fingerprints, and the apply puts the configured secrets back.
func TestSender_secretsChangedOutsideTerraformArePutBack(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	secrets := newSecretPair(t)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: senderConfig(env, name, secrets, "")},
			{
				PreConfig: func() {
					env.fake.ChangeSenderOutOfBand(name, func(v *autopilottest.SenderView) {
						v.RequestSecret, v.CallbackSecret = randSecret(t), randSecret(t)
					})
				},
				Config: senderConfig(env, name, secrets, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(senderRes, plancheck.ResourceActionUpdate)},
				},
				Check: func(*terraform.State) error {
					v, _ := env.fake.Sender(name)
					if v.RequestSecret != secrets.request || v.CallbackSecret != secrets.callback {
						return fmt.Errorf("the configured secrets were not put back")
					}
					return nil
				},
			},
		},
	})
}

// A secret made by another resource in the same apply is unknown at plan
// time, so it can't be fingerprinted then: an update is planned, and the
// apply sends it.
func TestSender_secretsUnknownAtPlanTime(t *testing.T) {
	env := newTestEnv(t)
	name := randName()
	config := func(seed string) string {
		return env.providerConfig() + fmt.Sprintf(`
resource "terraform_data" "seed" {
  input = %q
}

resource "autopilot_sender" "test" {
  name               = %q
  request_secret_wo  = "${terraform_data.seed.output}-request"
  callback_secret_wo = "${terraform_data.seed.output}-callback"
}
`, seed, name)
	}
	first, second := randSecret(t), randSecret(t)
	runTest(t, resource.TestCase{
		CheckDestroy: checkSendersGone(t, env),
		Steps: []resource.TestStep{
			{
				Config: config(first),
				Check:  resource.TestCheckResourceAttr(senderRes, "request_secret_fingerprint", client.Fingerprint(first+"-request")),
			},
			{
				Config: config(second),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(senderRes, plancheck.ResourceActionUpdate),
						plancheck.ExpectUnknownValue(senderRes, tfjsonpath.New("request_secret_fingerprint")),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(senderRes, "request_secret_fingerprint", client.Fingerprint(second+"-request")),
					resource.TestCheckResourceAttr(senderRes, "callback_secret_fingerprint", client.Fingerprint(second+"-callback")),
				),
			},
		},
	})
}

// A write from outside Terraform between plan and apply is reported, not
// overwritten. The destroy that follows meets the same stale version, reads
// the current one and deletes anyway.
func TestSender_staleUpdateIsReportedAndDeleteRereads(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	secrets := newSecretPair(t)
	path := "/v1/admin/senders/" + name
	runTest(t, resource.TestCase{
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkSendersGone(t, env),
			func(*terraform.State) error {
				deletes := env.fake.RequestsMatching(http.MethodDelete, path)
				if len(deletes) != 2 || deletes[0].Status != http.StatusConflict || deletes[1].Status != http.StatusNoContent {
					return fmt.Errorf("want a stale delete, then a delete at the current version; got %d deletes", len(deletes))
				}
				return nil
			},
		),
		Steps: []resource.TestStep{
			{Config: senderConfig(env, name, secrets, "")},
			{
				PreConfig: func() {
					env.fake.OnNextRequest(http.MethodPatch, path, func() {
						env.fake.ChangeSenderOutOfBand(name, func(v *autopilottest.SenderView) { v.CallbackHosts = []string{"elsewhere.example.com"} })
					})
				},
				Config:      senderConfig(env, name, secrets, `  kinds = ["research"]`),
				ExpectError: expectErr("changed outside of Terraform ... lock_version 1, AutoPilot now has 2). Nothing was overwritten"),
			},
		},
	})
}

// The first copy of a change is applied but its answer is lost. The client
// sends it again, meets the version its own change made, and recognises the
// change as its own.
func TestSender_lostChangeAnswerIsRecognised(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	secrets := newSecretPair(t)
	path := "/v1/admin/senders/" + name
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: senderConfig(env, name, secrets, "")},
			{
				PreConfig: func() { env.fake.DropNextResponse(http.MethodPatch, path) },
				Config:    senderConfig(env, name, secrets, `  kinds = ["research"]`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(senderRes, "lock_version", "2"),
					resource.TestCheckTypeSetElemAttr(senderRes, "kinds.*", "research"),
					func(*terraform.State) error {
						patches := env.fake.RequestsMatching(http.MethodPatch, path)
						if len(patches) != 2 || patches[1].Status != http.StatusConflict {
							return fmt.Errorf("want a change whose answer was lost, then a stale copy; got %d changes", len(patches))
						}
						return nil
					},
				),
			},
		},
	})
}

// A sender deleted outside Terraform drops out of state on refresh, and the
// next apply makes it again.
func TestSender_deletedOutsideTerraformIsMadeAgain(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	secrets := newSecretPair(t)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: senderConfig(env, name, secrets, "")},
			{
				PreConfig: func() { env.fake.RemoveSender(name) },
				Config:    senderConfig(env, name, secrets, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(senderRes, plancheck.ResourceActionCreate)},
				},
				Check: func(*terraform.State) error {
					if _, ok := env.fake.Sender(name); !ok {
						return fmt.Errorf("the sender was not made again")
					}
					return nil
				},
			},
		},
	})
}

func TestSender_import(t *testing.T) {
	env := newTestEnv(t)
	name := randName()
	secrets := newSecretPair(t)
	runTest(t, resource.TestCase{
		CheckDestroy: checkSendersGone(t, env),
		Steps: []resource.TestStep{
			{Config: senderConfig(env, name, secrets, `  kinds = ["implement-work-item", "research"]`)},
			{
				ResourceName:                         senderRes,
				ImportState:                          true,
				ImportStateId:                        name,
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "name",
			},
		},
	})
}

// An imported sender's secrets come from configuration. When they match the
// stored ones (by fingerprint) the plan after the import is empty; when they
// don't, the next apply sends them.
func TestSender_importThenPlan(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	secrets := newSecretPair(t)
	env.fake.SeedSender(autopilottest.SenderSeed{Name: name, RequestSecret: secrets.request, CallbackSecret: secrets.callback})
	other := newSecretPair(t)
	diags := runTestRecordingDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:             senderConfig(env, name, secrets, ""),
				ResourceName:       senderRes,
				ImportState:        true,
				ImportStateId:      name,
				ImportStatePersist: true,
			},
			{
				Config: senderConfig(env, name, secrets, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				Config: senderConfig(env, name, other, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(senderRes, plancheck.ResourceActionUpdate)},
				},
				Check: func(*terraform.State) error {
					if v, _ := env.fake.Sender(name); v.RequestSecret != other.request {
						return fmt.Errorf("the configured secrets were not sent")
					}
					return nil
				},
			},
		},
	})
	if !diags.hasWarning("Imported sender has no secrets in Terraform") {
		t.Error("an import should say the secrets come from configuration")
	}
}

func TestSender_importUnknownName(t *testing.T) {
	env := newTestEnv(t)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:        senderConfig(env, "tfacc-placeholder", newSecretPair(t), ""),
				ResourceName:  senderRes,
				ImportState:   true,
				ImportStateId: randName(),
				ExpectError:   expectErr("No such sender"),
			},
		},
	})
}

// AutoPilot's 503 unavailable is retried with backoff.
func TestSender_unavailableIsRetried(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				PreConfig: func() { env.fake.UnavailableNext(2) },
				Config:    senderConfig(env, randName(), newSecretPair(t), ""),
				Check:     resource.TestCheckResourceAttr(senderRes, "lock_version", "1"),
			},
		},
	})
}

// Mistakes AutoPilot would refuse fail the plan instead, in AutoPilot's words.
func TestSender_planTimeValidation(t *testing.T) {
	env := newTestEnv(t)
	secrets := newSecretPair(t)
	name := randName()
	cases := []struct {
		config string
		want   string
	}{
		{senderConfig(env, name, secrets, `  callback_hosts = ["Callbacks.Example.com"]`), "uppercase letters ... lowercase DNS name with at least one dot"},
		{senderConfig(env, name, secrets, `  callback_hosts = ["192.0.2.10"]`), "is an IP address"},
		{senderConfig(env, name, secrets, `  callback_hosts = ["localhost"]`), "is localhost"},
		{senderConfig(env, name, secrets, `  callback_hosts = ["https://callbacks.example.com"]`), "not a bare host name"},
		{senderConfig(env, name, secrets, `  callback_hosts = ["intranet"]`), "has no dot"},
		{senderConfig(env, name, secrets, `  kinds = []`), "at least 1"},
		{senderConfig(env, name, secrets, `  branch_prefix = "Bots"`), "lowercase word and a /"},
		{senderConfig(env, "Tracker", secrets, ""), "lowercase letter, then lowercase letters"},
		{senderConfig(env, name, secretPair{request: secrets.request, callback: secrets.request}, ""), "The two secrets are the same"},
		{senderConfig(env, name, secretPair{request: "too-short", callback: secrets.callback}, ""), "at least 32"},
	}
	steps := make([]resource.TestStep, 0, len(cases))
	for _, c := range cases {
		steps = append(steps, resource.TestStep{Config: c.config, PlanOnly: true, ExpectError: expectErr(c.want)})
	}
	runTest(t, resource.TestCase{Steps: steps})
}

// Only AutoPilot's not_found refusal means a sender is gone. A plain 404 (a
// proxy, a page that isn't AutoPilot's, a wrong path) fails the refresh or
// the delete, instead of dropping the sender from state or calling it deleted.
func TestSender_onlyTheNotFoundRefusalMeansGone(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	secrets := newSecretPair(t)
	path := "/v1/admin/senders/" + name
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: senderConfig(env, name, secrets, "")},
			{
				PreConfig: func() {
					env.fake.RespondNext(http.MethodGet, path, http.StatusNotFound, "text/html", "<html>Not Found</html>")
				},
				Config:      senderConfig(env, name, secrets, ""),
				ExpectError: expectErr("Error reading AutoPilot sender ... this 404 is not one"),
			},
			{
				PreConfig: func() {
					env.fake.RespondNext(http.MethodDelete, path, http.StatusNotFound, "text/html", "<html>Not Found</html>")
				},
				Config:      senderConfig(env, name, secrets, ""),
				Destroy:     true,
				ExpectError: expectErr("Error deleting AutoPilot sender ... this 404 is not one"),
			},
			{
				// Gone by the time the delete arrives: AutoPilot's not_found
				// refusal, which is success.
				PreConfig: func() {
					env.fake.OnNextRequest(http.MethodDelete, path, func() { env.fake.RemoveSender(name) })
				},
				Config:  senderConfig(env, name, secrets, ""),
				Destroy: true,
			},
		},
	})
}

func TestSender_wrongTokenIsReported(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	wrong := *env
	wrong.token = "wrong-" + env.token
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      senderConfig(&wrong, randName(), newSecretPair(t), ""),
				ExpectError: expectErr("HTTP 401 (unauthorized) ... AutoPilot refused the admin token"),
			},
		},
	})
}
