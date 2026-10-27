package server

import (
	"context"
	"errors"
	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
	"github.com/Kori1c/ecs-controller/internal/store"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

type incompleteInventory struct{ cloud.Client }

func (*incompleteInventory) DescribeInstances(context.Context, string) ([]cloud.Instance, error) {
	return nil, nil
}
func (*incompleteInventory) DescribeInstance(context.Context, string, string) (*cloud.Instance, error) {
	return nil, errors.New("cannot verify target")
}

func TestManualSyncDoesNotReportUnconfirmedInventoryAsSuccess(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SaveGroups([]app.AccountGroup{{GroupKey: "g", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "r"}})
	st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "r", GroupKey: "g", InstanceID: "i", InstanceStatus: "Running"})
	srv := New(st, t.TempDir(), "", "setup", nil)
	srv.CloudFactory = func(app.Account) cloud.Client { return &incompleteInventory{} }
	w := httptest.NewRecorder()
	srv.syncAllInstances(w)
	if w.Code == 200 {
		t.Fatalf("partial failure reported success: %s", w.Body.String())
	}
	if rows, _ := st.LoadAccounts(false); len(rows) != 1 {
		t.Fatal("unconfirmed row removed")
	}
}
func TestManualRefreshPreservesExpiredReleaseTask(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "r", InstanceID: "i", InstanceStatus: "Releasing", EIPManaged: true, EIPAllocationID: "eip-keep"})
	rows, _ := st.LoadAccounts(true)
	id := rows[0].ID
	st.EnqueueJob("release", "delete_instance", strconv.FormatInt(id, 10), map[string]any{})
	st.DB.Exec(`UPDATE jobs SET status='running',locked_at=?`, time.Now().Add(-time.Hour).Unix())
	srv := New(st, t.TempDir(), "", "setup", nil)
	srv.CloudFactory = func(app.Account) cloud.Client { return &fakeMissingInstanceClient{} }
	w := httptest.NewRecorder()
	srv.refreshAccount(w, map[string]any{"id": float64(id)})
	if w.Code != 404 {
		t.Fatalf("unexpected status %d", w.Code)
	}
	if visible, _ := st.LoadAccounts(false); len(visible) != 0 {
		t.Fatal("missing row still visible")
	}
	if kept, _ := st.LoadAccounts(true); len(kept) != 1 || kept[0].EIPAllocationID != "eip-keep" {
		t.Fatal("release recovery data lost")
	}
	job, err := st.ClaimJob(time.Minute)
	if err != nil || job == nil {
		t.Fatal("expired cleanup task lost")
	}
}
