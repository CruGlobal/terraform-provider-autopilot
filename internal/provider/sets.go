package provider

import (
	"context"
	"slices"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// stringsFromSet returns a known set's values, sorted. A null or unknown set
// gives nil; a known empty set gives an empty, non-nil slice, which the
// client sends as [].
func stringsFromSet(ctx context.Context, set types.Set, diags *diag.Diagnostics) []string {
	if set.IsNull() || set.IsUnknown() {
		return nil
	}
	out := []string{}
	diags.Append(set.ElementsAs(ctx, &out, false)...)
	slices.Sort(out)
	return out
}

// setFromStrings is a set of strings, empty (not null) for no values.
func setFromStrings(values []string) types.Set {
	elems := make([]attr.Value, 0, len(values))
	for _, v := range values {
		elems = append(elems, types.StringValue(v))
	}
	return types.SetValueMust(types.StringType, elems)
}
