package provider

import (
	"context"
	"fmt"
	"slices"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// stringsFromSet returns a known set's values, sorted. A null set gives nil
// (not sent); a known empty set gives an empty, non-nil slice, which the
// client sends as []. Every value is known by the time it is applied, so an
// unknown set is a bug, reported as an error rather than read as empty: an
// empty list sent by mistake would take away an app's repositories or
// consent.
func stringsFromSet(ctx context.Context, set types.Set, attr string, diags *diag.Diagnostics) []string {
	if set.IsUnknown() {
		unknownAtApply(diags, attr)
		return nil
	}
	if set.IsNull() {
		return nil
	}
	out := []string{}
	diags.Append(set.ElementsAs(ctx, &out, false)...)
	slices.Sort(out)
	return out
}

// unknownAtApply reports a value that should have been known by apply.
func unknownAtApply(diags *diag.Diagnostics, attr string) {
	diags.AddAttributeError(path.Root(attr), "Value unknown at apply",
		fmt.Sprintf("`%s` is still unknown when it is applied, so nothing was sent. This is a bug in the provider; "+
			"please report it.", attr))
}

// setFromStrings is a set of strings, empty (not null) for no values.
func setFromStrings(values []string) types.Set {
	elems := make([]attr.Value, 0, len(values))
	for _, v := range values {
		elems = append(elems, types.StringValue(v))
	}
	return types.SetValueMust(types.StringType, elems)
}
