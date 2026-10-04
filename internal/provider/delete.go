package provider

import (
	"context"

	"github.com/CruGlobal/terraform-provider-autopilot/internal/client"
)

// deleteWithIfMatch runs a DELETE that carries the state's lock_version. A
// delete is not an overwrite (the user asked for the record to be gone
// whatever it now holds), so a stale_object is answered by reading the
// current version once and deleting with that; only a second stale_object is
// returned. Every other error is returned as it is.
func deleteWithIfMatch(ctx context.Context, lockVersion int64,
	del func(ctx context.Context, lockVersion int64) error,
	currentVersion func(ctx context.Context) (int64, error),
) error {
	err := del(ctx, lockVersion)
	if err == nil || !client.IsStale(err) {
		return err
	}
	fresh, rerr := currentVersion(ctx)
	if rerr != nil {
		if client.IsNotFound(rerr) {
			return nil // already gone
		}
		return err
	}
	return del(ctx, fresh)
}
