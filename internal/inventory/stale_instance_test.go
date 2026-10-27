package inventory

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
)

// regionClient records which region each DescribeInstance confirmation used and
// can hold a different inventory per region, so region drift is observable.
type regionClient struct {
	cloud.Client
	byRegion    map[string][]cloud.Instance
	confirmSeen []string
}

func (c *regionClient) DescribeInstances(_ context.Context, region string) ([]cloud.Instance, error) {
	return c.byRegion[region], nil
}

func (c *regionClient) DescribeInstance(_ context.Context, region, id string) (*cloud.Instance, error) {
	c.confirmSeen = append(c.confirmSeen, region)
	for _, instance := range c.byRegion[region] {
		if instance.ID == id {
			copy := instance
			return &copy, nil
		}
	}
	return nil, &cloud.APIError{Code: "InvalidInstanceId.NotFound", HTTPStatus: 404}
}

func seedRow(t *testing.T, st interface {
	UpsertAccount(app.Account) error
}, account app.Account) {
	t.Helper()
	if err := st.UpsertAccount(account); err != nil {
		t.Fatal(err)
	}
}

func TestStaleReleasingRowIsPurgedWhenNoReleaseJobIsActive(t *testing.T) {
	st, group := newInventoryStore(t)
	defer st.Close()
	seedRow(t, st, app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: group.GroupKey, InstanceID: "i-gone", InstanceStatus: "Releasing"})

	result, err := SyncGroup(context.Background(), st, group, &regionClient{byRegion: map[string][]cloud.Instance{"cn-test": {{ID: "i-live", Status: "Running"}}}}, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Purged != 1 {
		t.Fatalf("stale releasing row survived: %+v", result)
	}
	if accounts, _ := st.LoadAccounts(true); len(accounts) != 1 || accounts[0].InstanceID != "i-live" {
		t.Fatalf("unexpected rows after purge: %#v", accounts)
	}
}

func TestReleasingRowWithActiveReleaseJobIsKept(t *testing.T) {
	st, group := newInventoryStore(t)
	defer st.Close()
	seedRow(t, st, app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: group.GroupKey, InstanceID: "i-releasing", InstanceStatus: "Releasing"})
	accounts, err := st.LoadAccounts(false)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("load: %#v err=%v", accounts, err)
	}
	if err := st.EnqueueJob("job-releasing", "delete_instance", strconv.FormatInt(accounts[0].ID, 10), map[string]any{"accountId": accounts[0].ID}); err != nil {
		t.Fatal(err)
	}

	result, err := SyncGroup(context.Background(), st, group, &regionClient{byRegion: map[string][]cloud.Instance{"cn-test": {}}}, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Purged != 0 {
		t.Fatalf("row owned by an active release job was purged: %+v", result)
	}
	if rows, _ := st.LoadAccounts(true); len(rows) != 1 {
		t.Fatalf("active release row disappeared: %#v", rows)
	}
}

func TestGroupKeyDriftStillReconcilesByAccessKey(t *testing.T) {
	st, group := newInventoryStore(t)
	defer st.Close()
	seedRow(t, st, app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "legacy-key", InstanceID: "i-gone", InstanceStatus: "Running"})
	seedRow(t, st, app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "legacy-key", InstanceID: "i-live", InstanceStatus: "Running"})

	result, err := SyncGroup(context.Background(), st, group, &regionClient{byRegion: map[string][]cloud.Instance{"cn-test": {{ID: "i-live", Status: "Running"}}}}, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Purged != 1 {
		t.Fatalf("drifted group key left a stale row: %+v", result)
	}
	rows, err := st.LoadAccounts(true)
	if err != nil || len(rows) != 1 || rows[0].InstanceID != "i-live" {
		t.Fatalf("unexpected rows: %#v err=%v", rows, err)
	}
	if rows[0].GroupKey != group.GroupKey {
		t.Fatalf("drifted row was not reattached to its group: %q", rows[0].GroupKey)
	}
	if rows[0].InstanceStatus != "Running" {
		t.Fatalf("live instance status not refreshed: %q", rows[0].InstanceStatus)
	}
}

func TestOtherAccountsRowsAreNeverPurged(t *testing.T) {
	st, group := newInventoryStore(t)
	defer st.Close()
	seedRow(t, st, app.Account{AccessKeyID: "other-ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "other", InstanceID: "i-other", InstanceStatus: "Running"})

	result, err := SyncGroup(context.Background(), st, group, &regionClient{byRegion: map[string][]cloud.Instance{"cn-test": {}}}, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Purged != 0 {
		t.Fatalf("another account's row was purged: %+v", result)
	}
	if rows, _ := st.LoadAccounts(true); len(rows) != 1 {
		t.Fatalf("another account's row was removed: %#v", rows)
	}
}

func TestSyncDoesNotQueryRowsOutsideItsRegion(t *testing.T) {
	st, group := newInventoryStore(t)
	defer st.Close()
	seedRow(t, st, app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-drift", GroupKey: group.GroupKey, InstanceID: "i-alive", InstanceStatus: "Running"})
	client := &regionClient{byRegion: map[string][]cloud.Instance{
		"cn-test":  {},
		"cn-drift": {{ID: "i-alive", Status: "Running"}},
	}}

	result, err := SyncGroup(context.Background(), st, group, client, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Purged != 0 {
		t.Fatalf("live instance in its own region was purged: %+v", result)
	}
	if len(client.confirmSeen) != 0 {
		t.Fatalf("sync crossed its region boundary: %v", client.confirmSeen)
	}
}

func TestReleasingRowIsNotResurrectedWhileInstanceStillExists(t *testing.T) {
	st, group := newInventoryStore(t)
	defer st.Close()
	seedRow(t, st, app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: group.GroupKey, InstanceID: "i-stuck", InstanceStatus: "Releasing"})

	result, err := SyncGroup(context.Background(), st, group, &regionClient{byRegion: map[string][]cloud.Instance{"cn-test": {{ID: "i-stuck", Status: "Stopped"}}}}, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Purged != 0 {
		t.Fatalf("existing instance was purged: %+v", result)
	}
	rows, _ := st.LoadAccounts(true)
	if len(rows) != 1 || rows[0].InstanceStatus != "Releasing" {
		t.Fatalf("in-flight release was resurrected: %#v", rows)
	}
}

func TestPurgeAbsentNeverTouchesAnotherAccountsRows(t *testing.T) {
	st, group := newInventoryStore(t)
	defer st.Close()
	seedRow(t, st, app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "g1", InstanceID: "i-own", InstanceStatus: "Running"})
	seedRow(t, st, app.Account{AccessKeyID: "other-ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "g2", InstanceID: "i-other", InstanceStatus: "Running"})

	purged, err := PurgeAbsent(context.Background(), st, group, &regionClient{byRegion: map[string][]cloud.Instance{"cn-test": {}}}, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if purged != 1 {
		t.Fatalf("purged=%d, want only the group's own row", purged)
	}
	rows, _ := st.LoadAccounts(true)
	if len(rows) != 1 || rows[0].InstanceID != "i-other" {
		t.Fatalf("another account's row was affected: %#v", rows)
	}
}
