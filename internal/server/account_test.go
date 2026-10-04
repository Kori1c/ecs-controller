package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
	"github.com/Kori1c/ecs-controller/internal/store"
)

type fakeAccountTester struct {
	*cloud.Service
	test func(context.Context, string, string) cloud.AccountTestResult
}

func (f *fakeAccountTester) TestAccount(ctx context.Context, region, site string) cloud.AccountTestResult {
	return f.test(ctx, region, site)
}

func TestAccountEndpointRestoresSecretAndReturnsAllChecks(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveGroups([]app.AccountGroup{{GroupKey: "saved", AccessKeyID: "test-key", AccessKeySecret: "saved-secret", RegionID: "cn-hongkong", SiteType: "international"}}); err != nil {
		t.Fatal(err)
	}
	srv := New(st, t.TempDir(), "unused", "setup-token", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, "/index.php?action=test_account", nil).WithContext(ctx)
	srv.CloudFactory = func(account app.Account) cloud.Client {
		if account.AccessKeySecret != "saved-secret" || account.AccessKeyID != "test-key" || account.SiteType != "international" {
			t.Fatalf("wrong restored account: region=%s site=%s", account.RegionID, account.SiteType)
		}
		return &fakeAccountTester{test: func(ctx context.Context, region, site string) cloud.AccountTestResult {
			if ctx.Err() != context.Canceled || region != "cn-hongkong" || site != "international" {
				t.Fatal("request context or account scope was not propagated")
			}
			return cloud.AccountTestResult{Status: "missing", Message: "need permissions", Checks: []cloud.PermissionCheck{{ID: "cdt", Required: true, Status: "missing", Policy: "AliyunCDTReadOnlyAccess"}}}
		}}
	}
	recorder := httptest.NewRecorder()
	srv.testAccount(recorder, request, map[string]any{"account": map[string]any{"AccessKeyId": "test-key", "AccessKeySecret": "********", "regionId": "cn-hongkong", "siteType": "international", "groupKey": "saved"}})
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v", recorder.Code, recorder.Header())
	}
	var result cloud.AccountTestResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "missing" || len(result.Checks) != 1 || result.Checks[0].Policy != "AliyunCDTReadOnlyAccess" {
		t.Fatalf("missing permission detail lost: %+v", result)
	}
	if strings.Contains(recorder.Body.String(), "saved-secret") || strings.Contains(recorder.Body.String(), "test-key") {
		t.Fatal("response leaked account credentials")
	}
}

func TestAccountEndpointRejectsIncompleteInputBeforeCloudRequests(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(st, t.TempDir(), "unused", "setup-token", nil)
	srv.CloudFactory = func(app.Account) cloud.Client {
		t.Fatal("invalid input reached cloud factory")
		return nil
	}
	for _, data := range []map[string]any{
		{},
		{"account": map[string]any{"AccessKeyId": "key", "regionId": "cn-hongkong"}},
		{"account": map[string]any{"AccessKeyId": "key", "AccessKeySecret": "secret", "regionId": "cn-hongkong", "siteType": "invalid"}},
		{"account": map[string]any{"AccessKeyId": "key", "AccessKeySecret": "********", "regionId": "cn-hongkong"}},
	} {
		recorder := httptest.NewRecorder()
		srv.testAccount(recorder, httptest.NewRequest(http.MethodPost, "/", nil), data)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("invalid input status=%d", recorder.Code)
		}
	}
}

func TestAccountEndpointRequiresAdminAuthentication(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(st, t.TempDir(), "unused", "setup-token", nil)
	srv.CloudFactory = func(app.Account) cloud.Client {
		t.Fatal("unauthenticated request reached cloud factory")
		return nil
	}
	recorder := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/index.php?action=test_account", strings.NewReader(`{"account":{"AccessKeyId":"key","AccessKeySecret":"secret","regionId":"cn-hongkong"}}`)))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated status=%d", recorder.Code)
	}
}
