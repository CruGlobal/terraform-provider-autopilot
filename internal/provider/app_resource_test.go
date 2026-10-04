package provider

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-autopilot/internal/autopilottest"
	"github.com/CruGlobal/terraform-provider-autopilot/internal/client"
	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const appRes = "autopilot_app.test"

// envAccRepos optionally names repositories (comma-separated owner/name) the
// live AutoPilot lets an app own. A test AutoPilot limits the repositories
// an app may own, so live tests can't make up their own. Without it, the app
// tests run live without repositories.
const envAccRepos = "AUTOPILOT_ACC_REPOS"

// liveRepos returns the repositories a live app may own, or n made-up ones
// for the fake. ok is false when a live run has fewer than n.
func (e *testEnv) liveRepos(n int) (repos []string, ok bool) {
	if !e.live() {
		for i := range n {
			repos = append(repos, fmt.Sprintf("example-org/repo-%d-%s", i, acctest.RandStringFromCharSet(6, acctest.CharSetAlphaNum)))
		}
		return repos, true
	}
	for _, r := range strings.Split(os.Getenv(envAccRepos), ",") {
		if r = strings.TrimSpace(r); r != "" {
			repos = append(repos, r)
		}
	}
	if len(repos) < n {
		return nil, false
	}
	return repos[:n], true
}

func appConfig(env *testEnv, name, body string) string {
	return env.providerConfig() + fmt.Sprintf(`
resource "autopilot_app" "test" {
  name = %q
%s
}
`, name, body)
}

// hcl renders a list of strings as an HCL list.
func hcl(values ...string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, fmt.Sprintf("%q", v))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func randLogin() string { return randName() + "@example.com" }

// checkAppsGone verifies after the destroy that no app in the prior state can
// still be read.
func checkAppsGone(t *testing.T, env *testEnv) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		c := env.apiClient(t)
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "autopilot_app" {
				continue
			}
			name := rs.Primary.Attributes["name"]
			_, err := c.GetApp(context.Background(), name)
			if err == nil {
				return fmt.Errorf("app %q still exists", name)
			}
			if !client.IsNotFound(err) {
				return err
			}
		}
		return nil
	}
}

func TestApp_basicLifecycle(t *testing.T) {
	env := newTestEnv(t)
	name := randName()
	tracker, monitor := randName(), randName()
	devA, devB := randLogin(), randLogin()
	// A live AutoPilot limits the repositories an app may own; without a
	// repository to use, the app is tested without one.
	reposLine := ""
	repos, haveRepos := env.liveRepos(1)
	if haveRepos {
		reposLine = "  repos = " + hcl(repos...)
	}
	path := "/v1/admin/apps/" + name
	first := fmt.Sprintf(`%s
  accepts = [
    { sender = %q, kinds = ["implement-work-item", "review-pr"], can_decide = true },
    { sender = %q, kinds = ["fix-error"] },
  ]
  developers = %s`, reposLine, tracker, monitor, hcl(devA, devB))
	second := fmt.Sprintf(`%s
  accepts = [
    { sender = %q, kinds = ["implement-work-item", "review-pr"], can_decide = true },
    { sender = %q, kinds = ["fix-error", "research"] },
  ]`, reposLine, tracker, monitor)

	runTest(t, resource.TestCase{
		CheckDestroy: checkAppsGone(t, env),
		Steps: []resource.TestStep{
			{
				Config: appConfig(env, name, first),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(appRes, "name", name),
					resource.TestCheckResourceAttr(appRes, "accepts.#", "2"),
					resource.TestCheckTypeSetElemNestedAttrs(appRes, "accepts.*", map[string]string{
						"sender": tracker, "kinds.#": "2", "can_decide": "true",
					}),
					// can_decide defaults to false.
					resource.TestCheckTypeSetElemNestedAttrs(appRes, "accepts.*", map[string]string{
						"sender": monitor, "kinds.#": "1", "can_decide": "false",
					}),
					resource.TestCheckResourceAttr(appRes, "developers.#", "2"),
					resource.TestCheckTypeSetElemAttr(appRes, "developers.*", devA),
					resource.TestCheckResourceAttr(appRes, "lock_version", "1"),
					func(s *terraform.State) error {
						if haveRepos {
							return resource.TestCheckTypeSetElemAttr(appRes, "repos.*", repos[0])(s)
						}
						return resource.TestCheckNoResourceAttr(appRes, "repos.#")(s)
					},
				),
			},
			{
				Config: appConfig(env, name, first),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// Removing developers from the configuration empties them.
				Config: appConfig(env, name, second),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckTypeSetElemNestedAttrs(appRes, "accepts.*", map[string]string{"sender": monitor, "kinds.#": "2"}),
					resource.TestCheckNoResourceAttr(appRes, "developers.#"),
					resource.TestCheckResourceAttr(appRes, "lock_version", "2"),
					func(*terraform.State) error {
						if env.live() {
							return nil
						}
						patches := env.fake.RequestsMatching(http.MethodPatch, path)
						if len(patches) != 1 {
							return fmt.Errorf("%d changes, want 1", len(patches))
						}
						if h := patches[0].Header.Get("If-Match"); h != `"1"` {
							return fmt.Errorf("If-Match = %q", h)
						}
						body := requestBody(t, patches[0])
						if got := keysOf(body); !slices.Equal(got, []string{"accepts", "developers"}) {
							return fmt.Errorf("the change sent %v; it should send only what changed", got)
						}
						if devs, _ := body["developers"].([]any); devs == nil || len(devs) != 0 {
							return fmt.Errorf("removed developers were sent as %v, want []", body["developers"])
						}
						return nil
					},
				),
			},
			{
				Config: appConfig(env, name, second),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// An app with only a name sends only its name; the lists are null in state
// although AutoPilot answers [] for them, so nothing shows as a change.
func TestApp_onlyName(t *testing.T) {
	env := newTestEnv(t)
	name := randName()
	runTest(t, resource.TestCase{
		CheckDestroy: checkAppsGone(t, env),
		Steps: []resource.TestStep{
			{
				Config: appConfig(env, name, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(appRes, "repos.#"),
					resource.TestCheckNoResourceAttr(appRes, "accepts.#"),
					resource.TestCheckNoResourceAttr(appRes, "developers.#"),
					func(*terraform.State) error {
						if env.live() {
							return nil
						}
						post := env.fake.RequestsMatching(http.MethodPost, "/v1/admin/apps")[0]
						if got := keysOf(requestBody(t, post)); !slices.Equal(got, []string{"name"}) {
							return fmt.Errorf("the create sent %v, want only the name", got)
						}
						return nil
					},
				),
			},
			{
				Config: appConfig(env, name, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// Removing every argument empties the app: an app without accepts accepts
// no sender.
func TestApp_removingArgumentsEmptiesThem(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	repos, _ := env.liveRepos(2)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: appConfig(env, name, fmt.Sprintf(`
  repos      = %s
  accepts    = [{ sender = %q, kinds = ["research"] }]
  developers = [%q]`, hcl(repos...), randName(), randLogin())),
			},
			{
				Config: appConfig(env, name, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(appRes, "repos.#"),
					resource.TestCheckNoResourceAttr(appRes, "accepts.#"),
					func(*terraform.State) error {
						v, _ := env.fake.App(name)
						if len(v.Repos)+len(v.Accepts)+len(v.Developers) != 0 {
							return fmt.Errorf("AutoPilot still holds %+v", v)
						}
						return nil
					},
				),
			},
			{
				Config: appConfig(env, name, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// can_decide is sent only when true: false is AutoPilot's default, and an
// entry whose kinds lack review-pr never carries it.
func TestApp_canDecideSentOnlyWhenTrue(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: appConfig(env, name, fmt.Sprintf(`
  accepts = [
    { sender = %q, kinds = ["research"], can_decide = false },
    { sender = %q, kinds = ["review-pr"], can_decide = true },
  ]`, "aaa-"+randName(), "zzz-"+randName())),
				Check: func(*terraform.State) error {
					post := env.fake.RequestsMatching(http.MethodPost, "/v1/admin/apps")[0]
					accepts, _ := requestBody(t, post)["accepts"].([]any)
					if len(accepts) != 2 {
						return fmt.Errorf("accepts = %v", accepts)
					}
					first, _ := accepts[0].(map[string]any)
					second, _ := accepts[1].(map[string]any)
					if _, ok := first["can_decide"]; ok {
						return fmt.Errorf("can_decide false was sent")
					}
					if second["can_decide"] != true {
						return fmt.Errorf("can_decide true was not sent")
					}
					return nil
				},
			},
		},
	})
}

func TestApp_lostCreateAnswerIsSentAgain(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	diags := runTestRecordingDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				PreConfig: func() { env.fake.DropNextResponse(http.MethodPost, "/v1/admin/apps") },
				Config:    appConfig(env, name, fmt.Sprintf(`  accepts = [{ sender = %q, kinds = ["research"] }]`, randName())),
				Check: func(*terraform.State) error {
					posts := env.fake.RequestsMatching(http.MethodPost, "/v1/admin/apps")
					if len(posts) != 2 || posts[1].Status != http.StatusOK {
						return fmt.Errorf("want a lost create, then a 200 for the same create; got %d creates", len(posts))
					}
					return nil
				},
			},
		},
	})
	if diags.hasWarning("App already existed") {
		t.Error("the provider's own lost create was reported as someone else's app")
	}
}

func TestApp_adoptsAnIdenticalRecordWithAWarning(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	env.fake.SeedApp(autopilottest.AppView{Name: name, Developers: []string{"someone@example.com"}})
	diags := runTestRecordingDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: appConfig(env, name, `  developers = ["someone@example.com"]`)},
		},
	})
	if !diags.hasWarning("App already existed") {
		t.Error("adopting an existing app should warn")
	}
}

func TestApp_nameTaken(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	env.fake.SeedApp(autopilottest.AppView{Name: name, Developers: []string{"someone@example.com"}})
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      appConfig(env, name, ""),
				ExpectError: expectErr("An app with this name already exists ... terraform import autopilot_app"),
			},
		},
	})
}

func TestApp_repoTaken(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	other := randName()
	env.fake.SeedApp(autopilottest.AppView{Name: other, Repos: []string{"Example-Org/Shared"}})
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Matched without regard to case.
				Config:      appConfig(env, randName(), `  repos = ["example-org/shared"]`),
				ExpectError: expectErr("Another app owns this repository ... belongs to the app " + other),
			},
		},
	})
}

func TestApp_import(t *testing.T) {
	env := newTestEnv(t)
	name := randName()
	runTest(t, resource.TestCase{
		CheckDestroy: checkAppsGone(t, env),
		Steps: []resource.TestStep{
			{
				Config: appConfig(env, name, fmt.Sprintf(`
  accepts    = [{ sender = %q, kinds = ["review-pr"], can_decide = true }]
  developers = [%q]`, randName(), randLogin())),
			},
			{
				ResourceName:                         appRes,
				ImportState:                          true,
				ImportStateId:                        name,
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "name",
			},
		},
	})
}

func TestApp_importThenPlanIsEmpty(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name, sender := randName(), randName()
	env.fake.SeedApp(autopilottest.AppView{
		Name:    name,
		Repos:   []string{"example-org/billing"},
		Accepts: []autopilottest.Accept{{Sender: sender, Kinds: []string{"fix-error", "research"}}},
	})
	config := appConfig(env, name, fmt.Sprintf(`
  repos   = ["example-org/billing"]
  accepts = [{ sender = %q, kinds = ["research", "fix-error"] }]`, sender))
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:             config,
				ResourceName:       appRes,
				ImportState:        true,
				ImportStateId:      name,
				ImportStatePersist: true,
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

func TestApp_staleUpdateIsReportedAndDeleteRereads(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	path := "/v1/admin/apps/" + name
	runTest(t, resource.TestCase{
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkAppsGone(t, env),
			func(*terraform.State) error {
				deletes := env.fake.RequestsMatching(http.MethodDelete, path)
				if len(deletes) != 2 || deletes[0].Status != http.StatusConflict {
					return fmt.Errorf("want a stale delete, then a delete at the current version; got %d deletes", len(deletes))
				}
				return nil
			},
		),
		Steps: []resource.TestStep{
			{Config: appConfig(env, name, `  developers = ["someone@example.com"]`)},
			{
				PreConfig: func() {
					env.fake.OnNextRequest(http.MethodPatch, path, func() {
						env.fake.ChangeAppOutOfBand(name, func(v *autopilottest.AppView) { v.Repos = []string{"example-org/elsewhere"} })
					})
				},
				Config:      appConfig(env, name, `  developers = ["someone-else@example.com"]`),
				ExpectError: expectErr("changed outside of Terraform ... Nothing was overwritten"),
			},
		},
	})
}

func TestApp_lostChangeAnswerIsRecognised(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name, sender := randName(), randName()
	path := "/v1/admin/apps/" + name
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: appConfig(env, name, fmt.Sprintf(`  accepts = [{ sender = %q, kinds = ["research"] }]`, sender))},
			{
				PreConfig: func() { env.fake.DropNextResponse(http.MethodPatch, path) },
				Config:    appConfig(env, name, fmt.Sprintf(`  accepts = [{ sender = %q, kinds = ["review-pr"], can_decide = true }]`, sender)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(appRes, "lock_version", "2"),
					func(*terraform.State) error {
						patches := env.fake.RequestsMatching(http.MethodPatch, path)
						if len(patches) != 2 || patches[1].Status != http.StatusOK {
							return fmt.Errorf("want a change whose answer was lost, then the same change answered 200; got %d changes", len(patches))
						}
						return nil
					},
				),
			},
		},
	})
}

// A repository added outside Terraform shows as a change, and the apply
// takes it away again.
func TestApp_changedOutsideTerraformIsPutBack(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: appConfig(env, name, `  repos = ["example-org/billing"]`)},
			{
				PreConfig: func() {
					env.fake.ChangeAppOutOfBand(name, func(v *autopilottest.AppView) {
						v.Repos = append(v.Repos, "example-org/payroll")
					})
				},
				Config: appConfig(env, name, `  repos = ["example-org/billing"]`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: func(*terraform.State) error {
					if v, _ := env.fake.App(name); !slices.Equal(v.Repos, []string{"example-org/billing"}) {
						return fmt.Errorf("repos = %v", v.Repos)
					}
					return nil
				},
			},
		},
	})
}

func TestApp_deletedOutsideTerraformIsMadeAgain(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: appConfig(env, name, "")},
			{
				PreConfig: func() { env.fake.RemoveApp(name) },
				Config:    appConfig(env, name, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionCreate)},
				},
			},
		},
	})
}

func TestApp_planTimeValidation(t *testing.T) {
	env := newTestEnv(t)
	name := randName()
	cases := []struct {
		body string
		want string
	}{
		{`  accepts = [{ sender = "tracker", kinds = ["research"], can_decide = true }]`, "can_decide without review-pr"},
		{`  accepts = [
    { sender = "tracker", kinds = ["research"] },
    { sender = "tracker", kinds = ["fix-error"] },
  ]`, "Sender accepted twice"},
		{`  accepts = [{ sender = "tracker", kinds = [] }]`, "at least 1"},
		{`  accepts = [{ sender = "Tracker", kinds = ["research"] }]`, "must be a sender's name"},
		{`  repos = ["example-org/billing", "Example-Org/Billing"]`, "Repeated repositories"},
		{`  repos = ["billing"]`, "must be owner/name"},
		{`  developers = ["Someone@example.com"]`, "must be an email address in lowercase ASCII"},
		{`  developers = ["someoné@example.com"]`, "must be an email address in lowercase ASCII"},
		{`  developers = ["someone"]`, "must be an email address"},
	}
	steps := make([]resource.TestStep, 0, len(cases)+1)
	for _, c := range cases {
		steps = append(steps, resource.TestStep{Config: appConfig(env, name, c.body), PlanOnly: true, ExpectError: expectErr(c.want)})
	}
	steps = append(steps,
		resource.TestStep{Config: appConfig(env, "Billing", ""), PlanOnly: true, ExpectError: expectErr("lowercase letter or digit")},
		resource.TestStep{Config: appConfig(env, strings.Repeat("a", 65), ""), PlanOnly: true, ExpectError: expectErr("at most 64")},
	)
	runTest(t, resource.TestCase{Steps: steps})
}

// A PATCH replaces a list as sent, so changing only the case of a repository
// is a change: planned, sent, and held.
func TestApp_caseOnlyChangeIsApplied(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: appConfig(env, name, `  repos = ["Example-Org/Billing"]`)},
			{
				Config: appConfig(env, name, `  repos = ["example-org/billing"]`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckTypeSetElemAttr(appRes, "repos.*", "example-org/billing"),
					resource.TestCheckResourceAttr(appRes, "lock_version", "2"),
					func(*terraform.State) error {
						if v, _ := env.fake.App(name); !slices.Equal(v.Repos, []string{"example-org/billing"}) {
							return fmt.Errorf("repos = %v", v.Repos)
						}
						return nil
					},
				),
			},
			{
				Config: appConfig(env, name, `  repos = ["example-org/billing"]`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}
