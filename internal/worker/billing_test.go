package worker

import (
	"context"
	"errors"
	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/store"
	"testing"
	"time"
)

type independentBillingCloud struct {
	fakeCloud
	balanceCalls, overviewCalls int
	balanceErr                  error
}

func (f *independentBillingCloud) GetAccountBalance(context.Context, string) (float64, string, error) {
	f.balanceCalls++
	return 9.123456, "USD", f.balanceErr
}
func (f *independentBillingCloud) GetBillOverview(context.Context, string, string) (float64, string, error) {
	f.overviewCalls++
	return 0.13259, "USD", nil
}
func TestBillingRefreshIsIndependentOfInstanceAndTrafficHealth(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "r", InstanceID: "i", TrafficAPIStatus: "error", ProtectionSuspended: true, HealthStatus: "error"}); err != nil {
		t.Fatal(err)
	}
	accounts, err := st.LoadAccounts(false)
	if err != nil || len(accounts) != 1 {
		t.Fatal(err)
	}
	fake := &independentBillingCloud{fakeCloud: fakeCloud{describeErr: errors.New("DNS timeout"), outboundErr: errors.New("CMS timeout")}, balanceErr: errors.New("balance timeout")}
	w := &Worker{Store: st, Cloud: fake}
	now := time.Now()
	w.refreshBillingAccounts(context.Background(), accounts, now)
	cycle := now.Format("2006-01")
	overview, ok := st.GetBillingCache(accounts[0].ID, "bill_overview", cycle, time.Hour)
	if !ok || overview["monthly_cost"] != 0.13259 {
		t.Fatal("traffic error prevented independent billing refresh")
	}
	if _, ok := st.GetBillingCache(accounts[0].ID, "balance", "", time.Hour); ok {
		t.Fatal("failed balance cached as valid zero")
	}
	fake.balanceErr = nil
	w.refreshBillingAccounts(context.Background(), accounts, now)
	balance, ok := st.GetBillingCache(accounts[0].ID, "balance", "", time.Hour)
	if !ok || balance["balance"] != 9.123456 {
		t.Fatal("failed balance was not retried")
	}
	if fake.balanceCalls != 2 || fake.overviewCalls != 1 {
		t.Fatal("independent cache/retry was not respected")
	}
	if _, ok := st.GetBillingCache(accounts[0].ID, "instance_bill", cycle, time.Hour); ok {
		t.Fatal("account summary fetched instance-scoped fallback")
	}
}
