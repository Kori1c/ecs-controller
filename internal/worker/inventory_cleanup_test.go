package worker

import (
	"context"
	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
	"github.com/Kori1c/ecs-controller/internal/inventory"
	"github.com/Kori1c/ecs-controller/internal/store"
	"strconv"
	"testing"
	"time"
)

func TestReleaseWorkerCanFinishHiddenMissingInstance(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "r", GroupKey: "g", InstanceID: "i-gone", InstanceStatus: "Releasing", EIPManaged: true, EIPAllocationID: "eip-owned"}); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.LoadAccounts(true)
	id := rows[0].ID
	s.EnqueueJob("release", "delete_instance", strconv.FormatInt(id, 10), map[string]any{"accountId": id})
	if _, err = s.PurgeCloudMissing("ak", "r", "i-gone"); err != nil {
		t.Fatal(err)
	}
	if visible, _ := s.LoadAccounts(false); len(visible) != 0 {
		t.Fatal("missing instance still visible")
	}
	fake := &fakeCloud{describeErr: &cloud.APIError{Code: "InvalidInstanceId.NotFound"}}
	w := &Worker{Store: s, Cloud: fake}
	job, err := s.ClaimJob(2 * time.Minute)
	if err != nil || job == nil {
		t.Fatalf("cleanup job unavailable: %v", err)
	}
	if err = w.deleteInstance(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if fake.unassociated != 1 || fake.released != 1 || fake.deleted != 0 {
		t.Fatalf("cleanup not completed safely: %+v", fake)
	}
	if kept, _ := s.LoadAccounts(true); len(kept) != 0 {
		t.Fatal("completed release retained local row")
	}
}
func TestMonitorMissingCleanupDoesNotDestroyDuplicateReleaseJob(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 2; i++ {
		s.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "r", InstanceID: "i-gone", InstanceStatus: "Releasing", EIPManaged: true, EIPAllocationID: "eip-owned"})
	}
	rows, _ := s.LoadAccounts(true)
	s.EnqueueJob("release", "delete_instance", strconv.FormatInt(rows[1].ID, 10), map[string]any{})
	s.DB.Exec(`UPDATE jobs SET status='running',locked_at=?`, time.Now().Add(-time.Hour).Unix())
	w := &Worker{Store: s}
	if err = w.purgeMissingAccount(context.Background(), rows[0]); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimJob(time.Minute)
	if err != nil || job == nil {
		t.Fatal("expired cleanup task was lost")
	}
	if kept, _ := s.LoadAccounts(true); len(kept) != 2 {
		t.Fatal("cleanup recovery rows were removed")
	}
}
func TestUnconfirmedReconciliationDoesNotRecordSuccess(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	g := app.AccountGroup{GroupKey: "g", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "r"}
	s.SaveGroups([]app.AccountGroup{g})
	s.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "r", GroupKey: "g", InstanceID: "i-unknown", InstanceStatus: "Running"})
	fake := &fakeCloud{inventoryInstances: []cloud.Instance{}, describeErr: &cloud.APIError{Code: "Forbidden.RAM"}}
	w := &Worker{Store: s, Cloud: fake}
	w.reconcileCloudInventory(context.Background(), time.Now())
	if s.GetSetting("inventory_reconcile_success_g", "") != "" {
		t.Fatal("unconfirmed sweep recorded success")
	}
	_, err = inventory.SyncGroup(context.Background(), s, g, fake, time.Now(), nil)
	if err == nil {
		t.Fatal("expected incomplete reconciliation")
	}
}
