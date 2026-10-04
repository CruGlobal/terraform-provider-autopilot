package provider

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/CruGlobal/terraform-provider-autopilot/internal/autopilottest"
	"github.com/CruGlobal/terraform-provider-autopilot/internal/client"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// The sweepers remove what a live run that failed part way left behind:
// every app and sender whose name starts with tfacc-, the prefix every test
// record has (randName). The acceptance workflow runs them before the tests:
//
//	go test ./internal/provider/ -sweep=live
//
// They read AUTOPILOT_ENDPOINT and AUTOPILOT_TOKEN, and never touch a record
// without the prefix.
func init() {
	resource.AddTestSweepers("autopilot_app", &resource.Sweeper{
		Name: "autopilot_app",
		F:    func(string) error { return sweepLive("apps", sweepTestApps) },
	})
	resource.AddTestSweepers("autopilot_sender", &resource.Sweeper{
		Name:         "autopilot_sender",
		Dependencies: []string{"autopilot_app"},
		F:            func(string) error { return sweepLive("senders", sweepTestSenders) },
	})
}

// testNamePrefix starts the name of every record a test makes.
const testNamePrefix = "tfacc-"

func sweepLive(what string, sweep func(context.Context, *client.Client) (int, error)) error {
	endpoint, token := os.Getenv(envEndpoint), os.Getenv(envToken)
	if endpoint == "" || token == "" {
		return fmt.Errorf("%s and %s must be set to sweep", envEndpoint, envToken)
	}
	c, err := client.New(endpoint, token, client.WithUserAgent(client.DefaultUserAgent+"/sweeper"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	n, err := sweep(ctx, c)
	log.Printf("[INFO] removed %d leftover test %s", n, what)
	return err
}

// sweepTestApps deletes every app whose name starts with tfacc-.
func sweepTestApps(ctx context.Context, c *client.Client) (int, error) {
	apps, err := c.ListApps(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, a := range apps {
		if !strings.HasPrefix(a.Name, testNamePrefix) {
			continue
		}
		if err := c.DeleteApp(ctx, a.Name, a.LockVersion); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// sweepTestSenders retires every sender whose name starts with tfacc-.
func sweepTestSenders(ctx context.Context, c *client.Client) (int, error) {
	senders, err := c.ListSenders(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, s := range senders {
		if !strings.HasPrefix(s.Name, testNamePrefix) {
			continue
		}
		if err := c.DeleteSender(ctx, s.Name, s.LockVersion); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// The sweepers remove test records, and nothing else.
func TestSweepers_removeOnlyTestRecords(t *testing.T) {
	env := newTestEnv(t)
	env.requireFake(t)
	for _, name := range []string{"tfacc-left-behind", "tracker"} {
		env.fake.SeedSender(autopilottest.SenderSeed{Name: name, RequestSecret: randSecret(t), CallbackSecret: randSecret(t)})
	}
	for _, name := range []string{"tfacc-left-behind", "billing"} {
		env.fake.SeedApp(autopilottest.AppView{Name: name})
	}
	ctx, c := context.Background(), env.apiClient(t)
	if n, err := sweepTestApps(ctx, c); err != nil || n != 1 {
		t.Fatalf("apps: removed %d, %v", n, err)
	}
	if n, err := sweepTestSenders(ctx, c); err != nil || n != 1 {
		t.Fatalf("senders: removed %d, %v", n, err)
	}
	if _, ok := env.fake.App("tfacc-left-behind"); ok {
		t.Error("the test app is still there")
	}
	if v, _ := env.fake.Sender("tfacc-left-behind"); !v.Retired {
		t.Error("the test sender was not retired")
	}
	if _, ok := env.fake.App("billing"); !ok {
		t.Error("a real app was removed")
	}
	if _, ok := env.fake.Sender("tracker"); !ok {
		t.Error("a real sender was removed")
	}
}
