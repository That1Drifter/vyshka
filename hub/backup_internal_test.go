package hub

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Regression for review finding B1 on PR #152: with keep finished snapshots
// already in the directory whose names sort after the new one (a clock that
// ran ahead and was corrected), rotation deleted the snapshot just taken while
// the endpoint answered 201 naming it. The snapshot just named must survive
// rotation and be listed as kept, whatever names the directory already holds.
func TestNewBackupIsNeverRotatedOut(t *testing.T) {
	for _, keep := range []int{1, 3} {
		dir := t.TempDir()
		for _, planted := range []string{
			"vyshka-20990101T000000.000Z.db",
			"vyshka-20990102T000000.000Z.db",
			"vyshka-20990103T000000.000Z.db",
		} {
			if err := os.WriteFile(filepath.Join(dir, planted), []byte("planted"), 0o600); err != nil {
				t.Fatal(err)
			}
		}

		name, err := freeBackupName(dir, time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("new"), 0o600); err != nil {
			t.Fatal(err)
		}
		kept, pruned := rotateBackups(dir, keep)

		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("keep %d: the snapshot just taken (%s) is gone after rotation: %v", keep, name, err)
		}
		if !slices.Contains(kept, name) || slices.Contains(pruned, name) {
			t.Errorf("keep %d: new snapshot %s, kept %v, pruned %v", keep, name, kept, pruned)
		}
		if len(kept) != keep {
			t.Errorf("keep %d: kept %d snapshots: %v", keep, len(kept), kept)
		}
	}
}
