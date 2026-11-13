package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
	"github.com/Kori1c/ecs-controller/internal/store"
)

func TestTrafficErrorNotificationsReflectFinalProtectionState(t *testing.T) {
	networkErr := fmt.Errorf("CMS failed: %w", &url.Error{Op: "Get", URL: "https://metrics.aliyuncs.com/?AccessKeyId=dummy&Signature=dummy", Err: context.DeadlineExceeded})
	for _, tc := range []struct {
		name           string
		cmsErr, cdtErr error
		used           float64
		reason, title  string
		stops          int
	}{
		{name: "network with CDT fallback", cmsErr: networkErr, used: 1},
		{name: "network both unavailable", cmsErr: networkErr, cdtErr: errors.New("CDT offline"), reason: "traffic_api_error"},
		{name: "credential with CDT fallback", cmsErr: &cloud.APIError{Code: "InvalidAccessKeyId.NotFound"}, used: 1},
		{name: "permission with CDT fallback", cmsErr: &cloud.APIError{Code: "Forbidden.RAM"}, used: 1},
		{name: "credential both unavailable", cmsErr: &cloud.APIError{Code: "SignatureDoesNotMatch"}, cdtErr: errors.New("CDT offline"), reason: "credential_invalid", title: "阿里云凭据异常"},
		{name: "permission both unavailable", cmsErr: &cloud.APIError{Code: "Forbidden.RAM"}, cdtErr: errors.New("CDT offline"), reason: "permission_denied", title: "阿里云接口访问受限"},
		{name: "CDT threshold still stops", cmsErr: networkErr, used: 120, stops: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			var events []map[string]string
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var event map[string]string
				if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
					t.Error(err)
				}
				events = append(events, event)
				w.WriteHeader(200)
			}))
			defer receiver.Close()
			for k, v := range map[string]string{"notify_email_enabled": "0", "notify_wh_enabled": "1", "notify_wh_url": receiver.URL, "notify_wh_method": "POST", "notify_wh_request_type": "JSON"} {
				if err := st.SetSetting(k, v); err != nil {
					t.Fatal(err)
				}
			}
			worker := &Worker{Store: st}
			fake := &fakeCloud{cdtTraffic: tc.used, cdtErr: tc.cdtErr}
			account := &app.Account{InstanceID: "i-test", GroupKey: "g", RegionID: "r", InstanceStatus: "Running", MaxTraffic: 100}
			now := time.Now()
			worker.handleTrafficError(context.Background(), fake, account, now, 95, "stop_and_notify", "KeepCharging", tc.cmsErr.Error(), tc.cmsErr, true, true)
			if account.ProtectionSuspendReason != tc.reason || account.ProtectionSuspended != (tc.reason != "") {
				t.Fatalf("wrong final protection state: %+v", account)
			}
			if fake.stopped != tc.stops || fake.started != 0 {
				t.Fatalf("unsafe automation: %+v", fake)
			}
			count := 0
			for _, event := range events {
				if event["title"] == "阿里云凭据异常" || event["title"] == "阿里云接口访问受限" {
					count++
					if event["title"] != tc.title {
						t.Fatalf("misleading title: %+v", event)
					}
					if tc.reason == "permission_denied" && strings.Contains(event["text"], "请更新 AK") {
						t.Fatal("permission error advised replacing AK")
					}
				}
			}
			if (tc.title != "" && count != 1) || (tc.title == "" && count != 0) {
				t.Fatalf("unexpected notification count %d: %+v", count, events)
			}
			if tc.title != "" {
				worker.handleTrafficError(context.Background(), fake, account, now.Add(time.Minute), 95, "stop_and_notify", "KeepCharging", tc.cmsErr.Error(), tc.cmsErr, false, false)
				if len(events) != 1 {
					t.Fatalf("repeated failure spammed notifications: %+v", events)
				}
				fake.cdtErr = nil
				fake.cdtTraffic = 1
				worker.handleTrafficError(context.Background(), fake, account, now.Add(2*time.Minute), 95, "stop_and_notify", "KeepCharging", tc.cmsErr.Error(), tc.cmsErr, false, false)
				if account.ProtectionSuspended || account.ProtectionNotifiedAt != 0 {
					t.Fatal("fallback recovery did not clear failure state")
				}
				// A later real failure must notify again after successful recovery.
				fake.cdtErr = errors.New("CDT offline again")
				worker.handleTrafficError(context.Background(), fake, account, now.Add(3*time.Minute), 95, "stop_and_notify", "KeepCharging", tc.cmsErr.Error(), tc.cmsErr, false, false)
				if len(events) != 2 {
					t.Fatal("new failure after recovery was not notified")
				}
			}
		})
	}
}
