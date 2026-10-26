package provider

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-autopilot/internal/autopilottest"
	"github.com/CruGlobal/terraform-provider-autopilot/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const appRes = "autopilot_app.test"

// envAccRepos optionally names repositories (comma-separated owner/name) the
// live AutoPilot lets an app own. A test AutoPilot limits the repositories an
// app may own (and its tasks may reach), so live tests can't make up their
// own. Without it, the app tests run live without repositories.
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
					resource.TestCheckNoResourceAttr(appRes, "github_logins.#"),
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
	name, dev := randName(), randLogin()
	repos, _ := env.liveRepos(2)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: appConfig(env, name, fmt.Sprintf(`
  repos         = %s
  accepts       = [{ sender = %q, kinds = ["research"] }]
  developers    = [%q]
  github_logins = [{ login = "someone-gh", developer = %q }]`, hcl(repos...), randName(), dev, dev)),
			},
			{
				Config: appConfig(env, name, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(appRes, "repos.#"),
					resource.TestCheckNoResourceAttr(appRes, "accepts.#"),
					resource.TestCheckNoResourceAttr(appRes, "github_logins.#"),
					func(*terraform.State) error {
						v, _ := env.fake.App(name)
						if len(v.Repos)+len(v.Accepts)+len(v.Developers)+len(v.GitHubLogins) != 0 {
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

// can_auto_merge is sent only when true, as can_decide is, so an AutoPilot
// that predates it still takes an entry that leaves it off.
func TestApp_canAutoMergeSentOnlyWhenTrue(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: appConfig(env, name, fmt.Sprintf(`
  accepts = [
    { sender = %q, kinds = ["implement-work-item"], can_auto_merge = false },
    { sender = %q, kinds = ["implement-work-item", "fix-error"], can_auto_merge = true },
  ]`, "aaa-"+randName(), "zzz-"+randName())),
				Check: func(*terraform.State) error {
					post := env.fake.RequestsMatching(http.MethodPost, "/v1/admin/apps")[0]
					accepts, _ := requestBody(t, post)["accepts"].([]any)
					if len(accepts) != 2 {
						return fmt.Errorf("accepts = %v", accepts)
					}
					first, _ := accepts[0].(map[string]any)
					second, _ := accepts[1].(map[string]any)
					if _, ok := first["can_auto_merge"]; ok {
						return fmt.Errorf("can_auto_merge false was sent")
					}
					if second["can_auto_merge"] != true {
						return fmt.Errorf("can_auto_merge true was not sent")
					}
					return nil
				},
			},
		},
	})
}

// can_auto_merge is off unless set, is kept when set, and can be turned off
// again.
func TestApp_canAutoMergeLifecycle(t *testing.T) {
	env := newTestEnv(t)
	name, tracker, monitor := randName(), randName(), randName()
	config := func(on bool) string {
		return appConfig(env, name, fmt.Sprintf(`
  accepts = [
    { sender = %q, kinds = ["implement-work-item", "research"], can_auto_merge = %t },
    { sender = %q, kinds = ["fix-error"] },
  ]`, tracker, on, monitor))
	}
	runTest(t, resource.TestCase{
		CheckDestroy: checkAppsGone(t, env),
		Steps: []resource.TestStep{
			{
				Config: config(true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckTypeSetElemNestedAttrs(appRes, "accepts.*", map[string]string{
						"sender": tracker, "can_auto_merge": "true", "can_decide": "false",
					}),
					resource.TestCheckTypeSetElemNestedAttrs(appRes, "accepts.*", map[string]string{
						"sender": monitor, "kinds.#": "1", "can_decide": "false",
					}),
					resource.TestCheckResourceAttr(appRes, "lock_version", "1"),
					// Left out, it is off at AutoPilot.
					func(*terraform.State) error {
						return env.checkAutoMerge(t, name, map[string]bool{tracker: true, monitor: false})
					},
				),
			},
			{
				Config: config(false),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckTypeSetElemNestedAttrs(appRes, "accepts.*", map[string]string{
						"sender": tracker, "can_auto_merge": "false",
					}),
					resource.TestCheckResourceAttr(appRes, "lock_version", "2"),
					func(*terraform.State) error {
						return env.checkAutoMerge(t, name, map[string]bool{tracker: false, monitor: false})
					},
				),
			},
		},
	})
}

// checkAutoMerge reads the app from AutoPilot, the fake or a live one, and
// checks each sender's can_auto_merge there.
func (env *testEnv) checkAutoMerge(t *testing.T, name string, want map[string]bool) error {
	app, err := env.apiClient(t).GetApp(context.Background(), name)
	if err != nil {
		return err
	}
	for _, a := range app.Accepts {
		if on, ok := want[a.Sender]; ok && a.CanAutoMerge != on {
			return fmt.Errorf("%s: can_auto_merge = %v at AutoPilot, want %v", a.Sender, a.CanAutoMerge, on)
		}
	}
	return nil
}

// can_decide_own_pr is sent only when true, as can_auto_merge is, so an
// AutoPilot that predates it still takes an entry that leaves it off.
func TestApp_canDecideOwnPRSentOnlyWhenTrue(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: appConfig(env, name, `
  accepts = [
    { sender = "autopilot-github", kinds = ["review-pr"], can_decide = true, can_decide_own_pr = false },
    { sender = "autopilot-mcp", kinds = ["review-pr"], can_decide = true, can_decide_own_pr = true },
  ]`),
				Check: func(*terraform.State) error {
					post := env.fake.RequestsMatching(http.MethodPost, "/v1/admin/apps")[0]
					accepts, _ := requestBody(t, post)["accepts"].([]any)
					if len(accepts) != 2 {
						return fmt.Errorf("accepts = %v", accepts)
					}
					first, _ := accepts[0].(map[string]any)
					second, _ := accepts[1].(map[string]any)
					if _, ok := first["can_decide_own_pr"]; ok {
						return fmt.Errorf("can_decide_own_pr false was sent")
					}
					if first["can_decide"] != true {
						return fmt.Errorf("can_decide true was not sent for autopilot-github")
					}
					if second["can_decide_own_pr"] != true {
						return fmt.Errorf("can_decide_own_pr true was not sent")
					}
					return nil
				},
			},
		},
	})
}

// can_decide_own_pr is off unless set, is kept when set, and is turned off
// again by false or by leaving it out. A review from GitHub may decide.
func TestApp_canDecideOwnPRLifecycle(t *testing.T) {
	env := newTestEnv(t)
	name := randName()
	config := func(ownPR string) string {
		return appConfig(env, name, fmt.Sprintf(`
  accepts = [
    { sender = "autopilot-mcp", kinds = ["review-pr"], can_decide = true%s },
    { sender = "autopilot-github", kinds = ["review-pr"], can_decide = true },
  ]`, ownPR))
	}
	runTest(t, resource.TestCase{
		CheckDestroy: checkAppsGone(t, env),
		Steps: []resource.TestStep{
			{
				Config: config(", can_decide_own_pr = true"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckTypeSetElemNestedAttrs(appRes, "accepts.*", map[string]string{
						"sender": "autopilot-mcp", "can_decide": "true", "can_decide_own_pr": "true",
					}),
					resource.TestCheckTypeSetElemNestedAttrs(appRes, "accepts.*", map[string]string{
						"sender": "autopilot-github", "can_decide": "true",
					}),
					resource.TestCheckResourceAttr(appRes, "lock_version", "1"),
					// Left out, it is off at AutoPilot.
					func(*terraform.State) error {
						return env.checkOwnPR(t, name, map[string]bool{"autopilot-mcp": true, "autopilot-github": false})
					},
				),
			},
			{
				Config: config(", can_decide_own_pr = false"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckTypeSetElemNestedAttrs(appRes, "accepts.*", map[string]string{
						"sender": "autopilot-mcp", "can_decide": "true", "can_decide_own_pr": "false",
					}),
					resource.TestCheckResourceAttr(appRes, "lock_version", "2"),
					func(*terraform.State) error {
						return env.checkOwnPR(t, name, map[string]bool{"autopilot-mcp": false, "autopilot-github": false})
					},
				),
			},
			{
				Config: config(", can_decide_own_pr = true"),
				Check: func(*terraform.State) error {
					return env.checkOwnPR(t, name, map[string]bool{"autopilot-mcp": true})
				},
			},
			{
				// Taken out of the configuration, it is turned off.
				Config: config(""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: func(*terraform.State) error {
					return env.checkOwnPR(t, name, map[string]bool{"autopilot-mcp": false, "autopilot-github": false})
				},
			},
		},
	})
}

// checkOwnPR reads the app from AutoPilot, the fake or a live one, and checks
// each sender's can_decide_own_pr there.
func (env *testEnv) checkOwnPR(t *testing.T, name string, want map[string]bool) error {
	app, err := env.apiClient(t).GetApp(context.Background(), name)
	if err != nil {
		return err
	}
	for _, a := range app.Accepts {
		if on, ok := want[a.Sender]; ok && a.CanDecideOwnPR != on {
			return fmt.Errorf("%s: can_decide_own_pr = %v at AutoPilot, want %v", a.Sender, a.CanDecideOwnPR, on)
		}
	}
	return nil
}

// approve_up_to is sent only when set, so an AutoPilot that predates it
// still takes an entry that leaves it out.
func TestApp_approveUpToSentOnlyWhenSet(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: appConfig(env, name, `
  accepts = [
    { sender = "autopilot-github", kinds = ["review-pr"], can_decide = true, approve_up_to = "low" },
    { sender = "autopilot-mcp", kinds = ["review-pr"], can_decide = true },
  ]`),
				Check: func(*terraform.State) error {
					post := env.fake.RequestsMatching(http.MethodPost, "/v1/admin/apps")[0]
					accepts, _ := requestBody(t, post)["accepts"].([]any)
					if len(accepts) != 2 {
						return fmt.Errorf("accepts = %v", accepts)
					}
					first, _ := accepts[0].(map[string]any)
					second, _ := accepts[1].(map[string]any)
					if first["approve_up_to"] != "low" {
						return fmt.Errorf("approve_up_to low was not sent for autopilot-github")
					}
					if _, ok := second["approve_up_to"]; ok {
						return fmt.Errorf("approve_up_to was sent for an entry that leaves it out")
					}
					return nil
				},
			},
		},
	})
}

// approve_up_to is none unless set, is kept when set, changes with the
// configuration, and is taken away by leaving it out.
func TestApp_approveUpToLifecycle(t *testing.T) {
	env := newTestEnv(t)
	name := randName()
	config := func(ceiling string) string {
		return appConfig(env, name, fmt.Sprintf(`
  accepts = [
    { sender = "autopilot-github", kinds = ["review-pr"], can_decide = true%s },
    { sender = "autopilot-mcp", kinds = ["review-pr"], can_decide = true },
  ]`, ceiling))
	}
	runTest(t, resource.TestCase{
		CheckDestroy: checkAppsGone(t, env),
		Steps: []resource.TestStep{
			{
				Config: config(`, approve_up_to = "low"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckTypeSetElemNestedAttrs(appRes, "accepts.*", map[string]string{
						"sender": "autopilot-github", "can_decide": "true", "approve_up_to": "low",
					}),
					resource.TestCheckResourceAttr(appRes, "lock_version", "1"),
					// Left out, there is none at AutoPilot.
					func(*terraform.State) error {
						return env.checkCeiling(t, name, map[string]string{"autopilot-github": "low", "autopilot-mcp": ""})
					},
				),
			},
			{
				Config: config(`, approve_up_to = "medium"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckTypeSetElemNestedAttrs(appRes, "accepts.*", map[string]string{
						"sender": "autopilot-github", "approve_up_to": "medium",
					}),
					resource.TestCheckResourceAttr(appRes, "lock_version", "2"),
					func(*terraform.State) error {
						return env.checkCeiling(t, name, map[string]string{"autopilot-github": "medium"})
					},
				),
			},
			{
				Config: config(`, approve_up_to = "medium"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// Taken out of the configuration, the ceiling is taken away.
				Config: config(""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(appRes, "accepts.0.approve_up_to"),
					resource.TestCheckNoResourceAttr(appRes, "accepts.1.approve_up_to"),
					func(*terraform.State) error {
						return env.checkCeiling(t, name, map[string]string{"autopilot-github": "", "autopilot-mcp": ""})
					},
				),
			},
		},
	})
}

// checkCeiling reads the app from AutoPilot, the fake or a live one, and
// checks each sender's approve_up_to there ("" for none).
func (env *testEnv) checkCeiling(t *testing.T, name string, want map[string]string) error {
	app, err := env.apiClient(t).GetApp(context.Background(), name)
	if err != nil {
		return err
	}
	for _, a := range app.Accepts {
		if ceiling, ok := want[a.Sender]; ok && a.ApproveUpTo != ceiling {
			return fmt.Errorf("%s: approve_up_to = %q at AutoPilot, want %q", a.Sender, a.ApproveUpTo, ceiling)
		}
	}
	return nil
}

// Only the three levels are taken, at plan time, before anything is sent.
func TestApp_approveUpToOnlyALevel(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	for _, bad := range []string{"Low", "none", "", "critical"} {
		runTest(t, resource.TestCase{
			Steps: []resource.TestStep{
				{
					Config: appConfig(env, randName(), fmt.Sprintf(
						`  accepts = [{ sender = "autopilot-github", kinds = ["review-pr"], can_decide = true, approve_up_to = %q }]`, bad)),
					ExpectError: expectErr("value must be one of"),
				},
			},
		})
	}
	if posts := env.fake.RequestsMatching(http.MethodPost, "/v1/admin/apps"); len(posts) != 0 {
		t.Errorf("%d creates were sent", len(posts))
	}
}

// approve_up_to with a can_decide not known at plan time waits for it, and
// the apply refuses it once can_decide is known to be false, before anything
// is sent.
func TestApp_approveUpToWithCanDecideNotKnownYet(t *testing.T) {
	env := newTestEnv(t)
	name := randName()
	config := func(decide bool) string {
		return env.providerConfig() + fmt.Sprintf(`
resource "terraform_data" "decide" {
  input = %t
}

resource "autopilot_app" "test" {
  name    = %q
  accepts = [{ sender = "autopilot-github", kinds = ["review-pr"], can_decide = terraform_data.decide.output, approve_up_to = "low" }]
}
`, decide, name)
	}
	runTest(t, resource.TestCase{
		CheckDestroy: checkAppsGone(t, env),
		Steps: []resource.TestStep{
			{
				Config: config(true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(appRes, "accepts.0.can_decide", "true"),
					resource.TestCheckResourceAttr(appRes, "accepts.0.approve_up_to", "low"),
				),
			},
			{
				Config:      config(false),
				ExpectError: expectErr("approve_up_to without can_decide"),
			},
		},
	})
	if env.fake != nil {
		if patches := env.fake.RequestsMatching(http.MethodPatch, "/v1/admin/apps/"+name); len(patches) != 0 {
			t.Errorf("%d changes were sent", len(patches))
		}
	}
}

// A ceiling raised or taken away outside Terraform shows as a change, and the
// apply puts it back.
func TestApp_approveUpToChangedOutsideTerraformIsPutBack(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	config := appConfig(env, name,
		`  accepts = [{ sender = "autopilot-github", kinds = ["review-pr"], can_decide = true, approve_up_to = "low" }]`)
	for _, outside := range []string{"high", ""} {
		runTest(t, resource.TestCase{
			Steps: []resource.TestStep{
				{Config: config},
				{
					PreConfig: func() {
						env.fake.ChangeAppOutOfBand(name, func(v *autopilottest.AppView) { v.Accepts[0].ApproveUpTo = outside })
					},
					Config: config,
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
					},
					Check: func(*terraform.State) error {
						if v, _ := env.fake.App(name); len(v.Accepts) != 1 || v.Accepts[0].ApproveUpTo != "low" {
							return fmt.Errorf("accepts = %+v, want approve_up_to low again", v.Accepts)
						}
						return nil
					},
				},
			},
		})
	}
}

// An imported app's approve_up_to is read into state, so a configuration that
// sets it plans nothing, and one that leaves it out plans taking it away.
func TestApp_importWithApproveUpTo(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	env.fake.SeedApp(autopilottest.AppView{
		Name: name,
		Accepts: []autopilottest.Accept{
			{Sender: "autopilot-github", Kinds: []string{"review-pr"}, CanDecide: true, ApproveUpTo: "medium"},
		},
	})
	entry := `  accepts = [{ sender = "autopilot-github", kinds = ["review-pr"], can_decide = true%s }]`
	capped := appConfig(env, name, fmt.Sprintf(entry, `, approve_up_to = "medium"`))
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:             capped,
				ResourceName:       appRes,
				ImportState:        true,
				ImportStateId:      name,
				ImportStatePersist: true,
			},
			{
				Config: capped,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				Config: appConfig(env, name, fmt.Sprintf(entry, "")),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: func(*terraform.State) error {
					if v, _ := env.fake.App(name); len(v.Accepts) != 1 || v.Accepts[0].ApproveUpTo != "" || !v.Accepts[0].CanDecide {
						return fmt.Errorf("accepts = %+v, want no ceiling", v.Accepts)
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

// An app this create didn't make is not taken over, even with identical
// values: another configuration may manage it. The apply says to import it,
// and changes nothing.
func TestApp_identicalExistingAppIsRefused(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	env.fake.SeedApp(autopilottest.AppView{Name: name, Developers: []string{"someone@example.com"}})
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      appConfig(env, name, `  developers = ["someone@example.com"]`),
				ExpectError: expectErr("An app with this name already exists ... this apply did not make ... terraform import autopilot_app"),
			},
		},
	})
	if v, _ := env.fake.App(name); v.LockVersion != 1 {
		t.Errorf("the existing app was changed: lock_version %d", v.LockVersion)
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
	name, dev := randName(), randLogin()
	runTest(t, resource.TestCase{
		CheckDestroy: checkAppsGone(t, env),
		Steps: []resource.TestStep{
			{
				Config: appConfig(env, name, fmt.Sprintf(`
  accepts       = [{ sender = %q, kinds = ["review-pr"], can_decide = true }]
  developers    = [%q]
  github_logins = [{ login = %q, developer = %q }]`, randName(), dev, randGitHubLogin(), dev)),
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
		{`  accepts = [{ sender = "tracker", kinds = ["research"], can_auto_merge = true }]`, "can_auto_merge without a pull request kind"},
		{`  accepts = [{ sender = "tracker", kinds = ["review-pr"], can_auto_merge = true }]`, "can_auto_merge without a pull request kind"},
		{`  accepts = [{ sender = "autopilot-mcp", kinds = ["review-pr"], can_decide_own_pr = true }]`, "can_decide_own_pr without can_decide"},
		{`  accepts = [{ sender = "autopilot-github", kinds = ["review-pr"], can_decide = false, can_decide_own_pr = true }]`,
			"can_decide_own_pr without can_decide"},
		{`  accepts = [{ sender = "tracker", kinds = ["review-pr"], can_decide = true, can_decide_own_pr = true }]`,
			"can_decide_own_pr for a sender not built in"},
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
		{`  developers = ["someone@example.com."]`, "must be an email address in lowercase ASCII"},
		{`  developers = ["someone@example..com"]`, "must be an email address in lowercase ASCII"},
		{`  developers = ["some!one@example.com"]`, "must be an email address in lowercase ASCII"},
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

// can_decide with a kind not known at plan time waits for the kind: the plan
// passes, and the apply checks it once it is known.
func TestApp_canDecideWithAKindNotKnownYet(t *testing.T) {
	env := newTestEnv(t)
	name, sender := randName(), randName()
	config := func(kind string) string {
		return env.providerConfig() + fmt.Sprintf(`
resource "terraform_data" "kind" {
  input = %q
}

resource "autopilot_app" "test" {
  name    = %q
  accepts = [{ sender = %q, kinds = [terraform_data.kind.output], can_decide = true }]
}
`, kind, name, sender)
	}
	runTest(t, resource.TestCase{
		CheckDestroy: checkAppsGone(t, env),
		Steps: []resource.TestStep{
			{
				Config: config("review-pr"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(appRes, "accepts.0.can_decide", "true"),
					resource.TestCheckResourceAttr(appRes, "accepts.0.kinds.0", "review-pr"),
				),
			},
			{
				Config:      config("research"),
				ExpectError: expectErr("can_decide without review-pr"),
			},
		},
	})
}

// can_decide_own_pr with a can_decide not known at plan time waits for it:
// the plan passes, and the apply refuses it once can_decide is known to be
// false.
func TestApp_canDecideOwnPRWithCanDecideNotKnownYet(t *testing.T) {
	env := newTestEnv(t)
	name := randName()
	config := func(decide bool) string {
		return env.providerConfig() + fmt.Sprintf(`
resource "terraform_data" "decide" {
  input = %t
}

resource "autopilot_app" "test" {
  name    = %q
  accepts = [{ sender = "autopilot-mcp", kinds = ["review-pr"], can_decide = terraform_data.decide.output, can_decide_own_pr = true }]
}
`, decide, name)
	}
	runTest(t, resource.TestCase{
		CheckDestroy: checkAppsGone(t, env),
		Steps: []resource.TestStep{
			{
				Config: config(true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(appRes, "accepts.0.can_decide", "true"),
					resource.TestCheckResourceAttr(appRes, "accepts.0.can_decide_own_pr", "true"),
				),
			},
			{
				Config:      config(false),
				ExpectError: expectErr("can_decide_own_pr without can_decide"),
			},
		},
	})
}

// can_auto_merge with a kind not known at plan time waits for the kind too.
func TestApp_canAutoMergeWithAKindNotKnownYet(t *testing.T) {
	env := newTestEnv(t)
	name, sender := randName(), randName()
	config := func(kind string) string {
		return env.providerConfig() + fmt.Sprintf(`
resource "terraform_data" "kind" {
  input = %q
}

resource "autopilot_app" "test" {
  name    = %q
  accepts = [{ sender = %q, kinds = [terraform_data.kind.output], can_auto_merge = true }]
}
`, kind, name, sender)
	}
	runTest(t, resource.TestCase{
		CheckDestroy: checkAppsGone(t, env),
		Steps: []resource.TestStep{
			{
				Config: config("fix-error"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(appRes, "accepts.0.can_auto_merge", "true"),
					resource.TestCheckResourceAttr(appRes, "accepts.0.kinds.0", "fix-error"),
				),
			},
			{
				Config:      config("research"),
				ExpectError: expectErr("can_auto_merge without a pull request kind"),
			},
		},
	})
}

// The accepts check skips an entry whose kinds aren't all known, and still
// refuses one whose known kinds leave out review-pr.
func TestAcceptsValidator_UnknownKind(t *testing.T) {
	ctx := context.Background()
	entry := func(kinds ...attr.Value) attr.Value {
		return types.ObjectValueMust(acceptAttrTypes, map[string]attr.Value{
			"sender":            types.StringValue("tracker"),
			"kinds":             types.SetValueMust(types.StringType, kinds),
			"can_decide":        types.BoolValue(true),
			"can_auto_merge":    types.BoolValue(false),
			"can_decide_own_pr": types.BoolNull(),
			"approve_up_to":     types.StringNull(),
		})
	}
	cases := []struct {
		name    string
		entry   attr.Value
		refused bool
	}{
		{"an unknown kind", entry(types.StringUnknown()), false},
		{"an unknown kind beside another", entry(types.StringValue("research"), types.StringUnknown()), false},
		{"review-pr", entry(types.StringValue("review-pr")), false},
		{"no review-pr", entry(types.StringValue("research")), true},
	}
	for _, c := range cases {
		req := validator.SetRequest{
			Path:        path.Root("accepts"),
			ConfigValue: types.SetValueMust(acceptObjectType, []attr.Value{c.entry}),
		}
		var resp validator.SetResponse
		acceptsValidator{}.ValidateSet(ctx, req, &resp)
		if resp.Diagnostics.HasError() != c.refused {
			t.Errorf("%s: refused = %v, want %v (%v)", c.name, resp.Diagnostics.HasError(), c.refused, resp.Diagnostics)
		}
	}
}

// The same for can_auto_merge: an entry whose kinds aren't all known waits,
// and one whose known kinds include neither implement-work-item nor fix-error
// is refused.
func TestAcceptsValidator_AutoMergeUnknownKind(t *testing.T) {
	ctx := context.Background()
	entry := func(kinds ...attr.Value) attr.Value {
		return types.ObjectValueMust(acceptAttrTypes, map[string]attr.Value{
			"sender":            types.StringValue("tracker"),
			"kinds":             types.SetValueMust(types.StringType, kinds),
			"can_decide":        types.BoolValue(false),
			"can_auto_merge":    types.BoolValue(true),
			"can_decide_own_pr": types.BoolNull(),
			"approve_up_to":     types.StringNull(),
		})
	}
	cases := []struct {
		name    string
		entry   attr.Value
		refused bool
	}{
		{"an unknown kind", entry(types.StringUnknown()), false},
		{"an unknown kind beside another", entry(types.StringValue("research"), types.StringUnknown()), false},
		{"implement-work-item", entry(types.StringValue("implement-work-item")), false},
		{"fix-error beside research", entry(types.StringValue("research"), types.StringValue("fix-error")), false},
		{"review-pr only", entry(types.StringValue("review-pr")), true},
		{"research only", entry(types.StringValue("research")), true},
	}
	for _, c := range cases {
		req := validator.SetRequest{
			Path:        path.Root("accepts"),
			ConfigValue: types.SetValueMust(acceptObjectType, []attr.Value{c.entry}),
		}
		var resp validator.SetResponse
		acceptsValidator{}.ValidateSet(ctx, req, &resp)
		if resp.Diagnostics.HasError() != c.refused {
			t.Errorf("%s: refused = %v, want %v (%v)", c.name, resp.Diagnostics.HasError(), c.refused, resp.Diagnostics)
		}
	}
}

// can_decide_own_pr is refused without can_decide (left out is false) and
// for a sender not built into AutoPilot, and waits while either is unknown.
func TestAcceptsValidator_OwnPR(t *testing.T) {
	ctx := context.Background()
	entry := func(sender types.String, canDecide, ownPR types.Bool) attr.Value {
		return types.ObjectValueMust(acceptAttrTypes, map[string]attr.Value{
			"sender":            sender,
			"kinds":             types.SetValueMust(types.StringType, []attr.Value{types.StringValue("review-pr")}),
			"can_decide":        canDecide,
			"can_auto_merge":    types.BoolNull(),
			"can_decide_own_pr": ownPR,
			"approve_up_to":     types.StringNull(),
		})
	}
	mcp, gh, tracker := types.StringValue("autopilot-mcp"), types.StringValue("autopilot-github"), types.StringValue("tracker")
	on, off := types.BoolValue(true), types.BoolValue(false)
	cases := []struct {
		name    string
		entry   attr.Value
		refused bool
	}{
		{"the MCP, with can_decide", entry(mcp, on, on), false},
		{"GitHub, with can_decide", entry(gh, on, on), false},
		{"can_decide left out", entry(mcp, types.BoolNull(), on), true},
		{"can_decide false", entry(gh, off, on), true},
		{"can_decide not known yet", entry(mcp, types.BoolUnknown(), on), false},
		{"a sender in the ledger", entry(tracker, on, on), true},
		{"a sender not known yet", entry(types.StringUnknown(), on, on), false},
		{"off, for a sender in the ledger", entry(tracker, off, off), false},
		{"left out", entry(tracker, off, types.BoolNull()), false},
		{"not known yet", entry(tracker, off, types.BoolUnknown()), false},
	}
	for _, c := range cases {
		req := validator.SetRequest{
			Path:        path.Root("accepts"),
			ConfigValue: types.SetValueMust(acceptObjectType, []attr.Value{c.entry}),
		}
		var resp validator.SetResponse
		acceptsValidator{}.ValidateSet(ctx, req, &resp)
		if resp.Diagnostics.HasError() != c.refused {
			t.Errorf("%s: refused = %v, want %v (%v)", c.name, resp.Diagnostics.HasError(), c.refused, resp.Diagnostics)
		}
	}
}

// A ceiling still unknown at apply is an error, never read as none, the
// widest setting.
func TestAcceptsFromSet_unknownCeiling(t *testing.T) {
	ctx := context.Background()
	entry := func(ceiling types.String) attr.Value {
		return types.ObjectValueMust(acceptAttrTypes, map[string]attr.Value{
			"sender":            types.StringValue("autopilot-github"),
			"kinds":             types.SetValueMust(types.StringType, []attr.Value{types.StringValue("review-pr")}),
			"can_decide":        types.BoolValue(true),
			"can_auto_merge":    types.BoolNull(),
			"can_decide_own_pr": types.BoolNull(),
			"approve_up_to":     ceiling,
		})
	}
	var diags diag.Diagnostics
	got := acceptsFromSet(ctx, types.SetValueMust(acceptObjectType, []attr.Value{entry(types.StringUnknown())}), &diags)
	if !diags.HasError() || got != nil {
		t.Errorf("unknown ceiling: accepts = %+v, diags = %v; want an error and nothing", got, diags)
	}
	diags = nil
	got = acceptsFromSet(ctx, types.SetValueMust(acceptObjectType, []attr.Value{entry(types.StringValue("low"))}), &diags)
	if diags.HasError() || len(got) != 1 || got[0].ApproveUpTo != "low" {
		t.Errorf("known ceiling: accepts = %+v, diags = %v", got, diags)
	}
}

// approve_up_to is refused without can_decide (left out is false), for any
// sender alike, and waits while can_decide is unknown.
func TestAcceptsValidator_ApproveUpTo(t *testing.T) {
	ctx := context.Background()
	entry := func(sender types.String, canDecide types.Bool, ceiling types.String) attr.Value {
		return types.ObjectValueMust(acceptAttrTypes, map[string]attr.Value{
			"sender":            sender,
			"kinds":             types.SetValueMust(types.StringType, []attr.Value{types.StringValue("review-pr")}),
			"can_decide":        canDecide,
			"can_auto_merge":    types.BoolNull(),
			"can_decide_own_pr": types.BoolNull(),
			"approve_up_to":     ceiling,
		})
	}
	gh, tracker := types.StringValue("autopilot-github"), types.StringValue("tracker")
	on, off := types.BoolValue(true), types.BoolValue(false)
	low := types.StringValue("low")
	cases := []struct {
		name    string
		entry   attr.Value
		refused bool
	}{
		{"GitHub, with can_decide", entry(gh, on, low), false},
		{"a sender in the ledger, with can_decide", entry(tracker, on, types.StringValue("high")), false},
		{"can_decide left out", entry(gh, types.BoolNull(), low), true},
		{"can_decide false", entry(tracker, off, low), true},
		{"can_decide not known yet", entry(gh, types.BoolUnknown(), low), false},
		{"left out", entry(tracker, off, types.StringNull()), false},
		{"not known yet", entry(tracker, off, types.StringUnknown()), false},
	}
	for _, c := range cases {
		req := validator.SetRequest{
			Path:        path.Root("accepts"),
			ConfigValue: types.SetValueMust(acceptObjectType, []attr.Value{c.entry}),
		}
		var resp validator.SetResponse
		acceptsValidator{}.ValidateSet(ctx, req, &resp)
		if resp.Diagnostics.HasError() != c.refused {
			t.Errorf("%s: refused = %v, want %v (%v)", c.name, resp.Diagnostics.HasError(), c.refused, resp.Diagnostics)
		}
	}
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

// Removing accepts alone sends only accepts, as []: the app accepts no
// sender, and keeps the rest.
func TestApp_removingAcceptsAlone(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	path := "/v1/admin/apps/" + name
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: appConfig(env, name, fmt.Sprintf(`
  repos      = ["example-org/billing"]
  accepts    = [{ sender = %q, kinds = ["research"] }]
  developers = ["someone@example.com"]`, randName()))},
			{
				Config: appConfig(env, name, `
  repos      = ["example-org/billing"]
  developers = ["someone@example.com"]`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(appRes, "accepts.#"),
					resource.TestCheckResourceAttr(appRes, "repos.#", "1"),
					func(*terraform.State) error {
						patches := env.fake.RequestsMatching(http.MethodPatch, path)
						if len(patches) != 1 {
							return fmt.Errorf("%d changes, want 1", len(patches))
						}
						body := requestBody(t, patches[0])
						if got := keysOf(body); !slices.Equal(got, []string{"accepts"}) {
							return fmt.Errorf("the change sent %v, want only accepts", got)
						}
						if a, _ := body["accepts"].([]any); a == nil || len(a) != 0 {
							return fmt.Errorf("accepts sent as %v, want []", body["accepts"])
						}
						v, _ := env.fake.App(name)
						if len(v.Accepts) != 0 || len(v.Repos) != 1 || len(v.Developers) != 1 {
							return fmt.Errorf("AutoPilot holds %+v", v)
						}
						return nil
					},
				),
			},
		},
	})
}

// accepts = [] is an empty set in state (not null), with no change planned
// after.
func TestApp_acceptsEmptyList(t *testing.T) {
	env := newTestEnv(t)
	name := randName()
	runTest(t, resource.TestCase{
		CheckDestroy: checkAppsGone(t, env),
		Steps: []resource.TestStep{
			{
				Config: appConfig(env, name, `  accepts = []`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(appRes, "accepts.#", "0"),
					func(*terraform.State) error {
						if env.live() {
							return nil
						}
						post := env.fake.RequestsMatching(http.MethodPost, "/v1/admin/apps")[0]
						if got := keysOf(requestBody(t, post)); !slices.Equal(got, []string{"accepts", "name"}) {
							return fmt.Errorf("the create sent %v", got)
						}
						return nil
					},
				),
			},
			{
				Config: appConfig(env, name, `  accepts = []`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// can_decide turned on outside Terraform shows as a change, and the apply
// turns it off again.
func TestApp_canDecideWidenedOutsideTerraformIsPutBack(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name, sender := randName(), randName()
	config := appConfig(env, name, fmt.Sprintf(`  accepts = [{ sender = %q, kinds = ["review-pr"] }]`, sender))
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					env.fake.ChangeAppOutOfBand(name, func(v *autopilottest.AppView) { v.Accepts[0].CanDecide = true })
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: func(*terraform.State) error {
					if v, _ := env.fake.App(name); len(v.Accepts) != 1 || v.Accepts[0].CanDecide {
						return fmt.Errorf("accepts = %+v, want can_decide off again", v.Accepts)
					}
					return nil
				},
			},
		},
	})
}

// can_auto_merge turned on outside Terraform shows as a change, and the apply
// turns it off again.
func TestApp_canAutoMergeWidenedOutsideTerraformIsPutBack(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name, sender := randName(), randName()
	config := appConfig(env, name, fmt.Sprintf(`  accepts = [{ sender = %q, kinds = ["implement-work-item"] }]`, sender))
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					env.fake.ChangeAppOutOfBand(name, func(v *autopilottest.AppView) { v.Accepts[0].CanAutoMerge = true })
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: func(*terraform.State) error {
					if v, _ := env.fake.App(name); len(v.Accepts) != 1 || v.Accepts[0].CanAutoMerge {
						return fmt.Errorf("accepts = %+v, want can_auto_merge off again", v.Accepts)
					}
					return nil
				},
			},
		},
	})
}

// can_decide_own_pr turned on outside Terraform shows as a change, and the
// apply turns it off again.
func TestApp_canDecideOwnPRWidenedOutsideTerraformIsPutBack(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	config := appConfig(env, name, `  accepts = [{ sender = "autopilot-github", kinds = ["review-pr"], can_decide = true }]`)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					env.fake.ChangeAppOutOfBand(name, func(v *autopilottest.AppView) { v.Accepts[0].CanDecideOwnPR = true })
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: func(*terraform.State) error {
					if v, _ := env.fake.App(name); len(v.Accepts) != 1 || v.Accepts[0].CanDecideOwnPR || !v.Accepts[0].CanDecide {
						return fmt.Errorf("accepts = %+v, want can_decide_own_pr off again", v.Accepts)
					}
					return nil
				},
			},
		},
	})
}

// An imported app's empty lists read as unset. A configuration that says []
// plans one update, which changes nothing at AutoPilot, and then nothing.
func TestApp_importThenEmptyLists(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	env.fake.SeedApp(autopilottest.AppView{Name: name})
	config := appConfig(env, name, `
  repos         = []
  accepts       = []
  developers    = []
  github_logins = []`)
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
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(appRes, "repos.#", "0"),
					resource.TestCheckResourceAttr(appRes, "accepts.#", "0"),
					resource.TestCheckResourceAttr(appRes, "developers.#", "0"),
					resource.TestCheckResourceAttr(appRes, "github_logins.#", "0"),
					// Sending [] for what is already empty changes nothing.
					resource.TestCheckResourceAttr(appRes, "lock_version", "1"),
				),
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

// Only AutoPilot's not_found refusal means an app is gone; a plain 404 fails
// the refresh or the delete.
func TestApp_onlyTheNotFoundRefusalMeansGone(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	path := "/v1/admin/apps/" + name
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: appConfig(env, name, "")},
			{
				PreConfig: func() {
					env.fake.RespondNext(http.MethodGet, path, http.StatusNotFound, "text/plain", "404 page not found")
				},
				Config:      appConfig(env, name, ""),
				ExpectError: expectErr("Error reading AutoPilot app ... this 404 is not one"),
			},
			{
				PreConfig: func() {
					env.fake.RespondNext(http.MethodDelete, path, http.StatusNotFound, "text/plain", "404 page not found")
				},
				Config:      appConfig(env, name, ""),
				Destroy:     true,
				ExpectError: expectErr("Error deleting AutoPilot app ... this 404 is not one"),
			},
			{
				PreConfig: func() {
					env.fake.OnNextRequest(http.MethodDelete, path, func() { env.fake.RemoveApp(name) })
				},
				Config:  appConfig(env, name, ""),
				Destroy: true,
			},
		},
	})
}

// lastAppPatch checks the body of the last change sent for an app, on the
// fake.
func lastAppPatch(t *testing.T, env *testEnv, name string, want map[string]any) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if env.live() {
			return nil
		}
		patches := env.fake.RequestsMatching(http.MethodPatch, "/v1/admin/apps/"+name)
		if len(patches) == 0 {
			return fmt.Errorf("no change was sent")
		}
		if body := requestBody(t, patches[len(patches)-1]); !reflect.DeepEqual(body, want) {
			return fmt.Errorf("the change sent %v, want %v", body, want)
		}
		return nil
	}
}

// An app's credential policy is set, changed and taken away. Its two
// arguments are one policy, so a change to either sends both, and removing
// it sends credential null.
func TestApp_credentialPolicy(t *testing.T) {
	env := newTestEnv(t)
	name := randName()
	runTest(t, resource.TestCase{
		CheckDestroy: checkAppsGone(t, env),
		Steps: []resource.TestStep{
			{
				Config: appConfig(env, name, `
  credential      = "seat_first"
  when_seats_full = "wait"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(appRes, "credential", "seat_first"),
					resource.TestCheckResourceAttr(appRes, "when_seats_full", "wait"),
					func(*terraform.State) error {
						if env.live() {
							return nil
						}
						post := env.fake.RequestsMatching(http.MethodPost, "/v1/admin/apps")[0]
						body := requestBody(t, post)
						if body["credential"] != "seat_first" || body["when_seats_full"] != "wait" {
							return fmt.Errorf("the create sent %v", body)
						}
						return nil
					},
				),
			},
			{
				Config: appConfig(env, name, `
  credential      = "seat_first"
  when_seats_full = "wait"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				Config: appConfig(env, name, `
  credential      = "seat_first"
  when_seats_full = "fall_back"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(appRes, "when_seats_full", "fall_back"),
					resource.TestCheckResourceAttr(appRes, "lock_version", "2"),
					lastAppPatch(t, env, name, map[string]any{"credential": "seat_first", "when_seats_full": "fall_back"}),
				),
			},
			{
				Config: appConfig(env, name, `  credential = "bedrock"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(appRes, "credential", "bedrock"),
					resource.TestCheckNoResourceAttr(appRes, "when_seats_full"),
					lastAppPatch(t, env, name, map[string]any{"credential": "bedrock"}),
				),
			},
			{
				Config: appConfig(env, name, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(appRes, "credential"),
					resource.TestCheckNoResourceAttr(appRes, "when_seats_full"),
					resource.TestCheckResourceAttr(appRes, "lock_version", "4"),
					lastAppPatch(t, env, name, map[string]any{"credential": nil}),
					func(*terraform.State) error {
						if env.live() {
							return nil
						}
						if v, _ := env.fake.App(name); v.Credential != "" || v.WhenSeatsFull != "" {
							return fmt.Errorf("AutoPilot still holds %q, %q", v.Credential, v.WhenSeatsFull)
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

// A change to another argument leaves the credential policy out, so the
// app keeps it.
func TestApp_credentialPolicyNotSentWhenUnchanged(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	policy := `
  credential      = "seat_first"
  when_seats_full = "fall_back"`
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: appConfig(env, name, policy)},
			{
				Config: appConfig(env, name, policy+`
  developers = ["someone@example.com"]`),
				Check: resource.ComposeAggregateTestCheckFunc(
					lastAppPatch(t, env, name, map[string]any{"developers": []any{"someone@example.com"}}),
					func(*terraform.State) error {
						if v, _ := env.fake.App(name); v.Credential != "seat_first" || v.WhenSeatsFull != "fall_back" {
							return fmt.Errorf("AutoPilot holds %q, %q", v.Credential, v.WhenSeatsFull)
						}
						return nil
					},
				),
			},
		},
	})
}

// A credential policy changed outside Terraform shows as a change, and the
// apply puts the configured one back; one set outside Terraform on an app
// that configures none is taken away.
func TestApp_credentialPolicyChangedOutsideTerraformIsPutBack(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name, other := randName(), randName()
	policy := `
  credential      = "seat_first"
  when_seats_full = "fall_back"`
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: appConfig(env, name, policy)},
			{
				PreConfig: func() {
					env.fake.ChangeAppOutOfBand(name, func(v *autopilottest.AppView) {
						v.Credential, v.WhenSeatsFull = "bedrock", ""
					})
				},
				Config: appConfig(env, name, policy),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: func(*terraform.State) error {
					if v, _ := env.fake.App(name); v.Credential != "seat_first" || v.WhenSeatsFull != "fall_back" {
						return fmt.Errorf("AutoPilot holds %q, %q", v.Credential, v.WhenSeatsFull)
					}
					return nil
				},
			},
		},
	})
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: appConfig(env, other, "")},
			{
				PreConfig: func() {
					env.fake.ChangeAppOutOfBand(other, func(v *autopilottest.AppView) {
						v.Credential, v.WhenSeatsFull = "seat_first", "wait"
					})
				},
				Config: appConfig(env, other, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					lastAppPatch(t, env, other, map[string]any{"credential": nil}),
					func(*terraform.State) error {
						if v, _ := env.fake.App(other); v.Credential != "" || v.WhenSeatsFull != "" {
							return fmt.Errorf("AutoPilot still holds %q, %q", v.Credential, v.WhenSeatsFull)
						}
						return nil
					},
				),
			},
		},
	})
}

// An imported app's credential policy is read into state, so a
// configuration that sets the same plans nothing.
func TestApp_importWithCredentialPolicy(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	env.fake.SeedApp(autopilottest.AppView{Name: name, Credential: "seat_first", WhenSeatsFull: "fall_back"})
	config := appConfig(env, name, `
  credential      = "seat_first"
  when_seats_full = "fall_back"`)
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

// The credential policy's rules fail the plan.
func TestApp_credentialPolicyPlanTimeValidation(t *testing.T) {
	env := newTestEnv(t)
	name := randName()
	cases := []struct {
		body string
		want string
	}{
		{`  credential = "seat_first"`, "when_seats_full is required with seat_first"},
		{`  when_seats_full = "wait"`, "when_seats_full only with seat_first"},
		{`
  credential      = "bedrock"
  when_seats_full = "fall_back"`, "when_seats_full only with seat_first"},
		{`  credential = "seats"`, "value must be one of"},
		{`  credential = "Bedrock"`, "value must be one of"},
		{`
  credential      = "seat_first"
  when_seats_full = "later"`, "value must be one of"},
	}
	steps := make([]resource.TestStep, 0, len(cases))
	for _, c := range cases {
		steps = append(steps, resource.TestStep{Config: appConfig(env, name, c.body), PlanOnly: true, ExpectError: expectErr(c.want)})
	}
	runTest(t, resource.TestCase{Steps: steps})
}

// A credential policy that is somehow still unknown at apply is an error,
// never sent as none: that would take the app's own policy away.
func TestUnknownCredentialPolicyIsAnError(t *testing.T) {
	var diags diag.Diagnostics
	m := appModel{Credential: types.StringUnknown(), WhenSeatsFull: types.StringNull()}
	if got := policyFromModel(m, &diags); got != nil || !diags.HasError() {
		t.Errorf("unknown credential gave %v, %v", got, diags)
	}
	diags = nil
	m = appModel{Credential: types.StringValue("seat_first"), WhenSeatsFull: types.StringUnknown()}
	if got := policyFromModel(m, &diags); got != nil || !diags.HasError() {
		t.Errorf("unknown when_seats_full gave %v, %v", got, diags)
	}
	diags = nil
	m = appModel{Credential: types.StringNull(), WhenSeatsFull: types.StringNull()}
	if got := policyFromModel(m, &diags); got == nil || got.Credential != "" || diags.HasError() {
		t.Errorf("no policy gave %v, %v; it takes the app's own away", got, diags)
	}
}

// A list that is somehow still unknown at apply is an error, never sent as
// []: an empty accepts or repos sent by mistake would take away the app's
// consent or its repositories.
func TestUnknownListIsAnErrorNotEmpty(t *testing.T) {
	ctx := context.Background()
	var diags diag.Diagnostics
	if got := acceptsFromSet(ctx, types.SetUnknown(acceptObjectType), &diags); got != nil || !diags.HasError() {
		t.Errorf("unknown accepts gave %v, %v", got, diags)
	}
	diags = nil
	if got := stringsFromSet(ctx, types.SetUnknown(types.StringType), "repos", &diags); got != nil || !diags.HasError() {
		t.Errorf("unknown repos gave %v, %v", got, diags)
	}
	diags = nil
	if got := stringsFromSet(ctx, types.SetNull(types.StringType), "repos", &diags); got != nil || diags.HasError() {
		t.Errorf("null repos gave %v, %v; null is simply not sent", got, diags)
	}
}

// An app's worker image and size are set, changed and taken away. The size's
// two arguments are one size, so a change to either sends both, and removing
// them sends both as null.
func TestApp_worker(t *testing.T) {
	env := newTestEnv(t)
	name := randName()
	big := `
  worker_image  = "ruby"
  worker_cpu    = 4096
  worker_memory = 16384`
	runTest(t, resource.TestCase{
		CheckDestroy: checkAppsGone(t, env),
		Steps: []resource.TestStep{
			{
				Config: appConfig(env, name, big),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(appRes, "worker_image", "ruby"),
					resource.TestCheckResourceAttr(appRes, "worker_cpu", "4096"),
					resource.TestCheckResourceAttr(appRes, "worker_memory", "16384"),
					func(*terraform.State) error {
						if env.live() {
							return nil
						}
						post := env.fake.RequestsMatching(http.MethodPost, "/v1/admin/apps")[0]
						body := requestBody(t, post)
						if body["worker_image"] != "ruby" || body["worker_cpu"] != float64(4096) || body["worker_memory"] != float64(16384) {
							return fmt.Errorf("the create sent %v", body)
						}
						return nil
					},
				),
			},
			{
				Config: appConfig(env, name, big),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// The same CPU with less memory still sends both.
				Config: appConfig(env, name, `
  worker_image  = "ruby"
  worker_cpu    = 4096
  worker_memory = 8192`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(appRes, "worker_memory", "8192"),
					resource.TestCheckResourceAttr(appRes, "lock_version", "2"),
					lastAppPatch(t, env, name, map[string]any{"worker_cpu": float64(4096), "worker_memory": float64(8192)}),
				),
			},
			{
				// The opt-in image with a browser, and the image alone is sent.
				Config: appConfig(env, name, `
  worker_image  = "ruby-browser"
  worker_cpu    = 4096
  worker_memory = 8192`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(appRes, "worker_image", "ruby-browser"),
					resource.TestCheckResourceAttr(appRes, "worker_memory", "8192"),
					resource.TestCheckResourceAttr(appRes, "lock_version", "3"),
					lastAppPatch(t, env, name, map[string]any{"worker_image": "ruby-browser"}),
				),
			},
			{
				Config: appConfig(env, name, `  worker_image = "go"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(appRes, "worker_image", "go"),
					resource.TestCheckNoResourceAttr(appRes, "worker_cpu"),
					resource.TestCheckNoResourceAttr(appRes, "worker_memory"),
					lastAppPatch(t, env, name, map[string]any{"worker_image": "go", "worker_cpu": nil, "worker_memory": nil}),
				),
			},
			{
				Config: appConfig(env, name, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(appRes, "worker_image"),
					resource.TestCheckResourceAttr(appRes, "lock_version", "5"),
					lastAppPatch(t, env, name, map[string]any{"worker_image": nil}),
					func(*terraform.State) error {
						if env.live() {
							return nil
						}
						if v, _ := env.fake.App(name); v.WorkerImage != "" || v.WorkerCPU != 0 || v.WorkerMemory != 0 {
							return fmt.Errorf("AutoPilot still holds %q, %d, %d", v.WorkerImage, v.WorkerCPU, v.WorkerMemory)
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

// A change to another argument leaves the worker out, so the app keeps it.
func TestApp_workerNotSentWhenUnchanged(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	worker := `
  worker_image  = "python"
  worker_cpu    = 1024
  worker_memory = 4096`
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: appConfig(env, name, worker)},
			{
				Config: appConfig(env, name, worker+`
  developers = ["someone@example.com"]`),
				Check: resource.ComposeAggregateTestCheckFunc(
					lastAppPatch(t, env, name, map[string]any{"developers": []any{"someone@example.com"}}),
					func(*terraform.State) error {
						if v, _ := env.fake.App(name); v.WorkerImage != "python" || v.WorkerCPU != 1024 || v.WorkerMemory != 4096 {
							return fmt.Errorf("AutoPilot holds %q, %d, %d", v.WorkerImage, v.WorkerCPU, v.WorkerMemory)
						}
						return nil
					},
				),
			},
		},
	})
}

// A worker changed outside Terraform shows as a change, and the apply puts
// the configured one back; one set outside Terraform on an app that
// configures none is taken away.
func TestApp_workerChangedOutsideTerraformIsPutBack(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name, other := randName(), randName()
	worker := `
  worker_image  = "ruby"
  worker_cpu    = 2048
  worker_memory = 16384`
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: appConfig(env, name, worker)},
			{
				PreConfig: func() {
					env.fake.ChangeAppOutOfBand(name, func(v *autopilottest.AppView) {
						v.WorkerImage, v.WorkerCPU, v.WorkerMemory = "node", 2048, 8192
					})
				},
				Config: appConfig(env, name, worker),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					lastAppPatch(t, env, name, map[string]any{
						"worker_image": "ruby", "worker_cpu": float64(2048), "worker_memory": float64(16384),
					}),
					func(*terraform.State) error {
						if v, _ := env.fake.App(name); v.WorkerImage != "ruby" || v.WorkerCPU != 2048 || v.WorkerMemory != 16384 {
							return fmt.Errorf("AutoPilot holds %q, %d, %d", v.WorkerImage, v.WorkerCPU, v.WorkerMemory)
						}
						return nil
					},
				),
			},
		},
	})
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: appConfig(env, other, "")},
			{
				PreConfig: func() {
					env.fake.ChangeAppOutOfBand(other, func(v *autopilottest.AppView) {
						v.WorkerImage, v.WorkerCPU, v.WorkerMemory = "go", 4096, 16384
					})
				},
				Config: appConfig(env, other, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					lastAppPatch(t, env, other, map[string]any{"worker_image": nil, "worker_cpu": nil, "worker_memory": nil}),
					func(*terraform.State) error {
						if v, _ := env.fake.App(other); v.WorkerImage != "" || v.WorkerCPU != 0 || v.WorkerMemory != 0 {
							return fmt.Errorf("AutoPilot still holds %q, %d, %d", v.WorkerImage, v.WorkerCPU, v.WorkerMemory)
						}
						return nil
					},
				),
			},
		},
	})
}

// An imported app's worker is read into state, so a configuration that sets
// the same plans nothing.
func TestApp_importWithWorker(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	env.fake.SeedApp(autopilottest.AppView{Name: name, WorkerImage: "ruby", WorkerCPU: 4096, WorkerMemory: 12288})
	config := appConfig(env, name, `
  worker_image  = "ruby"
  worker_cpu    = 4096
  worker_memory = 12288`)
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

// The worker's rules fail the plan.
func TestApp_workerPlanTimeValidation(t *testing.T) {
	env := newTestEnv(t)
	name := randName()
	cases := []struct {
		body string
		want string
	}{
		{`  worker_image = "rust"`, "value must be one of"},
		{`  worker_image = "Node"`, "value must be one of"},
		{`  worker_image = "ruby_browser"`, "value must be one of"},
		{`  worker_cpu = 4096`, "worker_memory is required with worker_cpu"},
		{`  worker_memory = 16384`, "worker_cpu is required with worker_memory"},
		{`
  worker_cpu    = 8192
  worker_memory = 16384`, "value must be one of"},
		{`
  worker_cpu    = 3072
  worker_memory = 8192`, "value must be one of"},
		{`
  worker_cpu    = 4096
  worker_memory = 30720`, "value must be between 512 and 16384"},
		{`
  worker_cpu    = 4096
  worker_memory = 4096`, "worker_memory doesn't go with worker_cpu"},
		{`
  worker_cpu    = 2048
  worker_memory = 8000`, "worker_memory doesn't go with worker_cpu"},
		{`
  worker_cpu    = 256
  worker_memory = 4096`, "worker_memory doesn't go with worker_cpu"},
	}
	steps := make([]resource.TestStep, 0, len(cases))
	for _, c := range cases {
		steps = append(steps, resource.TestStep{Config: appConfig(env, name, c.body), PlanOnly: true, ExpectError: expectErr(c.want)})
	}
	runTest(t, resource.TestCase{Steps: steps})
}

// Every size the plan allows is one the fake, like AutoPilot, takes, and
// every one it refuses, the fake refuses too.
func TestWorkerSizes_MatchFargate(t *testing.T) {
	allowed := 0
	for _, cpu := range []int64{128, 256, 512, 1024, 2048, 3072, 4096, 8192} {
		for memory := int64(256); memory <= 32768; memory += 256 {
			var fargate bool
			switch cpu {
			case 256:
				fargate = memory == 512 || memory == 1024 || memory == 2048
			case 512:
				fargate = memory >= 1024 && memory <= 4096 && memory%1024 == 0
			case 1024:
				fargate = memory >= 2048 && memory <= 8192 && memory%1024 == 0
			case 2048:
				fargate = memory >= 4096 && memory <= 16384 && memory%1024 == 0
			case 4096:
				fargate = memory >= 8192 && memory <= 30720 && memory%1024 == 0
			case 8192:
				fargate = memory >= 16384 && memory <= 61440 && memory%4096 == 0
			}
			want := fargate && cpu <= 4096 && memory <= 16384
			if got := slices.Contains(workerSizes[cpu], memory); got != want {
				t.Errorf("%d/%d: allowed = %v, want %v", cpu, memory, got, want)
			}
			if want {
				allowed++
			}
		}
	}
	if allowed != 36 {
		t.Errorf("%d sizes allowed, want 36", allowed)
	}
}

// A worker that is somehow still unknown at apply is an error, never sent as
// none: that would take the app's image or size away.
func TestUnknownWorkerIsAnError(t *testing.T) {
	var diags diag.Diagnostics
	if got := imageFromModel(appModel{WorkerImage: types.StringUnknown()}, &diags); got != nil || !diags.HasError() {
		t.Errorf("unknown worker_image gave %v, %v", got, diags)
	}
	diags = nil
	m := appModel{WorkerCPU: types.Int64Unknown(), WorkerMemory: types.Int64Value(8192)}
	if got := sizeFromModel(m, &diags); got != nil || !diags.HasError() {
		t.Errorf("unknown worker_cpu gave %v, %v", got, diags)
	}
	diags = nil
	m = appModel{WorkerCPU: types.Int64Value(2048), WorkerMemory: types.Int64Unknown()}
	if got := sizeFromModel(m, &diags); got != nil || !diags.HasError() {
		t.Errorf("unknown worker_memory gave %v, %v", got, diags)
	}
	diags = nil
	m = appModel{WorkerImage: types.StringNull(), WorkerCPU: types.Int64Null(), WorkerMemory: types.Int64Null()}
	if got := imageFromModel(m, &diags); got == nil || *got != "" || diags.HasError() {
		t.Errorf("no image gave %v, %v; it takes the app back to node", got, diags)
	}
	if got := sizeFromModel(m, &diags); got == nil || *got != (client.WorkerSize{}) || diags.HasError() {
		t.Errorf("no size gave %v, %v; it takes the app's size away", got, diags)
	}
}

// randGitHubLogin is a GitHub login with a random suffix, so live runs never
// collide.
func randGitHubLogin() string {
	return "tfacc-" + acctest.RandStringFromCharSet(10, "abcdefghijklmnopqrstuvwxyz0123456789")
}

// gitHubLoginsHCL renders pairs (login, developer, login, developer, ...) as
// a github_logins argument.
func gitHubLoginsHCL(pairs ...string) string {
	entries := make([]string, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		entries = append(entries, fmt.Sprintf("    { login = %q, developer = %q },", pairs[i], pairs[i+1]))
	}
	return "  github_logins = [\n" + strings.Join(entries, "\n") + "\n  ]"
}

// An app's GitHub logins are set, changed with its developers in one write,
// and taken away.
func TestApp_gitHubLogins(t *testing.T) {
	env := newTestEnv(t)
	name := randName()
	devA, devB := randLogin(), randLogin()
	loginA, loginB, loginC := randGitHubLogin(), randGitHubLogin(), randGitHubLogin()
	accepts := `  accepts = [{ sender = "autopilot-github", kinds = ["review-pr"] }]`
	first := accepts + "\n  developers = " + hcl(devA, devB) + "\n" + gitHubLoginsHCL(loginA, devA, loginB, devB)
	// devB leaves, with their login, and devA gains a second login.
	second := accepts + "\n  developers = " + hcl(devA) + "\n" + gitHubLoginsHCL(loginA, devA, loginC, devA)
	third := accepts + "\n  developers = " + hcl(devA)
	sortedPairs := func(pairs ...string) []any {
		type pair struct{ login, developer string }
		var ps []pair
		for i := 0; i+1 < len(pairs); i += 2 {
			ps = append(ps, pair{pairs[i], pairs[i+1]})
		}
		slices.SortFunc(ps, func(x, y pair) int { return strings.Compare(x.login, y.login) })
		out := []any{}
		for _, p := range ps {
			out = append(out, map[string]any{"login": p.login, "developer": p.developer})
		}
		return out
	}

	runTest(t, resource.TestCase{
		CheckDestroy: checkAppsGone(t, env),
		Steps: []resource.TestStep{
			{
				Config: appConfig(env, name, first),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(appRes, "github_logins.#", "2"),
					resource.TestCheckTypeSetElemNestedAttrs(appRes, "github_logins.*", map[string]string{"login": loginA, "developer": devA}),
					resource.TestCheckTypeSetElemNestedAttrs(appRes, "github_logins.*", map[string]string{"login": loginB, "developer": devB}),
					func(*terraform.State) error {
						if env.live() {
							return nil
						}
						post := env.fake.RequestsMatching(http.MethodPost, "/v1/admin/apps")[0]
						if got := requestBody(t, post)["github_logins"]; !reflect.DeepEqual(got, sortedPairs(loginA, devA, loginB, devB)) {
							return fmt.Errorf("the create sent github_logins %v", got)
						}
						return nil
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
				Config: appConfig(env, name, second),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(appRes, "github_logins.#", "2"),
					resource.TestCheckTypeSetElemNestedAttrs(appRes, "github_logins.*", map[string]string{"login": loginC, "developer": devA}),
					resource.TestCheckResourceAttr(appRes, "lock_version", "2"),
					lastAppPatch(t, env, name, map[string]any{
						"developers":    []any{devA},
						"github_logins": sortedPairs(loginA, devA, loginC, devA),
					}),
				),
			},
			{
				Config: appConfig(env, name, second),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// Removing github_logins empties it, and leaves the developers.
				Config: appConfig(env, name, third),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(appRes, "github_logins.#"),
					resource.TestCheckResourceAttr(appRes, "developers.#", "1"),
					lastAppPatch(t, env, name, map[string]any{"github_logins": []any{}}),
				),
			},
			{
				Config: appConfig(env, name, third),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// A change to another argument leaves github_logins out, so the app keeps
// them; and an app that never sets them never sends them.
func TestApp_gitHubLoginsNotSentWhenUnchanged(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	logins := `
  developers = ["alice@example.org"]
` + gitHubLoginsHCL("alice-gh", "alice@example.org")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: appConfig(env, name, logins)},
			{
				Config: appConfig(env, name, logins+`
  worker_image = "go"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					lastAppPatch(t, env, name, map[string]any{"worker_image": "go"}),
					func(*terraform.State) error {
						want := []autopilottest.GitHubLogin{{Login: "alice-gh", Developer: "alice@example.org"}}
						if v, _ := env.fake.App(name); !slices.Equal(v.GitHubLogins, want) {
							return fmt.Errorf("AutoPilot holds %v", v.GitHubLogins)
						}
						return nil
					},
				),
			},
		},
	})
}

// GitHub logins changed outside Terraform show as a change, and the apply
// puts the configured ones back; ones set outside Terraform on an app that
// configures none are taken away.
func TestApp_gitHubLoginsChangedOutsideTerraformIsPutBack(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name, other := randName(), randName()
	config := `
  developers = ["alice@example.org", "bob@example.org"]
` + gitHubLoginsHCL("alice-gh", "alice@example.org")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: appConfig(env, name, config)},
			{
				PreConfig: func() {
					env.fake.ChangeAppOutOfBand(name, func(v *autopilottest.AppView) {
						// The login now starts reviews in someone else's name.
						v.GitHubLogins = []autopilottest.GitHubLogin{{Login: "alice-gh", Developer: "bob@example.org"}}
					})
				},
				Config: appConfig(env, name, config),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					lastAppPatch(t, env, name, map[string]any{"github_logins": []any{
						map[string]any{"login": "alice-gh", "developer": "alice@example.org"},
					}}),
					func(*terraform.State) error {
						want := []autopilottest.GitHubLogin{{Login: "alice-gh", Developer: "alice@example.org"}}
						if v, _ := env.fake.App(name); !slices.Equal(v.GitHubLogins, want) {
							return fmt.Errorf("AutoPilot holds %v", v.GitHubLogins)
						}
						return nil
					},
				),
			},
		},
	})
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: appConfig(env, other, `  developers = ["alice@example.org"]`)},
			{
				PreConfig: func() {
					env.fake.ChangeAppOutOfBand(other, func(v *autopilottest.AppView) {
						v.GitHubLogins = []autopilottest.GitHubLogin{{Login: "someone-else", Developer: "alice@example.org"}}
					})
				},
				Config: appConfig(env, other, `  developers = ["alice@example.org"]`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(appRes, "github_logins.#"),
					lastAppPatch(t, env, other, map[string]any{"github_logins": []any{}}),
					func(*terraform.State) error {
						if v, _ := env.fake.App(other); len(v.GitHubLogins) != 0 {
							return fmt.Errorf("AutoPilot still holds %v", v.GitHubLogins)
						}
						return nil
					},
				),
			},
		},
	})
}

// An imported app's GitHub logins are read into state, so a configuration
// that sets the same plans nothing.
func TestApp_importWithGitHubLogins(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	env.fake.SeedApp(autopilottest.AppView{
		Name:       name,
		Developers: []string{"alice@example.org"},
		GitHubLogins: []autopilottest.GitHubLogin{
			{Login: "alice-gh", Developer: "alice@example.org"},
			{Login: "alice_work", Developer: "alice@example.org"},
		},
	})
	config := appConfig(env, name, `
  developers = ["alice@example.org"]
`+gitHubLoginsHCL("alice_work", "alice@example.org", "alice-gh", "alice@example.org"))
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

// An imported app's can_decide_own_pr is read into state, so a
// configuration that sets it plans nothing, and one that leaves it out plans
// turning it off.
func TestApp_importWithCanDecideOwnPR(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	name := randName()
	env.fake.SeedApp(autopilottest.AppView{
		Name: name,
		Accepts: []autopilottest.Accept{
			{Sender: "autopilot-mcp", Kinds: []string{"review-pr"}, CanDecide: true, CanDecideOwnPR: true},
		},
	})
	entry := `  accepts = [{ sender = "autopilot-mcp", kinds = ["review-pr"], can_decide = true%s }]`
	on := appConfig(env, name, fmt.Sprintf(entry, ", can_decide_own_pr = true"))
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:             on,
				ResourceName:       appRes,
				ImportState:        true,
				ImportStateId:      name,
				ImportStatePersist: true,
			},
			{
				Config: on,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				Config: appConfig(env, name, fmt.Sprintf(entry, "")),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(appRes, plancheck.ResourceActionUpdate)},
				},
				Check: func(*terraform.State) error {
					if v, _ := env.fake.App(name); len(v.Accepts) != 1 || v.Accepts[0].CanDecideOwnPR || !v.Accepts[0].CanDecide {
						return fmt.Errorf("accepts = %+v, want can_decide_own_pr off", v.Accepts)
					}
					return nil
				},
			},
		},
	})
}

// The rules for GitHub logins fail the plan.
func TestApp_gitHubLoginsPlanTimeValidation(t *testing.T) {
	env := newTestEnv(t)
	name := randName()
	devs := `  developers = ["alice@example.org", "bob@example.org"]
`
	cases := []struct {
		body string
		want string
	}{
		{devs + gitHubLoginsHCL("Alice-GH", "alice@example.org"), "must be a GitHub login in lowercase"},
		{devs + gitHubLoginsHCL("-alice", "alice@example.org"), "must be a GitHub login in lowercase"},
		{devs + gitHubLoginsHCL("_alice", "alice@example.org"), "must be a GitHub login in lowercase"},
		{devs + gitHubLoginsHCL("alice.gh", "alice@example.org"), "must be a GitHub login in lowercase"},
		{devs + gitHubLoginsHCL("alice@example.org", "alice@example.org"), "must be a GitHub login in lowercase"},
		{devs + gitHubLoginsHCL(strings.Repeat("a", 40), "alice@example.org"), "must be a GitHub login in lowercase"},
		{devs + gitHubLoginsHCL("", "alice@example.org"), "must be a GitHub login in lowercase"},
		{devs + gitHubLoginsHCL("alice-gh", "Alice@example.org"), "must be an email address in lowercase ASCII"},
		{devs + gitHubLoginsHCL("alice-gh", "alice"), "must be an email address in lowercase ASCII"},
		{devs + gitHubLoginsHCL("alice-gh", "alice@example.org", "alice-gh", "bob@example.org"), "GitHub login listed twice"},
		{devs + gitHubLoginsHCL("carol-gh", "carol@example.org"), "GitHub login for someone not in developers"},
		{gitHubLoginsHCL("alice-gh", "alice@example.org"), "GitHub login for someone not in developers"},
		{`  developers = []
` + gitHubLoginsHCL("alice-gh", "alice@example.org"), "GitHub login for someone not in developers"},
	}
	steps := make([]resource.TestStep, 0, len(cases)+2)
	for _, c := range cases {
		steps = append(steps, resource.TestStep{Config: appConfig(env, name, c.body), PlanOnly: true, ExpectError: expectErr(c.want)})
	}
	many := make([]string, 0, 202)
	for i := range 101 {
		many = append(many, fmt.Sprintf("login-%d", i), "alice@example.org")
	}
	steps = append(steps, resource.TestStep{Config: appConfig(env, name, devs+gitHubLoginsHCL(many...)), PlanOnly: true,
		ExpectError: expectErr("must contain at most 100 elements")})
	// What AutoPilot takes, the plan takes: a 39-character login, one that
	// starts with a digit, _ inside one, a developer with two logins, and
	// reviews from GitHub and the MCP that may decide, on the asker's own pull
	// request too. The plan is for a new app, so it is not empty.
	steps = append(steps, resource.TestStep{
		Config: appConfig(env, name, devs+`  accepts = [
    { sender = "autopilot-github", kinds = ["review-pr"], can_decide = true },
    { sender = "autopilot-mcp", kinds = ["review-pr"], can_decide = true, can_decide_own_pr = true },
  ]
`+gitHubLoginsHCL(strings.Repeat("a", 39), "alice@example.org", "0alice_work", "alice@example.org", "bob", "bob@example.org")),
		PlanOnly:           true,
		ExpectNonEmptyPlan: true,
	})
	runTest(t, resource.TestCase{Steps: steps})
}

// A developer not known at plan time waits: the plan passes, and the apply
// checks the pair once the developer is known.
func TestApp_gitHubLoginsWithADeveloperNotKnownYet(t *testing.T) {
	env := newTestEnv(t)
	name, login := randName(), randGitHubLogin()
	dev, other := randLogin(), randLogin()
	config := func(developer, paired string) string {
		return env.providerConfig() + fmt.Sprintf(`
resource "terraform_data" "dev" {
  input = %q
}

resource "autopilot_app" "test" {
  name          = %q
  developers    = [terraform_data.dev.output]
  github_logins = [{ login = %q, developer = %q }]
}
`, developer, name, login, paired)
	}
	runTest(t, resource.TestCase{
		CheckDestroy: checkAppsGone(t, env),
		Steps: []resource.TestStep{
			{
				Config: config(dev, dev),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(appRes, "developers.0", dev),
					resource.TestCheckResourceAttr(appRes, "github_logins.0.developer", dev),
				),
			},
			{
				// The new developer is unknown until terraform_data is
				// replaced, so the plan passes and the apply refuses.
				Config:      config(other, dev),
				ExpectError: expectErr("GitHub login for someone not in developers"),
			},
		},
	})
}

// The GitHub login rule is AutoPilot's, character for character.
func TestGitHubLoginPattern(t *testing.T) {
	for login, want := range map[string]bool{
		"a":                     true,
		"0":                     true,
		"octocat":               true,
		"octo-cat":              true,
		"octo_cat":              true,
		"octocat-":              true,
		"0octocat":              true,
		strings.Repeat("a", 39): true,
		strings.Repeat("a", 40): false,
		"":                      false,
		"Octocat":               false,
		"OCTOCAT":               false,
		"-octocat":              false,
		"_octocat":              false,
		"octo.cat":              false,
		"octo cat":              false,
		"octocat[bot]":          false,
		"octocät":               false,
		"octocat\n":             false,
	} {
		if got := gitHubLoginPattern.MatchString(login); got != want {
			t.Errorf("%q: matches = %v, want %v", login, got, want)
		}
	}
}

// The developer rule is AutoPilot's, step for step: what it takes, the plan
// takes, and what it refuses, the plan refuses.
func TestValidEmail(t *testing.T) {
	for email, want := range map[string]bool{
		"someone@example.com":                    true,
		"some.one+ap@example.com":                true,
		"some_one%x-y@mail.example.co":           true,
		"0@a.b":                                  true,
		strings.Repeat("a", 64) + "@example.com": true,
		strings.Repeat("a", 65) + "@example.com": false,
		"a@" + strings.Repeat("b", 248) + ".com": true,
		"a@" + strings.Repeat("b", 249) + ".com": false,
		"":                                       false,
		"someone":                                false,
		"@example.com":                           false,
		"someone@":                               false,
		"someone@example":                        false,
		"someone@.example.com":                   false,
		"someone@example.com.":                   false,
		"someone@example..com":                   false,
		"someone@example.com@example.com":        false,
		"Someone@example.com":                    false,
		"someone@Example.com":                    false,
		"someoné@example.com":                    false,
		"some one@example.com":                   false,
		"some!one@example.com":                   false,
		"some,one@example.com":                   false,
		"{someone}@example.com":                  false,
		"some..one@example.com":                  true,
		".someone@example.com":                   true,
	} {
		if got := validEmail(email); got != want {
			t.Errorf("%q: valid = %v, want %v", email, got, want)
		}
	}
}

// GitHub logins AutoPilot answered with map to state as the other lists do:
// none stays null where the configuration left them out.
func TestGitHubLoginsToSet(t *testing.T) {
	null := types.SetNull(gitHubLoginObjectType)
	empty := types.SetValueMust(gitHubLoginObjectType, nil)
	if got := gitHubLoginsToSet(nil, null); !got.IsNull() {
		t.Errorf("none, left out: %v, want null", got)
	}
	if got := gitHubLoginsToSet([]client.GitHubLogin{}, null); !got.IsNull() {
		t.Errorf("[], left out: %v, want null", got)
	}
	if got := gitHubLoginsToSet(nil, empty); got.IsNull() || len(got.Elements()) != 0 {
		t.Errorf("none, configured []: %v, want []", got)
	}
	got := gitHubLoginsToSet([]client.GitHubLogin{{Login: "alice-gh", Developer: "alice@example.org"}}, null)
	if got.IsNull() || len(got.Elements()) != 1 {
		t.Errorf("one, left out: %v, want it in state", got)
	}
}

// GitHub logins that are somehow still unknown at apply are an error, never
// sent as []: that would take every login away.
func TestUnknownGitHubLoginsAreAnError(t *testing.T) {
	ctx := context.Background()
	var diags diag.Diagnostics
	if got := gitHubLoginsFromSet(ctx, types.SetUnknown(gitHubLoginObjectType), &diags); got != nil || !diags.HasError() {
		t.Errorf("unknown github_logins gave %v, %v", got, diags)
	}
	diags = nil
	withUnknown := types.SetValueMust(gitHubLoginObjectType, []attr.Value{
		types.ObjectValueMust(gitHubLoginAttrTypes, map[string]attr.Value{
			"login": types.StringValue("alice-gh"), "developer": types.StringUnknown(),
		}),
	})
	if got := gitHubLoginsFromSet(ctx, withUnknown, &diags); got != nil || !diags.HasError() {
		t.Errorf("a developer unknown gave %v, %v", got, diags)
	}
	diags = nil
	if got := gitHubLoginsFromSet(ctx, types.SetNull(gitHubLoginObjectType), &diags); got != nil || diags.HasError() {
		t.Errorf("null github_logins gave %v, %v; null is simply not sent", got, diags)
	}
}
