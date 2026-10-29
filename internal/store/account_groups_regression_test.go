package store

import (
	"github.com/Kori1c/ecs-controller/internal/app"
	"testing"
)

func TestExplicitEmptyGroupsDoNotResurrectLegacyRows(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.UpsertAccount(app.Account{AccessKeyID: "dummy", AccessKeySecret: "dummy", RegionID: "r", InstanceID: "i", InstanceStatus: "Running"})
	if err = s.SaveGroups([]app.AccountGroup{}); err != nil {
		t.Fatal(err)
	}
	groups, err := s.LoadGroups()
	if err != nil || len(groups) != 0 {
		t.Fatalf("empty configuration restored old accounts: %d %v", len(groups), err)
	}
}
func TestMissingGroupsStillMigrateLegacyAccounts(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.DB.Exec(`DELETE FROM settings WHERE key='account_groups'`)
	s.UpsertAccount(app.Account{AccessKeyID: "dummy", AccessKeySecret: "dummy", RegionID: "r", InstanceID: "i", InstanceStatus: "Running"})
	groups, err := s.LoadGroups()
	if err != nil || len(groups) != 1 {
		t.Fatalf("legacy migration broken: %d %v", len(groups), err)
	}
}
func TestResetPasswordPreservesUserDataAndInvalidatesSessions(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.ResetAdminPassword("new-password"); err == nil {
		t.Fatal("must not initialize via password recovery")
	}
	s.SetAdminPassword("old-password")
	s.SetSetting("password_login_enabled", "0")
	s.SetSetting("traffic_threshold", "91")
	s.CreateSession("old-session", "csrf", 3600000000000)
	s.SavePasskeyCredential("keep", `{"id":"keep"}`)
	if err = s.ResetAdminPassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
	if !s.CheckAdminPassword("old-password") {
		t.Fatal("invalid reset changed password")
	}
	if err = s.ResetAdminPassword("new-password"); err != nil {
		t.Fatal(err)
	}
	if !s.CheckAdminPassword("new-password") || s.CheckAdminPassword("old-password") {
		t.Fatal("password was not replaced")
	}
	if _, ok := s.Session("old-session"); ok {
		t.Fatal("old session remains valid")
	}
	if s.GetSetting("traffic_threshold", "") != "91" || s.PasskeyCount() != 1 || s.GetSetting("password_login_enabled", "") != "1" {
		t.Fatal("reset destroyed data or did not enable password login")
	}
}
