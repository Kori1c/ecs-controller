package worker

import (
	"context"
	"errors"
	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/store"
	"testing"
	"time"
)

func TestCDTFallbackDoesNotResetThresholdNotificationDedup(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	worker := &Worker{Store: st}
	fake := &fakeCloud{cdtTraffic: 120}
	now := time.Now()
	previous := now.Add(-time.Minute).Unix()
	account := &app.Account{InstanceID: "i-test", MaxTraffic: 100, InstanceStatus: "Running", ProtectionNotifiedAt: previous}
	worker.handleTrafficError(context.Background(), fake, account, now, 95, "notify_only", "KeepCharging", "CMS timeout", errors.New("CMS timeout"), false, false)
	if account.ProtectionNotifiedAt != previous {
		t.Fatal("CMS failure reset healthy CDT threshold notification dedup")
	}
	if account.ProtectionSuspended {
		t.Fatal("CDT fallback did not remain active")
	}
}
