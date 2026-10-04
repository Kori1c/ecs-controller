package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type permissionTestTransport func(*http.Request) (*http.Response, error)

func (f permissionTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func permissionResponse(status int, data map[string]any) (*http.Response, error) {
	body, _ := json.Marshal(data)
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
}

func permissionService(fn permissionTestTransport) *Service {
	s := NewRPCService("test-key", "test-secret", "cn-hongkong")
	for _, client := range []*RPCClient{s.ECS, s.VPC, s.EIP, s.CMS, s.CDT, s.BSS} {
		client.HTTPClient = &http.Client{Transport: fn}
	}
	return s
}

func defaultPermissionResponse(t *testing.T, r *http.Request, instances bool) (*http.Response, error) {
	t.Helper()
	action := r.URL.Query().Get("Action")
	switch action {
	case "DescribeRegions":
		return permissionResponse(200, map[string]any{"Regions": map[string]any{"Region": []any{map[string]any{"RegionId": "cn-hongkong"}}}})
	case "DescribeInstances":
		items := []any{}
		if instances {
			items = append(items, map[string]any{"InstanceId": "i-test", "Status": "Running"})
		}
		return permissionResponse(200, map[string]any{"TotalCount": len(items), "Instances": map[string]any{"Instance": items}})
	case "StartInstance", "StopInstance", "DeleteInstance", "CreateVpc":
		if r.URL.Query().Get("DryRun") != "true" {
			t.Errorf("unsafe write request: %s", action)
		}
		return permissionResponse(400, map[string]any{"Code": "DryRunOperation", "Message": "dry run passed"})
	case "DescribeMetricList", "ListCdtInternetTraffic", "DescribeEipAddresses", "QueryAccountBalance", "QueryBillOverview", "DescribeInstanceBill", "DescribeSplitItemBill":
		return permissionResponse(200, map[string]any{"Code": "200", "Datapoints": "[]"})
	default:
		t.Errorf("unexpected or unsafe API: %s", action)
		return permissionResponse(400, map[string]any{"Code": "UnexpectedAPI"})
	}
}

func permissionChecksByID(result AccountTestResult) map[string]PermissionCheck {
	checks := make(map[string]PermissionCheck)
	for _, check := range result.Checks {
		checks[check.ID] = check
	}
	return checks
}

func TestAccountPermissionsSelectsMatchingInstanceState(t *testing.T) {
	s := permissionService(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Query().Get("Action") {
		case "DescribeInstances":
			return permissionResponse(200, map[string]any{"TotalCount": 2, "Instances": map[string]any{"Instance": []any{
				map[string]any{"InstanceId": "i-running", "Status": "Running"},
				map[string]any{"InstanceId": "i-stopped", "Status": "Stopped"},
			}}})
		case "StartInstance":
			if r.URL.Query().Get("InstanceId") != "i-stopped" {
				t.Error("start preflight did not select a stopped instance")
			}
		case "StopInstance":
			if r.URL.Query().Get("InstanceId") != "i-running" {
				t.Error("stop preflight did not select a running instance")
			}
		}
		return defaultPermissionResponse(t, r, true)
	})
	s.TestAccount(context.Background(), "cn-hongkong", "china")
}

func TestAccountPermissionsUsesOnlyReadsAndDocumentedDryRuns(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	s := permissionService(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		calls[r.URL.Query().Get("Action")]++
		mu.Unlock()
		return defaultPermissionResponse(t, r, true)
	})
	result := s.TestAccount(context.Background(), "cn-hongkong", "china")
	checks := permissionChecksByID(result)
	if !result.Success || result.Status != "warning" || result.InstanceCount == nil || *result.InstanceCount != 1 {
		t.Fatalf("unexpected summary: %+v", result)
	}
	for _, id := range []string{"ecs_read", "cms", "cdt", "eip_read", "ecs_start", "ecs_stop", "ecs_delete", "vpc_create", "balance", "bill_overview", "instance_bill", "split_bill"} {
		if checks[id].Status != "ok" {
			t.Errorf("%s = %+v", id, checks[id])
		}
	}
	for _, id := range []string{"ecs_create", "vpc_manage", "eip_manage"} {
		if checks[id].Status != "unchecked" || checks[id].Policy == "" {
			t.Errorf("unsupported write must remain unverified: %+v", checks[id])
		}
	}
	for _, action := range []string{"StartInstance", "StopInstance", "DeleteInstance", "CreateVpc", "DescribeMetricList", "ListCdtInternetTraffic", "DescribeEipAddresses", "QueryAccountBalance", "QueryBillOverview", "DescribeInstanceBill", "DescribeSplitItemBill"} {
		if calls[action] != 1 {
			t.Errorf("%s calls=%d", action, calls[action])
		}
	}
}

func TestAccountPermissionsReportsIndependentMissingPermissions(t *testing.T) {
	denied := map[string]bool{"DescribeMetricList": true, "ListCdtInternetTraffic": true, "StopInstance": true, "QueryAccountBalance": true, "DescribeSplitItemBill": true}
	s := permissionService(func(r *http.Request) (*http.Response, error) {
		if denied[r.URL.Query().Get("Action")] {
			return permissionResponse(403, map[string]any{"Code": "Forbidden.RAM", "Message": "not authorized"})
		}
		return defaultPermissionResponse(t, r, true)
	})
	result := s.TestAccount(context.Background(), "cn-hongkong", "china")
	if result.Status != "missing" || result.Success {
		t.Fatalf("summary=%+v", result)
	}
	checks := permissionChecksByID(result)
	for _, id := range []string{"cms", "cdt", "ecs_stop", "balance", "split_bill"} {
		if checks[id].Status != "missing" || checks[id].Policy == "" {
			t.Errorf("%s=%+v", id, checks[id])
		}
	}
	for _, id := range []string{"ecs_read", "eip_read", "ecs_start", "bill_overview", "instance_bill"} {
		if checks[id].Status != "ok" {
			t.Errorf("independent check %s=%+v", id, checks[id])
		}
	}
}

func TestAccountPermissionsEmptyRegionStillChecksReadPermissions(t *testing.T) {
	s := permissionService(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Query().Get("Action") {
		case "StartInstance", "StopInstance", "DeleteInstance":
			t.Error("must not invent an instance ID")
		}
		return defaultPermissionResponse(t, r, false)
	})
	result := s.TestAccount(context.Background(), "cn-hongkong", "china")
	checks := permissionChecksByID(result)
	for _, id := range []string{"ecs_start", "ecs_stop", "ecs_delete"} {
		if checks[id].Status != "unchecked" {
			t.Errorf("empty region %s=%+v", id, checks[id])
		}
	}
	for _, id := range []string{"cms", "cdt", "eip_read", "balance"} {
		if checks[id].Status != "ok" {
			t.Errorf("read check %s=%+v", id, checks[id])
		}
	}
}

func TestAccountPermissionsDoesNotMarkFailedECSAsConnected(t *testing.T) {
	s := permissionService(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("Action") == "DescribeInstances" {
			return permissionResponse(403, map[string]any{"Code": "Forbidden.RAM"})
		}
		return defaultPermissionResponse(t, r, false)
	})
	result := s.TestAccount(context.Background(), "cn-hongkong", "china")
	if result.Success || result.InstanceCount != nil || permissionChecksByID(result)["ecs_read"].Status != "missing" {
		t.Fatalf("failed ECS request was marked as connected: %+v", result)
	}
}

func TestAccountPermissionsInvalidCredentialsStopsFurtherProbes(t *testing.T) {
	var calls atomic.Int32
	s := permissionService(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return permissionResponse(404, map[string]any{"Code": "InvalidAccessKeyId.NotFound"})
	})
	result := s.TestAccount(context.Background(), "cn-hongkong", "china")
	if result.Status != "error" || result.Success || calls.Load() != 1 {
		t.Fatalf("result=%+v calls=%d", result, calls.Load())
	}
}

func TestAccountPermissionsHonorsBillingSite(t *testing.T) {
	var calls atomic.Int32
	s := permissionService(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Host, "business") {
			calls.Add(1)
			if r.URL.Host != "business.ap-southeast-1.aliyuncs.com" {
				t.Errorf("wrong international billing host: %s", r.URL.Host)
			}
		}
		return defaultPermissionResponse(t, r, false)
	})
	s.TestAccount(context.Background(), "cn-hongkong", "international")
	if calls.Load() != 4 {
		t.Fatalf("billing calls=%d", calls.Load())
	}
}

func TestAccountPermissionsCancellationDoesNotLeakSignedURL(t *testing.T) {
	s := permissionService(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, errors.New("signed URL containing test-key and test-secret")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	result := s.TestAccount(ctx, "cn-hongkong", "china")
	if time.Since(start) > time.Second {
		t.Fatal("canceled request kept running")
	}
	body, _ := json.Marshal(result)
	if strings.Contains(string(body), "test-key") || strings.Contains(string(body), "test-secret") {
		t.Fatal("credentials leaked into response")
	}
}

func TestPermissionErrorsDoNotConfuseBusinessFailuresWithMissingRAM(t *testing.T) {
	for _, test := range []struct{ code, message, status string }{
		{"Forbidden.RAM", "", "missing"},
		{"NoPermission", "", "missing"},
		{"UnauthorizedOperation", "", "missing"},
		{"Forbidden", "not authorized by RAM policy", "missing"},
		{"Forbidden", "service not available", "unavailable"},
		{"Forbidden", "The operation is forbidden due to an invalid parameter", "unavailable"},
		{"Forbidden", "The authorized key has exceeded its quota", "unavailable"},
		{"Forbidden", "RAM policy denies this operation", "missing"},
		{"Forbidden", "RAM user cannot perform this operation", "missing"},
		{"IncorrectInstanceStatus", "", "unchecked"},
		{"InvalidInstanceId.NotFound", "", "unchecked"},
		{"DryRun.InvalidAmount", "", "unchecked"},
		{"KMSKeyUnauthorized", "", "unavailable"},
		{"OperationDenied.NoStock", "", "unavailable"},
		{"InsufficientBalance", "", "unavailable"},
		{"SignatureDoesNotMatch", "", "unavailable"},
	} {
		t.Run(test.code+test.message, func(t *testing.T) {
			check := PermissionCheck{}
			applyPermissionError(&check, &APIError{Code: test.code, Message: test.message, HTTPStatus: 403})
			if check.Status != test.status {
				t.Fatalf("%s: got %s, want %s", test.code, check.Status, test.status)
			}
		})
	}
}

func TestAccountPermissionsOnlyExactDryRunSuccessIsAccepted(t *testing.T) {
	for _, code := range []string{"DryRun.InvalidAmount", "IncorrectInstanceStatus", ""} {
		t.Run(code, func(t *testing.T) {
			s := permissionService(func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Get("Action") == "StopInstance" {
					status := 400
					if code == "" {
						status = 200
					}
					return permissionResponse(status, map[string]any{"Code": code})
				}
				return defaultPermissionResponse(t, r, true)
			})
			result := s.TestAccount(context.Background(), "cn-hongkong", "china")
			if permissionChecksByID(result)["ecs_stop"].Status != "unchecked" {
				t.Fatal("ambiguous DryRun result treated as passed")
			}
		})
	}
}
