package server

import (
	"encoding/json"
	"errors"
	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSystemSettingsSaveDoesNotSynchronizeAccounts(t *testing.T) {
	srv, st := logsTestServer(t)
	st.SaveGroups([]app.AccountGroup{{GroupKey: "keep", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "r"}})
	srv.CloudFactory = func(app.Account) cloud.Client { t.Fatal("settings save invoked cloud API"); return nil }
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, logsRequest(http.MethodPost, "/?action=save_config", `{"save_scope":"settings","traffic_threshold":91,"Notification":{"telegram":{"token":"dummy-token","chat_id":"123"}}}`, true))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	groups, err := st.LoadGroups()
	if err != nil || len(groups) != 1 || groups[0].GroupKey != "keep" {
		t.Fatal("settings changed accounts")
	}
	if st.GetSetting("traffic_threshold", "") != "91" {
		t.Fatal("settings not persisted")
	}
	for _, body := range []string{`{"save_scope":"settings","Accounts":[]}`, `{"save_scope":"accounts","Accounts":[],"traffic_threshold":1}`} {
		w = httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, logsRequest(http.MethodPost, "/?action=save_config", body, true))
		if w.Code != 400 {
			t.Fatal("mixed scope accepted")
		}
	}
}
func TestAccountOnlySavePreservesSettingsAndReportsSyncWarnings(t *testing.T) {
	srv, st := logsTestServer(t)
	st.SetSetting("traffic_threshold", "87")
	st.SetSetting("keep_alive", "1")
	srv.CloudFactory = func(app.Account) cloud.Client { return &fakeSyncClient{describeErr: errors.New("mock cloud offline")} }
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, logsRequest(http.MethodPost, "/?action=save_config", `{"save_scope":"accounts","Accounts":[{"groupKey":"g","AccessKeyId":"ak","AccessKeySecret":"sk","regionId":"r"}]}`, true))
	var response struct {
		Success  bool     `json:"success"`
		Warnings []string `json:"warnings"`
	}
	json.Unmarshal(w.Body.Bytes(), &response)
	if w.Code != 200 || !response.Success || len(response.Warnings) == 0 {
		t.Fatalf("sync failure hidden: %s", w.Body.String())
	}
	if st.GetSetting("traffic_threshold", "") != "87" || st.GetSetting("keep_alive", "") != "1" {
		t.Fatal("account save reset settings")
	}
}
func TestSettingsSecretsSupportClearKeepAndOmitted(t *testing.T) {
	srv, st := logsTestServer(t)
	for _, tc := range []struct {
		key  string
		path []string
	}{
		{"notify_tg_token", []string{"Notification", "telegram", "token"}},
		{"notify_tg_proxy_pass", []string{"Notification", "telegram", "proxy_pass"}},
		{"notify_password", []string{"Notification", "password"}},
		{"ddns_cf_token", []string{"Ddns", "cloudflare", "token"}},
	} {
		t.Run(tc.key, func(t *testing.T) {
			if err := srv.saveSecret(tc.key, "dummy-secret"); err != nil {
				t.Fatal(err)
			}
			saved := st.GetSetting(tc.key, "")
			makeData := func(value string, include bool) map[string]any {
				data := map[string]any{}
				parent := data
				for _, p := range tc.path[:len(tc.path)-1] {
					child := map[string]any{}
					parent[p] = child
					parent = child
				}
				if include {
					parent[tc.path[len(tc.path)-1]] = value
				}
				return data
			}
			for _, data := range []map[string]any{makeData("", false), makeData("********", true)} {
				if err := srv.saveConfig(data); err != nil {
					t.Fatal(err)
				}
				if st.GetSetting(tc.key, "") != saved {
					t.Fatal("omitted or masked secret overwritten")
				}
			}
			if err := srv.saveConfig(makeData("", true)); err != nil {
				t.Fatal(err)
			}
			if st.GetSetting(tc.key, "") != "" || masked(st.GetSetting(tc.key, "")) != "" {
				t.Fatal("explicitly cleared secret came back")
			}
		})
	}
}
