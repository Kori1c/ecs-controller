package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// AccountTester checks access without changing cloud resources.
type AccountTester interface {
	TestAccount(context.Context, string, string) AccountTestResult
}

type PermissionCheck struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Required  bool   `json:"required"`
	Policy    string `json:"policy"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	ErrorCode string `json:"errorCode,omitempty"`
}

type AccountTestResult struct {
	Success       bool              `json:"success"`
	Status        string            `json:"status"`
	Message       string            `json:"message"`
	InstanceCount *int              `json:"instanceCount,omitempty"`
	Checks        []PermissionCheck `json:"checks"`
}

func (s *Service) TestAccount(parent context.Context, region, siteType string) AccountTestResult {
	ctx, cancel := context.WithTimeout(parent, 40*time.Second)
	defer cancel()
	checks := []PermissionCheck{
		{ID: "ecs_read", Name: "查看实例", Required: true, Policy: "AliyunECSFullAccess"},
		{ID: "cms", Name: "实例流量（CMS）", Required: true, Policy: "AliyunCloudMonitorReadOnlyAccess"},
		{ID: "cdt", Name: "账号流量（CDT）", Required: true, Policy: "AliyunCDTReadOnlyAccess"},
		{ID: "eip_read", Name: "公网 IP 和带宽", Required: true, Policy: "AliyunCloudMonitorReadOnlyAccess"},
		{ID: "ecs_start", Name: "开机", Required: true, Policy: "AliyunECSFullAccess"},
		{ID: "ecs_stop", Name: "关机 / 流量超限自动关机", Required: true, Policy: "AliyunECSFullAccess"},
		{ID: "ecs_delete", Name: "释放实例", Policy: "AliyunECSFullAccess"},
		{ID: "ecs_create", Name: "创建实例和配置安全组", Policy: "AliyunECSFullAccess"},
		{ID: "vpc_create", Name: "创建专用网络", Policy: "AliyunVPCFullAccess"},
		{ID: "vpc_manage", Name: "创建交换机和清理网络", Policy: "AliyunVPCFullAccess"},
		{ID: "eip_manage", Name: "创建 / 更换 / 释放 EIP", Policy: "AliyunEIPFullAccess"},
		{ID: "balance", Name: "账户余额", Policy: "AliyunBSSReadOnlyAccess"},
		{ID: "bill_overview", Name: "消费概览", Policy: "AliyunBSSReadOnlyAccess"},
		{ID: "instance_bill", Name: "实例账单", Policy: "AliyunBSSReadOnlyAccess"},
		{ID: "split_bill", Name: "账单明细", Policy: "AliyunBSSReadOnlyAccess"},
	}
	for i := range checks {
		checks[i].Status = "unchecked"
		checks[i].Message = "未执行真实创建或删除，请在阿里云确认已添加此权限"
	}
	result := AccountTestResult{Checks: checks}

	discoveryCtx, discoveryCancel := context.WithTimeout(ctx, 12*time.Second)
	regions, regionErr := s.DescribeRegions(discoveryCtx)
	if invalidAccountCredentials(regionErr) {
		applyPermissionError(&checks[0], regionErr)
		for i := 1; i < len(checks); i++ {
			checks[i].Message = "账号验证失败，未继续检查"
		}
		discoveryCancel()
		result.Status, result.Message = "error", "账号密钥无效，请检查 AK ID 和 AK Secret"
		return result
	}
	instances, instanceErr := s.DescribeInstances(discoveryCtx, region)
	discoveryCancel()
	if regionErr != nil {
		applyPermissionError(&checks[0], regionErr)
	} else if instanceErr != nil {
		applyPermissionError(&checks[0], instanceErr)
	} else {
		found := false
		for _, item := range regions {
			if stringValue(item["RegionId"]) == region {
				found = true
				break
			}
		}
		if found {
			checks[0].Status, checks[0].Message = "ok", "可以查看当前区域的实例"
			result.Success = true
		} else {
			checks[0].Status, checks[0].Message = "unavailable", "所选区域不可用，请检查区域设置"
		}
	}
	if instanceErr == nil {
		count := len(instances)
		result.InstanceCount = &count
	}
	if invalidAccountCredentials(instanceErr) {
		for i := 1; i < len(checks); i++ {
			checks[i].Message = "账号验证失败，未继续检查"
		}
		result.Status, result.Message = "error", "账号密钥无效，请检查 AK ID 和 AK Secret"
		return result
	}

	type probe struct {
		index  int
		client *RPCClient
		action string
		params map[string]string
		dryRun bool
	}
	end := time.Now().Add(-90 * time.Second).Truncate(time.Minute).UnixMilli()
	metricParams := map[string]string{
		"Namespace": "acs_ecs_dashboard", "MetricName": "InternetOutRate", "Period": "60",
		"StartTime": fmt.Sprint(end - 10*60*1000), "EndTime": fmt.Sprint(end), "Length": "10",
	}
	if len(instances) > 0 {
		dimensions, _ := json.Marshal(map[string]string{"instanceId": instances[0].ID})
		metricParams["Dimensions"] = string(dimensions)
	}
	bss := s.WithSite(siteType).BSS
	cycle := time.Now().Format("2006-01")
	probes := []probe{
		{index: 1, client: s.CMS, action: "DescribeMetricList", params: metricParams},
		{index: 2, client: s.CDT, action: "ListCdtInternetTraffic"},
		{index: 3, client: s.EIP, action: "DescribeEipAddresses", params: map[string]string{"RegionId": region, "MaxResults": "1"}},
		{index: 8, client: s.VPC, action: "CreateVpc", params: map[string]string{"RegionId": region, "CidrBlock": "192.168.0.0/16", "DryRun": "true"}, dryRun: true},
		{index: 11, client: bss, action: "QueryAccountBalance"},
		{index: 12, client: bss, action: "QueryBillOverview", params: map[string]string{"BillingCycle": cycle, "Granularity": "MONTHLY"}},
		{index: 13, client: bss, action: "DescribeInstanceBill", params: map[string]string{"BillingCycle": cycle, "Granularity": "MONTHLY", "MaxResults": "1"}},
		{index: 14, client: bss, action: "DescribeSplitItemBill", params: map[string]string{"BillingCycle": cycle, "BillingDate": time.Now().Format("2006-01-02"), "Granularity": "DAILY", "MaxResults": "1"}},
	}
	for i, action := range []string{"StartInstance", "StopInstance", "DeleteInstance"} {
		index := 4 + i
		if len(instances) == 0 {
			checks[index].Message = "当前区域没有可供检查的实例，暂时无法验证此权限"
			continue
		}
		instance := instances[0]
		for _, candidate := range instances {
			if (action == "StartInstance" && candidate.Status == "Stopped") || (action == "StopInstance" && candidate.Status == "Running") {
				instance = candidate
				break
			}
		}
		params := map[string]string{"RegionId": region, "InstanceId": instance.ID, "DryRun": "true"}
		if action == "StopInstance" {
			params["StoppedMode"] = "KeepCharging"
		}
		if action == "DeleteInstance" {
			params["Force"] = "true"
		}
		probes = append(probes, probe{index: index, client: s.ECS, action: action, params: params, dryRun: true})
	}

	// Only documented DryRun APIs are allowed here. Never infer write access
	// from a read request or send DryRun to an API that may ignore it.
	var wg sync.WaitGroup
	limit := make(chan struct{}, 6)
	for _, p := range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case limit <- struct{}{}:
				defer func() { <-limit }()
			case <-ctx.Done():
				applyPermissionError(&checks[p.index], ctx.Err())
				return
			}
			probeCtx, probeCancel := context.WithTimeout(ctx, 10*time.Second)
			defer probeCancel()
			_, err := p.client.Call(probeCtx, p.action, p.params)
			var apiErr *APIError
			if p.dryRun {
				if errors.As(err, &apiErr) && apiErr.Code == "DryRunOperation" {
					checks[p.index].Status, checks[p.index].Message = "ok", "权限预检通过，未执行实际操作"
				} else if err == nil {
					checks[p.index].Status, checks[p.index].Message = "unchecked", "接口未返回明确的预检结果，不能确认此权限"
				} else {
					applyPermissionError(&checks[p.index], err)
				}
				return
			}
			if err == nil {
				checks[p.index].Status, checks[p.index].Message = "ok", "可以查询"
			} else {
				applyPermissionError(&checks[p.index], err)
			}
		}()
	}
	wg.Wait()
	missing, pending := 0, 0
	result.Success = true
	for _, check := range checks {
		if check.Required && check.Status != "ok" {
			result.Success = false
		}
		if check.Status == "missing" {
			missing++
		} else if check.Status != "ok" {
			pending++
		}
	}
	result.Status = "warning"
	if missing > 0 {
		result.Status = "missing"
		result.Message = fmt.Sprintf("发现 %d 项缺少权限，请按下方提示补充；可选功能不用时可忽略", missing)
	} else if pending > 0 {
		result.Message = "本次有项目未能确认，请按下方提示处理；未验证不代表没有权限"
	} else {
		result.Status, result.Message = "ok", "本次检查的权限均已通过"
	}
	return result
}

func invalidAccountCredentials(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	code := strings.ToLower(apiErr.Code)
	return strings.Contains(code, "invalidaccesskey") || strings.Contains(code, "signature") || code == "accesskeydisabled"
}

func applyPermissionError(check *PermissionCheck, err error) {
	check.Status = "unavailable"
	check.Message = "暂时无法检查，请检查网络、服务开通情况后重试"
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return
	}
	check.ErrorCode = apiErr.Code
	if invalidAccountCredentials(err) {
		check.Message = "账号密钥无效，请检查 AK ID 和 AK Secret"
		return
	}
	code := strings.ToLower(apiErr.Code)
	message := strings.ToLower(apiErr.Message)
	explicitForbidden := code == "forbidden" && (strings.Contains(message, "permission") || strings.Contains(message, "not authorized") || strings.Contains(message, "unauthorized") || strings.Contains(message, "access denied") || strings.Contains(message, "ram policy") || strings.Contains(message, "ram user"))
	if explicitForbidden || code == "forbidden.ram" || strings.HasPrefix(code, "forbidden.ram.") ||
		code == "nopermission" || strings.HasPrefix(code, "forbidden.nopermission") ||
		code == "accessdenied" || strings.HasPrefix(code, "forbidden.accessdenied") ||
		code == "unauthorized" || code == "unauthorizedoperation" {
		check.Status, check.Message = "missing", "缺少此操作权限，请添加对应权限；如已添加，请检查是否限制了区域或实例"
		return
	}
	if strings.Contains(code, "invalid") || strings.Contains(code, "incorrect") || strings.Contains(code, "notfound") {
		check.Status, check.Message = "unchecked", "当前资源或参数不满足预检条件，暂时无法确认权限"
	}
}
