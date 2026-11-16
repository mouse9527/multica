// Modified for the mouse9527/multica fork: Router v6, slice 2 (M-97), adds the
// intercept call (see intercept.go).

package inbox

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CreateIfEligible returns inserted=false without an error when the recipient
// does not belong to the workspace. Other database errors remain failures.
// Eligibility is checked by the insert's snapshot, without additional locks.
func CreateIfEligible(ctx context.Context, queries *db.Queries, params db.CreateInboxItemParams) (item db.InboxItem, inserted bool, err error) {
	if intercept(ctx, &params) {
		return db.InboxItem{}, false, nil
	}
	item, err = queries.CreateInboxItem(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.InboxItem{}, false, nil
	}
	if err != nil {
		return db.InboxItem{}, false, err
	}
	return item, true, nil
}
