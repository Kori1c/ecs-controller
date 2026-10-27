package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type failingConfirm struct{ cloud.Client }

func (f *failingConfirm) DescribeInstances(context.Context, string) ([]cloud.Instance, error) {
	return nil, nil
}
func (f *failingConfirm) DescribeInstance(context.Context, string, string) (*cloud.Instance, error) {
	return nil, errors.New("confirmation unavailable")
}

func TestReconciliationReturnsUnconfirmedFailure(t *testing.T) {
	st, g := newInventoryStore(t)
	defer st.Close()
	seedRow(t, st, app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: g.RegionID, GroupKey: g.GroupKey, InstanceID: "i-unknown", InstanceStatus: "Running"})
	_, err := SyncGroup(context.Background(), st, g, &failingConfirm{}, time.Now(), nil)
	if err == nil {
		t.Fatal("unconfirmed reconciliation reported success")
	}
	rows, _ := st.LoadAccounts(false)
	if len(rows) != 1 {
		t.Fatal("unconfirmed instance disappeared")
	}
}
func TestRealRPCDeletedInstanceDisappearsWithoutCopyingLiveInstance(t *testing.T) {
	st, g := newInventoryStore(t)
	defer st.Close()
	seedRow(t, st, app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: g.RegionID, GroupKey: g.GroupKey, InstanceID: "i-gone", InstanceStatus: "Running"})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		items := []any{map[string]any{"InstanceId": "i-live", "Status": "Running", "PublicIpAddress": map[string]any{"IpAddress": []string{"203.0.113.7"}}}}
		if q.Has("InstanceId.1") {
			t.Error("legacy filter used")
		}
		if q.Get("InstanceIds") == `["i-gone"]` {
			items = []any{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"TotalCount": len(items), "Instances": map[string]any{"Instance": items}})
	}))
	defer api.Close()
	client := &cloud.Service{ECS: &cloud.RPCClient{Endpoint: api.URL, AccessKey: "ak", Secret: "sk", HTTPClient: api.Client()}}
	result, err := SyncGroup(context.Background(), st, g, client, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := st.LoadAccounts(false)
	if result.Purged != 1 || len(rows) != 1 || rows[0].InstanceID != "i-live" {
		t.Fatalf("not a cloud mirror: %+v %+v", result, rows)
	}
}
