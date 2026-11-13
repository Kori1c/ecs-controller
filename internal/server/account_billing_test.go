package server

import (
	"encoding/json"
	"github.com/Kori1c/ecs-controller/internal/app"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAccountBillingKeepsUnknownDistinctFromZero(t *testing.T) {
	srv, st := logsTestServer(t)
	if err := st.SaveGroups([]app.AccountGroup{{GroupKey: "g", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "r", SiteType: "international"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(app.Account{GroupKey: "g", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "r", InstanceID: "i", TrafficAPIStatus: "error"}); err != nil {
		t.Fatal(err)
	}
	st.SetSetting("enable_billing", "1")
	accounts, _ := st.LoadAccounts(false)
	id := accounts[0].ID
	read := func() map[string]any {
		w := httptest.NewRecorder()
		srv.config(w)
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result["Accounts"].([]any)[0].(map[string]any)["billing"].(map[string]any)
	}
	billing := read()
	if billing["monthly_cost"] != nil || billing["balance"] != nil || billing["error"] == nil || billing["currency"] != "USD" {
		t.Fatalf("unknown data became valid zero: %+v", billing)
	}
	st.SetBillingCache(id, "bill_overview", time.Now().Format("2006-01"), map[string]any{"monthly_cost": 0.13259, "currency": "USD"})
	billing = read()
	if billing["monthly_cost"] != 0.13259 || billing["balance"] != nil {
		t.Fatal("partial summary hidden or filled with zero")
	}
	st.SetBillingCache(id, "balance", "", map[string]any{"balance": 0, "currency": "USD"})
	billing = read()
	if billing["balance"] != float64(0) || billing["error"] != nil {
		t.Fatalf("traffic failure incorrectly invalidated valid billing: %+v", billing)
	}
}
