package worker

import (
	"context"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
)

func (w *Worker) refreshBillingAccounts(ctx context.Context, accounts []app.Account, now time.Time) {
	for _, snapshot := range accounts {
		if ctx.Err() != nil {
			return
		}
		// A preceding inventory/instance check may have removed this row.
		account, err := w.Store.Account(snapshot.ID, false)
		if err != nil || account.InstanceID == "" || account.CloudPresence == "missing" {
			continue
		}
		client := w.Cloud
		if w.CloudFactory != nil {
			client = w.CloudFactory(app.AccountGroup{AccessKeyID: account.AccessKeyID, AccessKeySecret: account.AccessKeySecret, RegionID: account.RegionID, SiteType: account.SiteType})
		}
		if client == nil {
			continue
		}
		cycle := now.Format("2006-01")
		if billingClient, ok := client.(cloud.BillingClient); ok {
			if _, valid := w.Store.GetBillingCache(account.ID, "balance", "", 6*time.Hour); !valid {
				if balance, currency, err := billingClient.GetAccountBalance(ctx, account.SiteType); err == nil {
					_ = w.Store.SetBillingCache(account.ID, "balance", "", map[string]any{"balance": balance, "currency": currency})
				} else {
					_ = w.Store.SetBillingCache(account.ID, "balance", "", map[string]any{"error": err.Error()})
				}
			}
			if _, valid := w.Store.GetBillingCache(account.ID, "bill_overview", cycle, 6*time.Hour); !valid {
				if total, currency, err := billingClient.GetBillOverview(ctx, account.SiteType, cycle); err == nil {
					_ = w.Store.SetBillingCache(account.ID, "bill_overview", cycle, map[string]any{"monthly_cost": total, "currency": currency})
				} else {
					_ = w.Store.SetBillingCache(account.ID, "bill_overview", cycle, map[string]any{"error": err.Error()})
				}
			}
			// Account summary and instance billing have different scopes. Do not use
			// an instance bill to fill missing account-wide consumption or balance.
			continue
		}
		if _, valid := w.Store.GetBillingCache(account.ID, "instance_bill", cycle, 6*time.Hour); !valid {
			balance, cost, currency, err := client.GetBilling(ctx, account.SiteType, account.InstanceID, cycle)
			if err != nil {
				_ = w.Store.SetBillingCache(account.ID, "instance_bill", cycle, map[string]any{"error": err.Error()})
			} else {
				_ = w.Store.SetBillingCache(account.ID, "instance_bill", cycle, map[string]any{"balance": balance, "monthly_cost": cost, "currency": currency})
			}
		}
	}
}
