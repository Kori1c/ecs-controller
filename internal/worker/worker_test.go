package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
	"github.com/Kori1c/ecs-controller/internal/store"
)

type fakeCloud struct {
	runErr, assocErr                         error
	deleted, unassociated, released, cleaned int
	describeStatus                           string
	describeErr                              error
	inventoryInstances                       []cloud.Instance
	inventoryErr                             error
	inventoryCalls                           int
	cdtTraffic                               float64
	cdtErr                                   error
	cdtCalls                                 int
	outboundErr                              error
	outboundBytes                            float64
	outboundLastMS                           int64
	outboundPoints                           int
	started, stopped                         int
}

type fakeDailyCloud struct {
	fakeCloud
	dailyBytes   float64
	dailyPoints  int
	dailyStartMS int64
	dailyEndMS   int64
}

func (f *fakeDailyCloud) GetInstanceDailyTraffic(_ context.Context, _ string, _ string, _ string, startMS, endMS int64) (float64, int, error) {
	f.dailyStartMS = startMS
	f.dailyEndMS = endMS
	return f.dailyBytes, f.dailyPoints, nil
}

func (f *fakeCloud) DescribeRegions(context.Context) ([]map[string]any, error) { return nil, nil }
func (f *fakeCloud) DescribeZones(context.Context, string) ([]map[string]any, error) {
	return []map[string]any{{"ZoneId": "zone-1"}}, nil
}
func (f *fakeCloud) DescribeInstances(context.Context, string) ([]cloud.Instance, error) {
	f.inventoryCalls++
	return f.inventoryInstances, f.inventoryErr
}
func (f *fakeCloud) DescribeInstance(context.Context, string, string) (*cloud.Instance, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	if f.describeStatus != "" {
		return &cloud.Instance{Status: f.describeStatus}, nil
	}
	if f.inventoryInstances != nil {
		return nil, &cloud.APIError{Code: "InvalidInstanceId.NotFound", HTTPStatus: 404}
	}
	return nil, nil
}

func TestAutomaticInventoryReconciliationDiscoversAndRetiresInstances(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	group := app.AccountGroup{GroupKey: "g", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", MaxTraffic: 200}
	if err := s.SaveGroups([]app.AccountGroup{group}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "g", InstanceID: "i-old", InstanceStatus: "Running"}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeCloud{inventoryInstances: []cloud.Instance{{ID: "i-new", Status: "Running"}}}
	w := &Worker{Store: s, CloudFactory: func(app.AccountGroup) cloud.Client { return fake }}
	start := time.Date(2026, 8, 31, 12, 0, 0, 0, time.Local)

	w.reconcileCloudInventory(context.Background(), start)
	accounts, err := s.LoadAccounts(false)
	if err != nil || len(accounts) != 1 || accounts[0].InstanceID != "i-new" {
		t.Fatalf("first inventory accounts=%#v err=%v", accounts, err)
	}
	if fake.inventoryCalls != 1 {
		t.Fatalf("inventory calls=%d", fake.inventoryCalls)
	}

	w.reconcileCloudInventory(context.Background(), start.Add(5*time.Minute))
	if fake.inventoryCalls != 1 {
		t.Fatalf("inventory interval was not enforced: calls=%d", fake.inventoryCalls)
	}

	w.reconcileCloudInventory(context.Background(), start.Add(11*time.Minute))
	accounts, err = s.LoadAccounts(false)
	if err != nil || len(accounts) != 1 || accounts[0].InstanceID != "i-new" {
		t.Fatalf("retired inventory accounts=%#v err=%v", accounts, err)
	}
	if fake.inventoryCalls != 2 {
		t.Fatalf("second due inventory did not run: calls=%d", fake.inventoryCalls)
	}
	if fake.deleted != 0 || fake.unassociated != 0 || fake.released != 0 {
		t.Fatalf("inventory reconciliation mutated cloud resources: %+v", fake)
	}
}
func (f *fakeCloud) StartInstance(context.Context, string, string) error {
	f.started++
	return nil
}
func (f *fakeCloud) StopInstance(context.Context, string, string, string) error {
	f.stopped++
	return nil
}
func (f *fakeCloud) DeleteInstance(context.Context, string, string) error { f.deleted++; return nil }
func (f *fakeCloud) RunInstances(context.Context, cloud.RunRequest) (cloud.RunResult, error) {
	if f.runErr != nil {
		return cloud.RunResult{}, f.runErr
	}
	return cloud.RunResult{InstanceID: "i-created", PublicIP: "203.0.113.10"}, nil
}
func (f *fakeCloud) AllocateEIP(context.Context, string) (string, string, error) {
	return "eip-1", "203.0.113.11", nil
}
func (f *fakeCloud) AssociateEIP(context.Context, string, string, string) error {
	if f.assocErr != nil {
		return f.assocErr
	}
	return nil
}
func (f *fakeCloud) UnassociateEIP(context.Context, string, string) error {
	f.unassociated++
	return nil
}
func (f *fakeCloud) ReleaseEIP(context.Context, string, string) error { f.released++; return nil }
func (f *fakeCloud) PrepareNetwork(context.Context, string, string, string, string) (string, string, string, error) {
	return "vpc-1", "vsw-1", "sg-1", nil
}
func (f *fakeCloud) CleanupNetwork(context.Context, string, string, string, string) error {
	f.cleaned++
	return nil
}
func (f *fakeCloud) GetTraffic(context.Context, string) (float64, error) {
	f.cdtCalls++
	return f.cdtTraffic, f.cdtErr
}
func (f *fakeCloud) GetOutboundTrafficDelta(context.Context, string, string, string, int64, int64) (float64, int64, int, string, error) {
	return f.outboundBytes, f.outboundLastMS, f.outboundPoints, "InternetOutRate", f.outboundErr
}
func (f *fakeCloud) GetBilling(context.Context, string, string, string) (float64, float64, string, error) {
	return 0, 0, "CNY", nil
}

func TestDailyTrafficEventSeparatesCMSYesterdayAndCDTCurrentUsage(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.SaveGroups([]app.AccountGroup{{GroupKey: "g", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-hongkong", Remark: "香港账号", MaxTraffic: 190}}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-hongkong", GroupKey: "g", InstanceID: "i-1", InstanceName: "ECS-01", InstanceStatus: "Running", TrafficAPIStatus: "ok"}); err != nil {
		t.Fatal(err)
	}
	accounts, err := s.LoadAccounts(false)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("load account: %#v %v", accounts, err)
	}
	day := time.Date(2026, 7, 31, 12, 0, 0, 0, time.Local)
	if err := s.AddTrafficHistory(accounts[0].ID, 10, day.Add(-13*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTrafficHistory(accounts[0].ID, 14.33, day); err != nil {
		t.Fatal(err)
	}

	w := &Worker{Store: s, CloudFactory: func(app.AccountGroup) cloud.Client { return &fakeCloud{cdtTraffic: 8.2} }}
	event, err := w.dailyTrafficEvent(context.Background(), day)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(event.Text, "CMS 实例昨日消耗流量：\n- ECS-01：4.33 GB") || !strings.Contains(event.Text, "CDT 账号流量已使用：\n- 香港账号：8.20 GB/190 GB") || !strings.Contains(event.Text, "数据状态：完整") {
		t.Fatalf("unexpected daily traffic event:\n%s", event.Text)
	}
}

func TestDailyTrafficEventUsesExactCMSDayWindow(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.SaveGroups([]app.AccountGroup{{GroupKey: "g", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-hongkong", Remark: "香港账号", MaxTraffic: 190}}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-hongkong", GroupKey: "g", InstanceID: "i-1", InstanceName: "ECS-01", InstanceStatus: "Running", TrafficAPIStatus: "ok"}); err != nil {
		t.Fatal(err)
	}

	reportDay := time.Date(2026, 7, 31, 12, 0, 0, 0, time.Local)
	dayStart := time.Date(2026, 7, 31, 0, 0, 0, 0, time.Local)
	daily := &fakeDailyCloud{dailyBytes: 7.72 * 1024 * 1024 * 1024, dailyPoints: 24}
	w := &Worker{
		Store: s,
		CloudFactory: func(app.AccountGroup) cloud.Client {
			return daily
		},
	}
	event, err := w.dailyTrafficEvent(context.Background(), reportDay)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(event.Text, "CMS 实例昨日消耗流量：\n- ECS-01：7.72 GB") {
		t.Fatalf("daily CMS traffic did not use the direct result:\n%s", event.Text)
	}
	if daily.dailyStartMS != dayStart.UnixMilli() || daily.dailyEndMS != dayStart.AddDate(0, 0, 1).UnixMilli() {
		t.Fatalf("daily CMS traffic used the wrong range: %d - %d", daily.dailyStartMS, daily.dailyEndMS)
	}
}

func TestCreateTaskCompensatesAfterEIPFailure(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SaveGroups([]app.AccountGroup{{GroupKey: "g", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-hongkong", MaxTraffic: 200}}); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"accountGroupKey": "g", "regionId": "cn-hongkong", "instanceType": "ecs.test", "zoneId": "cn-hongkong-b", "imageId": "img", "publicIpMode": "eip", "systemDiskSize": 40}
	if err := s.CreateTask("task-1", "preview", "g", "cn-hongkong", "ecs.test", payload); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueJob("task-1", "create_ecs", "task-1", payload); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimJob(2 * 60 * 1000000000)
	if err != nil || job == nil {
		t.Fatalf("claim: %#v %v", job, err)
	}
	fake := &fakeCloud{assocErr: errors.New("associate failed")}
	w := &Worker{Store: s, Cloud: fake}
	if err := w.execute(context.Background(), job); err == nil {
		t.Fatal("expected create failure")
	}
	if fake.deleted != 1 || fake.unassociated != 1 || fake.released != 1 || fake.cleaned != 1 {
		t.Fatalf("compensation counts: %#v", fake)
	}
	task, _ := s.GetTask("task-1")
	if task.Status != "failed" {
		t.Fatalf("task status: %#v", task)
	}
}

func TestGroupTrafficUsedDoesNotDoubleCountCDTAggregate(t *testing.T) {
	accounts := []app.Account{
		{GroupKey: "group-1", TrafficUsed: 12.5, TrafficAPIStatus: "fallback_cdt"},
		{GroupKey: "group-1", TrafficUsed: 12.5, TrafficAPIStatus: "fallback_cdt"},
	}
	used := groupTrafficUsed(accounts)
	if used["group-1"] != 12.5 {
		t.Fatalf("CDT aggregate was double-counted: %#v", used)
	}
}

func TestProtectionTrafficUsesTheHigherCMSOrCDTValue(t *testing.T) {
	w := &Worker{}
	account := app.Account{RegionID: "cn-hongkong"}
	fake := &fakeCloud{cdtTraffic: 120}
	used, source := w.protectionTraffic(context.Background(), fake, account, 80)
	if used != 120 || source != "CDT" {
		t.Fatalf("higher CDT value was not selected: used=%v source=%s", used, source)
	}
	fake.cdtTraffic = 60
	used, source = w.protectionTraffic(context.Background(), fake, account, 80)
	if used != 80 || source != "CMS" {
		t.Fatalf("higher CMS value was not selected: used=%v source=%s", used, source)
	}
}

func TestRefreshTrafficDoesNotFallbackToCDT(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-hongkong", GroupKey: "g", InstanceID: "i-1"}); err != nil {
		t.Fatal(err)
	}
	accounts, err := s.LoadAccounts(false)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("load account: %#v %v", accounts, err)
	}
	fake := &fakeCloud{cdtTraffic: 120, outboundErr: errors.New("CMS unavailable")}
	w := &Worker{Store: s}
	traffic, status, message, refreshErr := w.refreshTraffic(context.Background(), fake, accounts[0], time.Now())
	if refreshErr == nil || status != "error" || traffic != 0 || !strings.Contains(message, "CMS") {
		t.Fatalf("unexpected CMS failure result: traffic=%v status=%q message=%q err=%v", traffic, status, message, refreshErr)
	}
	if fake.cdtCalls != 0 {
		t.Fatalf("CMS failure unexpectedly queried CDT %d times", fake.cdtCalls)
	}
}

func TestProtectionUsesCDTWhenCMSFails(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fake := &fakeCloud{cdtTraffic: 120}
	w := &Worker{Store: s}
	account := &app.Account{ID: 1, GroupKey: "g", RegionID: "cn-hongkong", InstanceID: "i-1", InstanceStatus: "Running", MaxTraffic: 100}
	if !w.applyTrafficProtection(context.Background(), fake, account, time.Now(), 95, "stop_and_notify", "KeepCharging", 0, false, true, false) {
		t.Fatal("CDT fallback was treated as unavailable")
	}
	if fake.stopped != 1 || account.InstanceStatus != "Stopping" {
		t.Fatalf("CDT threshold did not stop the instance: calls=%d account=%+v", fake.stopped, *account)
	}
}

func TestProtectionPausesWhenBothTrafficAPIsFail(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fake := &fakeCloud{cdtErr: errors.New("CDT unavailable")}
	w := &Worker{Store: s}
	account := &app.Account{ID: 1, GroupKey: "g", RegionID: "cn-hongkong", InstanceID: "i-1", InstanceStatus: "Stopped", MaxTraffic: 100}
	if w.applyTrafficProtection(context.Background(), fake, account, time.Now(), 95, "stop_and_notify", "KeepCharging", 99, false, true, true) {
		t.Fatal("protection should pause when both traffic APIs fail")
	}
	if fake.started != 0 || fake.stopped != 0 || !account.ProtectionSuspended {
		t.Fatalf("unsafe automation occurred: calls=%+v account=%+v", fake, *account)
	}
}

func TestCreateTaskPersistsAccount(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.SaveGroups([]app.AccountGroup{{GroupKey: "g", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-hongkong", MaxTraffic: 200}})
	p := map[string]any{"accountGroupKey": "g", "regionId": "cn-hongkong", "instanceType": "ecs.test", "zoneId": "z", "imageId": "img", "publicIpMode": "ecs_public_ip", "systemDiskSize": 40}
	_ = s.CreateTask("task-2", "preview", "g", "cn-hongkong", "ecs.test", p)
	_ = s.EnqueueJob("task-2", "create_ecs", "task-2", p)
	job, _ := s.ClaimJob(2 * 60 * 1000000000)
	if err := (&Worker{Store: s, Cloud: &fakeCloud{}}).execute(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	accounts, err := s.LoadAccounts(false)
	if err != nil || len(accounts) != 1 || accounts[0].InstanceID != "i-created" {
		t.Fatalf("accounts: %#v %v", accounts, err)
	}
}

func TestDeleteTaskReleasesEIPAndMarksAccount(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-hongkong", GroupKey: "g", InstanceID: "i-delete", InstanceStatus: "Stopped", EIPAllocationID: "eip-1", EIPManaged: true}); err != nil {
		t.Fatal(err)
	}
	accounts, _ := s.LoadAccounts(false)
	if len(accounts) != 1 {
		t.Fatal("account was not inserted")
	}
	id := accounts[0].ID
	if err := s.EnqueueJob("delete-job", "delete_instance", fmt.Sprint(id), map[string]any{"accountId": id}); err != nil {
		t.Fatal(err)
	}
	job, _ := s.ClaimJob(2 * time.Minute)
	if job == nil {
		t.Fatal("job was not claimed")
	}
	fake := &fakeCloud{}
	if err := (&Worker{Store: s, Cloud: fake}).execute(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if fake.deleted != 1 || fake.unassociated != 1 || fake.released != 1 {
		t.Fatalf("release calls: %#v", fake)
	}
	account, err := s.Account(id, true)
	if err == nil || account != nil {
		t.Fatalf("deleted account remained: %#v %v", account, err)
	}
}

func TestDeleteTaskStopsRunningInstanceBeforeDeletion(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "g", InstanceID: "i-running", InstanceStatus: "Releasing"}); err != nil {
		t.Fatal(err)
	}
	accounts, _ := s.LoadAccounts(false)
	if len(accounts) != 1 {
		t.Fatal("account was not inserted")
	}
	if err := s.EnqueueJob("delete-running", "delete_instance", fmt.Sprint(accounts[0].ID), nil); err != nil {
		t.Fatal(err)
	}
	job, _ := s.ClaimJob(2 * time.Minute)
	fake := &fakeCloud{describeStatus: "Running"}
	if err := (&Worker{Store: s, Cloud: fake}).execute(context.Background(), job); err == nil {
		t.Fatal("expected deletion to wait for stop")
	}
	if fake.stopped != 1 || fake.deleted != 0 {
		t.Fatalf("running instance was not safely stopped: %#v", fake)
	}
}

func TestCleanupMissingReleaseFailedOnlyRetiresConfirmedNotFoundInstance(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "g", InstanceID: "i-missing", InstanceStatus: "ReleaseFailed"}); err != nil {
		t.Fatal(err)
	}
	accounts, err := s.LoadAccounts(false)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("load account: %#v %v", accounts, err)
	}
	w := &Worker{Store: s}
	removed, err := w.cleanupMissingReleaseFailed(accounts[0], &cloud.APIError{Code: "InvalidInstanceId.NotFound", HTTPStatus: 404})
	if err != nil || !removed {
		t.Fatalf("missing ReleaseFailed account was not removed: removed=%v err=%v", removed, err)
	}
	if _, err := s.Account(accounts[0].ID, false); err == nil {
		t.Fatal("retired account remained visible")
	}
}

func TestCleanupMissingReleaseFailedDoesNotRetireOnOtherErrors(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "g", InstanceID: "i-unknown", InstanceStatus: "ReleaseFailed"}); err != nil {
		t.Fatal(err)
	}
	accounts, err := s.LoadAccounts(false)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("load account: %#v %v", accounts, err)
	}
	w := &Worker{Store: s}
	removed, err := w.cleanupMissingReleaseFailed(accounts[0], fmt.Errorf("temporary cloud API failure"))
	if err != nil || removed {
		t.Fatalf("non-not-found error changed account: removed=%v err=%v", removed, err)
	}
	account, err := s.Account(accounts[0].ID, false)
	if err != nil || account.InstanceStatus != "ReleaseFailed" {
		t.Fatalf("account was not preserved: %#v %v", account, err)
	}
}

func TestScheduleDue(t *testing.T) {
	now := time.Date(2026, 7, 30, 21, 15, 0, 0, time.Local)
	if !scheduleDue(now, "21:00", "2026-07-29") {
		t.Fatal("schedule should be due")
	}
	if scheduleDue(now, "21:30", "2026-07-29") {
		t.Fatal("future schedule should not be due")
	}
	if scheduleDue(now, "21:00", "2026-07-30") {
		t.Fatal("schedule should run once per day")
	}
}

func TestRunScheduleSkipsWhenDisabled(t *testing.T) {
	fake := &fakeCloud{}
	account := &app.Account{
		ScheduleEnabled:      false,
		ScheduleStartEnabled: true,
		StartTime:            "21:00",
		InstanceStatus:       "Stopped",
	}
	now := time.Date(2026, 7, 30, 21, 15, 0, 0, time.Local)

	(&Worker{}).runSchedule(context.Background(), fake, account, now, "")

	if fake.started != 0 || fake.stopped != 0 {
		t.Fatalf("disabled schedule triggered cloud actions: %#v", fake)
	}
}

func TestScheduledStopBlocksKeepAliveUntilScheduledStart(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	fake := &fakeCloud{}
	w := &Worker{Store: s}
	now := time.Date(2026, 7, 30, 21, 15, 0, 0, time.Local)
	account := &app.Account{
		ScheduleEnabled:       true,
		ScheduleStartEnabled:  true,
		ScheduleStopEnabled:   true,
		StartTime:             "08:00",
		StopTime:              "21:00",
		ScheduleLastStartDate: "2026-07-29",
		InstanceStatus:        "Running",
		ScheduleLastStopDate:  "2026-07-29",
	}
	if err := s.UpsertAccount(*account); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadAccounts(false)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("load account: %#v %v", loaded, err)
	}
	account = &loaded[0]

	w.runSchedule(context.Background(), fake, account, now, "KeepCharging")
	if fake.stopped != 1 || fake.started != 0 || account.InstanceStatus != "Stopping" || !account.ScheduleStopActive || account.ScheduleLastStartDate != "2026-07-30" {
		t.Fatalf("scheduled stop state: calls=%d account=%+v", fake.stopped, *account)
	}

	// Simulate the next poll after ECS reaches Stopped. Keep-alive must not
	// undo the scheduled stop while no scheduled start has occurred.
	account.InstanceStatus = "Stopped"
	w.runCachedAutomation(context.Background(), fake, account, now.Add(10*time.Minute), 95, "stop_and_notify", "KeepCharging", true, false)
	if fake.started != 0 {
		t.Fatalf("keep-alive restarted a scheduled-stop instance: %#v", fake)
	}
}

func TestScheduledStartClearsScheduledStopBlock(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	fake := &fakeCloud{}
	account := &app.Account{
		ScheduleEnabled:       true,
		ScheduleStartEnabled:  true,
		StartTime:             "08:00",
		InstanceStatus:        "Stopped",
		ScheduleStopActive:    true,
		ScheduleLastStartDate: "2026-07-29",
	}
	if err := s.UpsertAccount(*account); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadAccounts(false)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("load account: %#v %v", loaded, err)
	}
	account = &loaded[0]
	w := &Worker{Store: s}
	w.runSchedule(context.Background(), fake, account, time.Date(2026, 7, 30, 8, 15, 0, 0, time.Local), "KeepCharging")
	if fake.started != 1 || account.InstanceStatus != "Starting" || account.ScheduleStopActive {
		t.Fatalf("scheduled start did not clear block: calls=%d account=%+v", fake.started, *account)
	}
}

func TestExternalStopIsRecoveredByKeepAlive(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	fake := &fakeCloud{}
	account := &app.Account{InstanceStatus: "Stopped"}
	if err := s.UpsertAccount(*account); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadAccounts(false)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("load account: %#v %v", loaded, err)
	}
	w := &Worker{Store: s}
	w.runCachedAutomation(context.Background(), fake, &loaded[0], time.Date(2026, 7, 30, 12, 0, 0, 0, time.Local), 95, "stop_and_notify", "KeepCharging", true, false)
	if fake.started != 1 || loaded[0].InstanceStatus != "Starting" {
		t.Fatalf("external stop was not recovered: calls=%d account=%+v", fake.started, loaded[0])
	}
}

func TestKeepAliveRespectsIntentionalStopBlocks(t *testing.T) {
	tests := []struct {
		name               string
		account            app.Account
		requiresProtection bool
	}{
		{name: "traffic threshold", account: app.Account{ScheduleBlockedByTraffic: true}},
		{name: "scheduled stop", account: app.Account{ScheduleEnabled: true, ScheduleStopEnabled: true, ScheduleStopActive: true}},
		{name: "manual stop", account: app.Account{AutoStartBlocked: true}},
		{name: "current traffic threshold", account: app.Account{MaxTraffic: 100, TrafficUsed: 100}, requiresProtection: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if canKeepAlive(test.account, test.requiresProtection) {
				t.Fatal("keep-alive should be blocked")
			}
		})
	}
}

func TestTelegramSettingEnabledAcceptsLegacyValues(t *testing.T) {
	for _, value := range []string{"1", "true", "TRUE", "yes", "on"} {
		if !telegramSettingEnabled(value) {
			t.Fatalf("legacy Telegram setting %q was not recognized", value)
		}
	}
	for _, value := range []string{"", "0", "false", "off"} {
		if telegramSettingEnabled(value) {
			t.Fatalf("disabled Telegram setting %q was recognized as enabled", value)
		}
	}
}

func TestTelegramOffsetResetsWhenBotTokenChanges(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.SetTelegramState("token_fingerprint", telegramTokenFingerprint("old-token")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTelegramState("last_update_id", "918522623"); err != nil {
		t.Fatal(err)
	}
	w := &Worker{Store: s}
	if got := w.telegramOffset("new-token"); got != 0 {
		t.Fatalf("offset from old bot was reused: %d", got)
	}
	if got := w.telegramOffset("new-token"); got != 0 {
		t.Fatalf("reset offset was not persisted: %d", got)
	}
	if err := s.SetTelegramState("last_update_id", "12"); err != nil {
		t.Fatal(err)
	}
	if got := w.telegramOffset("new-token"); got != 12 {
		t.Fatalf("current bot offset was not preserved: %d", got)
	}
}

func TestTelegramStringValuePreservesLargeIDs(t *testing.T) {
	if got := stringValue(float64(5029056175)); got != "5029056175" {
		t.Fatalf("large Telegram ID was formatted incorrectly: %q", got)
	}
}

func TestTelegramTrafficShowsCDTAndInstanceTrafficSeparately(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SaveGroups([]app.AccountGroup{{GroupKey: "g", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-hongkong", MaxTraffic: 190, Remark: "香港"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-hongkong", GroupKey: "g", InstanceID: "i-1", MaxTraffic: 190, TrafficUsed: 2, TrafficAPIStatus: "ok"}); err != nil {
		t.Fatal(err)
	}
	w := &Worker{Store: s, Cloud: &fakeCloud{cdtTraffic: 6.99}}
	body := w.telegramTraffic(context.Background())
	for _, expected := range []string{"CDT 流量：6.99 GB / 190.00 GB", "实例流量：2.00 GB / 190.00 GB", "使用率：4%（取两者较高值）"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("Telegram overview missing %q: %s", expected, body)
		}
	}
}

func TestTelegramMenuUsesCompactDrillDownLayout(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SaveGroups([]app.AccountGroup{{GroupKey: "g", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-hongkong", MaxTraffic: 190, Remark: "香港"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-hongkong", GroupKey: "g", InstanceID: "i-1", InstanceName: "web-01", InstanceStatus: "Running"}); err != nil {
		t.Fatal(err)
	}
	w := &Worker{Store: s}
	if body := w.telegramHome(); !strings.Contains(body, "1 个账号") || !strings.Contains(body, "🟢 运行 1") {
		t.Fatalf("home summary is incomplete: %s", body)
	}

	main := w.mainKeyboard()["inline_keyboard"].([][]map[string]string)
	if len(main) != 2 || len(main[0]) != 2 || main[0][0]["callback_data"] != "m:traffic" || main[0][1]["callback_data"] != "m:list:1" {
		t.Fatalf("unexpected main menu layout: %#v", main)
	}

	instances := w.instancesKeyboard(1)["inline_keyboard"].([][]map[string]string)
	if len(instances) < 2 || !strings.HasPrefix(instances[0][0]["text"], "🟢 ") || instances[0][0]["callback_data"] != "m:inst:1:1" {
		t.Fatalf("instance menu does not expose status and page: %#v", instances)
	}
}

// orphanCloud reports an empty inventory and confirms that every instance is
// gone, which is what the cloud looks like after the user deleted it in the
// Aliyun console.
type orphanCloud struct {
	fakeCloud
	mu sync.Mutex
}

func (o *orphanCloud) DescribeInstances(context.Context, string) ([]cloud.Instance, error) {
	o.mu.Lock()
	o.inventoryCalls++
	o.mu.Unlock()
	return nil, nil
}

func (o *orphanCloud) DescribeInstance(context.Context, string, string) (*cloud.Instance, error) {
	return nil, &cloud.APIError{Code: "InvalidInstanceId.NotFound", HTTPStatus: 404}
}

func TestOrphanRowsOutsideGroupsArePurged(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Only one group is configured, but the database still holds a row for a
	// region that is no longer part of any group.
	if err := s.SaveGroups([]app.AccountGroup{{GroupKey: "g", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", MaxTraffic: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-orphan", GroupKey: "old-group", InstanceID: "i-gone", InstanceStatus: "Running"}); err != nil {
		t.Fatal(err)
	}
	cloudClient := &orphanCloud{}
	w := &Worker{Store: s, CloudFactory: func(app.AccountGroup) cloud.Client { return cloudClient }}
	start := time.Date(2026, 8, 31, 12, 0, 0, 0, time.Local)

	w.reconcileCloudInventory(context.Background(), start)

	rows, err := s.LoadAccounts(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("orphaned instance survived reconciliation: %#v", rows)
	}
}
