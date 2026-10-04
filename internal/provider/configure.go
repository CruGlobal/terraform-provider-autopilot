package provider

import (
	"fmt"

	"github.com/CruGlobal/terraform-provider-autopilot/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// clientFromProviderData casts the value the provider stashed at Configure
// time. A nil ProviderData (the framework calls Configure on resources before
// the provider is configured during validation) is not an error; callers must
// tolerate a nil client until then.
func clientFromProviderData(data any, diags *diag.Diagnostics) *client.Client {
	if data == nil {
		return nil
	}
	c, ok := data.(*client.Client)
	if !ok {
		diags.AddError(
			"Unexpected provider data type",
			fmt.Sprintf("Expected *client.Client, got %T. This is a bug in the provider; please report it.", data),
		)
		return nil
	}
	return c
}
