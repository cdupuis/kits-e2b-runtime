package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"

	kitstorage "github.com/docker/kits-e2b/storage"

	"github.com/e2b-dev/infra/packages/api/internal/db"
	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
)

// E2B retains an earlier pause snapshot after a resumed VM expires. Its private
// volumes must survive too; otherwise that snapshot resumes with empty data.
// Lookup failures preserve data rather than assuming the instance is gone.
func (o *Orchestrator) removeExpiredKitStorage(ctx context.Context, sbx sandbox.NodeSandbox) error {
	if os.Getenv(kitstorage.Environment) == "" {
		return nil
	}
	_, err := db.GetSnapshotBuilds(ctx, o.sqlcDB, sbx.TeamID, sbx.SandboxID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, db.ErrSnapshotNotFound) {
		return fmt.Errorf("check retained Kit snapshot: %w", err)
	}
	_, err = kitstorage.RemoveConfigured(ctx, sbx.TeamID.String(), sbx.SandboxID)

	return err
}
