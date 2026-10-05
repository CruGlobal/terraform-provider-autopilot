package provider

import (
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-autopilot/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// addAPIError gives each kind of failed answer its own advice.
func TestAddAPIError_Advice(t *testing.T) {
	cases := []struct {
		name string
		err  *client.Error
		want string
	}{
		{
			name: "a redirect",
			err:  &client.Error{Method: "DELETE", Path: "senders/tracker", Status: 302, Message: "Found"},
			want: "AutoPilot never redirects",
		},
	}
	for _, c := range cases {
		var diags diag.Diagnostics
		addAPIError(&diags, "Error", c.err, senderAPIFields)
		if len(diags) != 1 || !strings.Contains(diags[0].Detail(), c.want) {
			t.Errorf("%s: diagnostics = %v, want one that says %q", c.name, diags, c.want)
		}
	}
}
