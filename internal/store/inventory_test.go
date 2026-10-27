package store

import (
	"github.com/Kori1c/ecs-controller/internal/app"
	"strconv"
	"testing"
	"time"
)

func TestMissingCleanupProtectsAllDuplicateReleaseJobs(t *testing.T) {
	for _, status := range []string{"queued", "retry", "running", "failed"} {
		t.Run(status, func(t *testing.T) {
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			for i := 0; i < 2; i++ {
				if err = s.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "r", InstanceID: "i", InstanceStatus: "Releasing", EIPManaged: true, EIPAllocationID: "eip-keep"}); err != nil {
					t.Fatal(err)
				}
			}
			rows, _ := s.LoadAccounts(true)
			if len(rows) != 2 {
				t.Fatal("need duplicates")
			}
			entity := strconv.FormatInt(rows[1].ID, 10)
			if err = s.EnqueueJob("cleanup", "delete_instance", entity, map[string]any{"accountId": rows[1].ID}); err != nil {
				t.Fatal(err)
			}
			if _, err = s.DB.Exec(`UPDATE jobs SET status=?,locked_at=? WHERE job_id='cleanup'`, status, time.Now().Add(-time.Hour).Unix()); err != nil {
				t.Fatal(err)
			}
			removed, err := s.PurgeCloudMissing("ak", "r", "i")
			if err != nil || removed {
				t.Fatalf("unfinished cleanup deleted: %v %v", removed, err)
			}
			visible, _ := s.LoadAccounts(false)
			kept, _ := s.LoadAccounts(true)
			var jobs int
			s.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE job_id='cleanup'`).Scan(&jobs)
			if len(visible) != 0 || len(kept) != 2 || jobs != 1 {
				t.Fatalf("visible=%d kept=%d jobs=%d", len(visible), len(kept), jobs)
			}
			if kept[1].EIPAllocationID != "eip-keep" || kept[1].InstanceStatus != "Releasing" {
				t.Fatal("release recovery data lost")
			}
			if err = s.FinishJob("cleanup"); err != nil {
				t.Fatal(err)
			}
			removed, err = s.PurgeCloudMissing("ak", "r", "i")
			if err != nil || !removed {
				t.Fatalf("finished cleanup should purge: %v %v", removed, err)
			}
		})
	}
}
