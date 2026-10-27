package cloud

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExactInstanceQueryContract(t *testing.T) {
	for _, tc := range []struct {
		name, body     string
		found, missing bool
	}{
		{"target not first", `{"Instances":{"Instance":[{"InstanceId":"i-other"},{"InstanceId":"i-target","Status":"Running"}]},"TotalCount":2}`, true, false},
		{"other only", `{"Instances":{"Instance":[{"InstanceId":"i-other"}]},"TotalCount":1}`, false, false},
		{"empty", `{"Instances":{"Instance":[]},"TotalCount":0}`, false, true},
		{"incomplete", `{"Instances":{"Instance":[]},"TotalCount":1}`, false, false},
		{"malformed", `{}`, false, false},
		{"forbidden", `{"Code":"Forbidden.RAM","Message":"denied"}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				q := r.URL.Query()
				if q.Get("InstanceIds") != `["i-target"]` || q.Has("InstanceId.1") {
					t.Errorf("incorrect filter: %v", q)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tc.body))
			}))
			defer api.Close()
			svc := &Service{ECS: &RPCClient{Endpoint: api.URL, AccessKey: "dummy", Secret: "dummy", HTTPClient: api.Client()}}
			item, err := svc.DescribeInstance(context.Background(), "cn-test", "i-target")
			if tc.found {
				if err != nil || item == nil || item.ID != "i-target" {
					t.Fatalf("wrong result: %+v %v", item, err)
				}
			} else if err == nil || item != nil {
				t.Fatalf("must reject response: %+v %v", item, err)
			}
			if IsNotFound(err) != tc.missing {
				t.Fatalf("incorrect missing classification: %v", err)
			}
			if _, err = svc.DescribeInstance(context.Background(), "cn-test", " "); err == nil || calls != 1 {
				t.Fatal("blank ID should not call API")
			}
		})
	}
}
