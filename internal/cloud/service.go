package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Instance struct {
	ID                string `json:"instanceId"`
	Name              string `json:"instanceName"`
	Status            string `json:"status"`
	PublicIP          string `json:"publicIp"`
	PrivateIP         string `json:"privateIp"`
	InstanceType      string `json:"instanceType"`
	CPU               int    `json:"cpu"`
	Memory            int    `json:"memory"`
	OSName            string `json:"osName"`
	InternetBandwidth int    `json:"internetMaxBandwidthOut"`
}

type RunRequest struct {
	RegionID, ZoneID, InstanceType, ImageID, InstanceName string
	VPCID, VSwitchID, SecurityGroupID                     string
	Bandwidth, DiskSize, LoginPort                        int
	DiskCategory, PublicIPMode, Password, ClientToken     string
}

type RunResult struct{ InstanceID, PublicIP string }

type Client interface {
	DescribeRegions(context.Context) ([]map[string]any, error)
	DescribeZones(context.Context, string) ([]map[string]any, error)
	DescribeInstances(context.Context, string) ([]Instance, error)
	DescribeInstance(context.Context, string, string) (*Instance, error)
	StartInstance(context.Context, string, string) error
	StopInstance(context.Context, string, string, string) error
	DeleteInstance(context.Context, string, string) error
	RunInstances(context.Context, RunRequest) (RunResult, error)
	AllocateEIP(context.Context, string) (string, string, error)
	AssociateEIP(context.Context, string, string, string) error
	UnassociateEIP(context.Context, string, string) error
	ReleaseEIP(context.Context, string, string) error
	PrepareNetwork(context.Context, string, string, string, string) (vpcID, vswitchID, securityGroupID string, err error)
	CleanupNetwork(context.Context, string, string, string, string) error
	GetTraffic(context.Context, string) (float64, error)
	GetOutboundTrafficDelta(context.Context, string, string, string, int64, int64) (bytes float64, lastSampleMS int64, points int, metric string, err error)
	GetBilling(context.Context, string, string, string) (balance float64, monthlyCost float64, currency string, err error)
}

// PreflightClient contains the optional inventory APIs used by the create
// preview. Keeping it separate preserves compatibility with small fake cloud
// clients used by monitoring and unit tests.
type PreflightClient interface {
	DescribeInstanceType(context.Context, string, string) (map[string]any, error)
	DescribeAvailableZones(context.Context, string, string, string) ([]map[string]any, error)
	DescribeImagesForArchitecture(context.Context, string, string, string) ([]map[string]any, error)
	GetSystemDiskOptions(context.Context, string, string, string) ([]map[string]any, error)
}

type BillingClient interface {
	GetAccountBalance(context.Context, string) (float64, string, error)
	GetBillOverview(context.Context, string, string) (float64, string, error)
}

type BillingDetail struct {
	Date              string           `json:"date"`
	ProductName       string           `json:"product_name"`
	ProductCode       string           `json:"product_code,omitempty"`
	ProductDetail     string           `json:"product_detail,omitempty"`
	BillingItem       string           `json:"billing_item,omitempty"`
	BillingItemCode   string           `json:"billing_item_code,omitempty"`
	BillingType       string           `json:"billing_type,omitempty"`
	SubscriptionType  string           `json:"subscription_type,omitempty"`
	InstanceID        string           `json:"instance_id,omitempty"`
	Amount            float64          `json:"amount"`
	Currency          string           `json:"currency"`
	Usage             float64          `json:"usage,omitempty"`
	Unit              string           `json:"unit,omitempty"`
	InstanceConfig    string           `json:"instance_config,omitempty"`
	ServicePeriod     int64            `json:"service_period_seconds,omitempty"`
	ServicePeriodUnit string           `json:"service_period_unit,omitempty"`
	CurrentResource   *BillingResource `json:"current_resource,omitempty"`
}

// BillingResource is a current resource snapshot used to put a historical bill
// line in context. It is deliberately kept separate from the bill cache.
type BillingResource struct {
	InstanceID string             `json:"instance_id,omitempty"`
	SystemDisk *BillingSystemDisk `json:"system_disk,omitempty"`
	EIP        *BillingEIP        `json:"eip,omitempty"`
}

type BillingSystemDisk struct {
	Size     int    `json:"size_gib,omitempty"`
	Category string `json:"category,omitempty"`
	Status   string `json:"status,omitempty"`
}

type BillingEIP struct {
	AllocationID string `json:"allocation_id,omitempty"`
	Status       string `json:"status,omitempty"`
	Bandwidth    int    `json:"bandwidth_mbps,omitempty"`
	Count        int    `json:"count,omitempty"`
}

// InstancePublicNetwork is the current EIP binding for one ECS instance.
// It is kept separate from historical billing enrichment so instance lists can
// display the live EIP address and bandwidth without querying disk resources.
type InstancePublicNetwork struct {
	AllocationID string
	Address      string
	Bandwidth    int
}

// BillingDetailClient is optional so existing lightweight cloud fakes do not
// need to implement the detail endpoint used only by the billing modal.
type BillingDetailClient interface {
	GetBillingDetails(context.Context, string, string, string) ([]BillingDetail, error)
}

// BillingResourceClient is optional. It provides current disk and EIP details
// without making an unavailable inventory API block historical billing data.
type BillingResourceClient interface {
	DescribeBillingResources(context.Context, string, []string) (map[string]BillingResource, error)
}

// InstancePublicNetworkClient is optional for compatibility with lightweight
// cloud clients. It supplies the current EIP bandwidth during instance sync.
type InstancePublicNetworkClient interface {
	DescribeInstancePublicNetworks(context.Context, string, []string) (map[string]InstancePublicNetwork, error)
}

// MonthlyTrafficClient queries the current month's instance traffic directly
// from CloudMonitor instead of accumulating samples from the last refresh.
type MonthlyTrafficClient interface {
	GetInstanceMonthlyTraffic(context.Context, string, string, string, int64, int64) (bytes float64, points int, err error)
}

// DailyTrafficClient queries one exact calendar day's instance traffic from
// CloudMonitor instead of deriving a day from month-to-date samples.
type DailyTrafficClient interface {
	GetInstanceDailyTraffic(context.Context, string, string, string, int64, int64) (bytes float64, points int, err error)
}

// NetworkClient allows the caller to authorize the correct remote login port
// without changing the legacy Client interface.
type NetworkClient interface {
	PrepareNetworkForPort(context.Context, string, string, string, string, int) (string, string, string, error)
}

// BandwidthEIPClient preserves the legacy AllocateEIP method while allowing
// ECS creation and replacement to carry the user's selected bandwidth.
type BandwidthEIPClient interface {
	AllocateEIPWithBandwidth(context.Context, string, int) (string, string, error)
}

type Service struct{ ECS, VPC, EIP, CMS, CDT, BSS *RPCClient }

func NewRPCService(accessKey, secret, region string) *Service {
	base := func(product, version, endpoint string) *RPCClient {
		return &RPCClient{HTTPClient: &http.Client{Timeout: 30 * time.Second}, Endpoint: endpoint, Version: version, Product: product, AccessKey: accessKey, Secret: secret}
	}
	return &Service{
		ECS: base("Ecs", "2014-05-26", "https://ecs."+region+".aliyuncs.com/"),
		VPC: base("Vpc", "2016-04-28", "https://vpc."+region+".aliyuncs.com/"),
		EIP: base("Vpc", "2016-04-28", "https://vpc."+region+".aliyuncs.com/"),
		CMS: base("Cms", "2019-01-01", "https://metrics.aliyuncs.com/"),
		CDT: base("CDT", "2021-08-13", "https://cdt.aliyuncs.com/"),
		BSS: base("BssOpenApi", "2017-12-14", "https://business.aliyuncs.com/"),
	}
}

func (s *Service) WithSite(siteType string) *Service {
	copyService := *s
	if siteType == "international" {
		copyService.BSS = &RPCClient{HTTPClient: s.BSS.HTTPClient, Endpoint: "https://business.ap-southeast-1.aliyuncs.com/", Version: "2017-12-14", Product: "BssOpenApi", AccessKey: s.BSS.AccessKey, Secret: s.BSS.Secret}
	}
	return &copyService
}

func (s *Service) DescribeRegions(ctx context.Context) ([]map[string]any, error) {
	result, err := s.ECS.Call(ctx, "DescribeRegions", nil)
	if err != nil {
		return nil, err
	}
	return mapsAt(result, "Regions.Region"), nil
}

func (s *Service) DescribeZones(ctx context.Context, region string) ([]map[string]any, error) {
	result, err := s.ECS.Call(ctx, "DescribeZones", map[string]string{"RegionId": region})
	if err != nil {
		return nil, err
	}
	return mapsAt(result, "Zones.Zone"), nil
}

func (s *Service) DescribeImages(ctx context.Context, region, osKey string) ([]map[string]any, error) {
	return s.describeImages(ctx, region, osKey, "")
}

func (s *Service) DescribeImagesForArchitecture(ctx context.Context, region, osKey, architecture string) ([]map[string]any, error) {
	return s.describeImages(ctx, region, osKey, architecture)
}

func (s *Service) describeImages(ctx context.Context, region, osKey, architecture string) ([]map[string]any, error) {
	params := map[string]string{"RegionId": region, "Status": "Available", "ImageOwnerAlias": "system", "PageSize": "100"}
	if architecture != "" {
		params["Architecture"] = architecture
	}
	result, err := s.ECS.Call(ctx, "DescribeImages", params)
	if err != nil {
		return nil, err
	}
	images := mapsAt(result, "Images.Image")
	key := strings.ToLower(osKey)
	out := make([]map[string]any, 0)
	for _, image := range images {
		text := strings.ToLower(stringValue(image["OSName"]) + " " + stringValue(image["OSNameEn"]) + " " + stringValue(image["ImageName"]))
		if key == "" || ((strings.Contains(key, "debian") && strings.Contains(text, "debian")) || (strings.Contains(key, "ubuntu") && strings.Contains(text, "ubuntu")) || (strings.Contains(key, "windows") && strings.Contains(text, "windows")) || (strings.Contains(key, "alibaba") && strings.Contains(text, "alibaba")) || (strings.Contains(key, "centos") && strings.Contains(text, "centos"))) {
			out = append(out, image)
		}
	}
	return out, nil
}

func (s *Service) DescribeInstanceType(ctx context.Context, region, instanceType string) (map[string]any, error) {
	encoded, _ := json.Marshal([]string{instanceType})
	result, err := s.ECS.Call(ctx, "DescribeInstanceTypes", map[string]string{"RegionId": region, "InstanceTypes": string(encoded)})
	if err != nil {
		return nil, err
	}
	types := mapsAt(result, "InstanceTypes.InstanceType")
	for _, item := range types {
		if stringValue(item["InstanceTypeId"]) == instanceType {
			return item, nil
		}
	}
	if len(types) == 0 {
		return nil, fmt.Errorf("实例规格 %s 不存在或当前账号不可用", instanceType)
	}
	return types[0], nil
}

func (s *Service) DescribeAvailableZones(ctx context.Context, region, instanceType, diskCategory string) ([]map[string]any, error) {
	params := map[string]string{"RegionId": region, "DestinationResource": "Zone", "InstanceType": instanceType, "InstanceChargeType": "PostPaid", "SpotStrategy": "NoSpot", "NetworkCategory": "vpc", "IoOptimized": "optimized", "AvailableResourceCreation": "Instance"}
	if diskCategory != "" {
		params["SystemDisk.Category"] = diskCategory
	}
	result, err := s.ECS.Call(ctx, "DescribeAvailableResource", params)
	if err != nil {
		return nil, err
	}
	zones := mapsAt(result, "AvailableZones.AvailableZone")
	if len(zones) == 0 {
		// Some API responses expose the same records under Zones.Zone.
		zones = mapsAt(result, "Zones.Zone")
	}
	out := make([]map[string]any, 0, len(zones))
	for _, zone := range zones {
		status := strings.ToLower(stringValue(zone["Status"]))
		if status != "" && status != "available" && status != "stock" {
			continue
		}
		out = append(out, zone)
	}
	return out, nil
}

func (s *Service) DescribeImagesWithArchitecture(ctx context.Context, region, osKey, architecture string) ([]map[string]any, error) {
	return s.describeImages(ctx, region, osKey, architecture)
}

func (s *Service) GetSystemDiskOptions(ctx context.Context, region, zone, instanceType string) ([]map[string]any, error) {
	result, err := s.ECS.Call(ctx, "DescribeAvailableResource", map[string]string{"RegionId": region, "ZoneId": zone, "DestinationResource": "SystemDisk", "InstanceType": instanceType, "InstanceChargeType": "PostPaid", "SpotStrategy": "NoSpot", "NetworkCategory": "vpc", "IoOptimized": "optimized"})
	if err != nil {
		return nil, err
	}
	return diskOptionsFromResponse(result), nil
}

func (s *Service) DescribeInstances(ctx context.Context, region string) ([]Instance, error) {
	const pageSize = 100
	const maxPages = 10000
	instances := make([]Instance, 0)
	for page := 1; page <= maxPages; page++ {
		result, err := s.ECS.Call(ctx, "DescribeInstances", map[string]string{
			"RegionId":   region,
			"PageNumber": strconv.Itoa(page),
			"PageSize":   strconv.Itoa(pageSize),
		})
		if err != nil {
			return nil, err
		}
		items := mapsAt(result, "Instances.Instance")
		total := intValue(result["TotalCount"])
		if total > 0 && len(items) == 0 && len(instances) < total {
			return nil, fmt.Errorf("DescribeInstances returned an incomplete page")
		}
		for _, item := range items {
			instances = append(instances, instanceFromMap(item))
		}
		if total > 0 && len(instances) >= total {
			return instances, nil
		}
		if len(items) < pageSize {
			return instances, nil
		}
	}
	return nil, fmt.Errorf("DescribeInstances exceeded the pagination safety limit")
}

func (s *Service) DescribeInstance(ctx context.Context, region, id string) (*Instance, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("查询实例必须指定实例 ID")
	}
	ids, _ := json.Marshal([]string{id})
	result, err := s.ECS.Call(ctx, "DescribeInstances", map[string]string{"RegionId": region, "InstanceIds": string(ids)})
	if err != nil {
		return nil, err
	}
	container, ok := result["Instances"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("DescribeInstances 响应缺少实例列表，无法确认 %s", id)
	}
	items, ok := container["Instance"].([]any)
	if !ok {
		return nil, fmt.Errorf("DescribeInstances 实例列表格式异常，无法确认 %s", id)
	}
	for _, raw := range items {
		item, valid := raw.(map[string]any)
		if valid && stringValue(item["InstanceId"]) == id {
			v := instanceFromMap(item)
			return &v, nil
		}
	}
	if len(items) == 0 && intValue(result["TotalCount"]) == 0 {
		return nil, &APIError{Code: "InvalidInstanceId.NotFound", Message: fmt.Sprintf("instance %s not found", id)}
	}
	return nil, fmt.Errorf("DescribeInstances 未返回目标实例 %s，响应无法用于确认实例不存在", id)
}

func (s *Service) DescribeInstancePublicNetworks(ctx context.Context, region string, instanceIDs []string) (map[string]InstancePublicNetwork, error) {
	resources := make(map[string]InstancePublicNetwork, len(instanceIDs))
	seen := make(map[string]struct{}, len(instanceIDs))
	failures := make([]string, 0)
	for _, instanceID := range instanceIDs {
		instanceID = strings.TrimSpace(instanceID)
		if instanceID == "" {
			continue
		}
		if _, ok := seen[instanceID]; ok {
			continue
		}
		seen[instanceID] = struct{}{}
		eips, err := s.EIP.Call(ctx, "DescribeEipAddresses", map[string]string{
			"RegionId":     region,
			"InstanceId":   instanceID,
			"InstanceType": "EcsInstance",
			"MaxResults":   "100",
		})
		if err != nil {
			failures = append(failures, instanceID+": "+err.Error())
			continue
		}
		items := mapsAt(eips, "EipAddresses.EipAddress")
		if len(items) == 0 {
			continue
		}
		first := items[0]
		allocationID := stringValue(first["AllocationId"])
		if allocationID == "" {
			continue
		}
		resources[instanceID] = InstancePublicNetwork{
			AllocationID: allocationID,
			Address:      firstString(first, "IpAddress", "EipAddress", "Address"),
			Bandwidth:    intValue(first["Bandwidth"]),
		}
	}
	if len(failures) > 0 {
		return resources, fmt.Errorf("current EIP lookup: %s", strings.Join(failures, "; "))
	}
	return resources, nil
}

func (s *Service) DescribeBillingResources(ctx context.Context, region string, instanceIDs []string) (map[string]BillingResource, error) {
	resources := make(map[string]BillingResource, len(instanceIDs))
	seen := make(map[string]struct{}, len(instanceIDs))
	failures := make([]string, 0)

	for _, instanceID := range instanceIDs {
		instanceID = strings.TrimSpace(instanceID)
		if instanceID == "" {
			continue
		}
		if _, ok := seen[instanceID]; ok {
			continue
		}
		seen[instanceID] = struct{}{}

		resource := BillingResource{InstanceID: instanceID}
		disks, diskErr := s.ECS.Call(ctx, "DescribeDisks", map[string]string{
			"RegionId":   region,
			"InstanceId": instanceID,
			"MaxResults": "100",
		})
		if diskErr != nil {
			failures = append(failures, "DescribeDisks: "+diskErr.Error())
		} else {
			for _, disk := range mapsAt(disks, "Disks.Disk") {
				if !strings.EqualFold(stringValue(disk["Type"]), "system") {
					continue
				}
				resource.SystemDisk = &BillingSystemDisk{
					Size:     intValue(disk["Size"]),
					Category: stringValue(disk["Category"]),
					Status:   stringValue(disk["Status"]),
				}
				break
			}
		}

		eips, eipErr := s.EIP.Call(ctx, "DescribeEipAddresses", map[string]string{
			"RegionId":     region,
			"InstanceId":   instanceID,
			"InstanceType": "EcsInstance",
			"MaxResults":   "100",
		})
		if eipErr != nil {
			failures = append(failures, "DescribeEipAddresses: "+eipErr.Error())
		} else {
			eipItems := mapsAt(eips, "EipAddresses.EipAddress")
			if len(eipItems) > 0 {
				first := eipItems[0]
				resource.EIP = &BillingEIP{
					AllocationID: stringValue(first["AllocationId"]),
					Status:       stringValue(first["Status"]),
					Bandwidth:    intValue(first["Bandwidth"]),
					Count:        len(eipItems),
				}
			}
			for _, eip := range eipItems {
				allocationID := stringValue(eip["AllocationId"])
				if allocationID == "" {
					continue
				}
				resources[allocationID] = BillingResource{
					InstanceID: instanceID,
					EIP: &BillingEIP{
						AllocationID: allocationID,
						Status:       stringValue(eip["Status"]),
						Bandwidth:    intValue(eip["Bandwidth"]),
						Count:        1,
					},
				}
			}
		}
		resources[instanceID] = resource
	}

	if len(failures) > 0 {
		return resources, fmt.Errorf("current billing resource lookup: %s", strings.Join(failures, "; "))
	}
	return resources, nil
}

func (s *Service) StartInstance(ctx context.Context, region, id string) error {
	_, err := s.ECS.Call(ctx, "StartInstance", map[string]string{"RegionId": region, "InstanceId": id})
	return err
}

func (s *Service) StopInstance(ctx context.Context, region, id, mode string) error {
	_, err := s.ECS.Call(ctx, "StopInstance", map[string]string{"RegionId": region, "InstanceId": id, "StoppedMode": mode})
	return err
}

func (s *Service) DeleteInstance(ctx context.Context, region, id string) error {
	_, err := s.ECS.Call(ctx, "DeleteInstance", map[string]string{"RegionId": region, "InstanceId": id, "Force": "true"})
	return err
}

func (s *Service) RunInstances(ctx context.Context, req RunRequest) (RunResult, error) {
	bandwidth := req.Bandwidth
	allocatePublicIP := "true"
	if req.PublicIPMode == "eip" {
		bandwidth = 0
		allocatePublicIP = "false"
	}
	clientToken := req.ClientToken
	if clientToken == "" {
		clientToken = req.InstanceName
	}
	p := map[string]string{"RegionId": req.RegionID, "ZoneId": req.ZoneID, "InstanceType": req.InstanceType, "ImageId": req.ImageID, "InstanceName": req.InstanceName, "VSwitchId": req.VSwitchID, "SecurityGroupId.1": req.SecurityGroupID, "InternetMaxBandwidthOut": strconv.Itoa(bandwidth), "AllocatePublicIp": allocatePublicIP, "InternetChargeType": "PayByTraffic", "InstanceChargeType": "PostPaid", "Password": req.Password, "Amount": "1", "ClientToken": clientToken, "IoOptimized": "optimized", "DeletionProtection": "false"}
	if req.VPCID != "" {
		p["VpcId"] = req.VPCID
	}
	if req.DiskCategory != "" {
		p["SystemDisk.Category"] = req.DiskCategory
	}
	if req.DiskSize > 0 {
		p["SystemDisk.Size"] = strconv.Itoa(req.DiskSize)
	}
	result, err := s.ECS.Call(ctx, "RunInstances", p)
	if err != nil {
		return RunResult{}, err
	}
	id := stringValue(result["InstanceId"])
	if id == "" {
		if ids := mapsAt(result, "InstanceIdSet.InstanceId"); len(ids) > 0 {
			id = stringValue(ids[0]["InstanceId"])
		}
	}
	return RunResult{InstanceID: id, PublicIP: stringValue(result["PublicIpAddress"])}, nil
}

func (s *Service) AllocateEIP(ctx context.Context, region string) (string, string, error) {
	return s.AllocateEIPWithBandwidth(ctx, region, 100)
}

func (s *Service) AllocateEIPWithBandwidth(ctx context.Context, region string, bandwidth int) (string, string, error) {
	if bandwidth < 1 {
		bandwidth = 1
	}
	result, err := s.EIP.Call(ctx, "AllocateEipAddress", map[string]string{"RegionId": region, "InternetChargeType": "PayByTraffic", "Bandwidth": strconv.Itoa(bandwidth)})
	return stringValue(result["AllocationId"]), stringValue(result["EipAddress"]), err
}
func (s *Service) AssociateEIP(ctx context.Context, region, allocationID, instanceID string) error {
	_, err := s.EIP.Call(ctx, "AssociateEipAddress", map[string]string{"RegionId": region, "AllocationId": allocationID, "InstanceId": instanceID, "InstanceType": "Ecs"})
	return err
}
func (s *Service) UnassociateEIP(ctx context.Context, region, allocationID string) error {
	_, err := s.EIP.Call(ctx, "UnassociateEipAddress", map[string]string{"RegionId": region, "AllocationId": allocationID})
	return err
}
func (s *Service) ReleaseEIP(ctx context.Context, region, allocationID string) error {
	_, err := s.EIP.Call(ctx, "ReleaseEipAddress", map[string]string{"RegionId": region, "AllocationId": allocationID})
	return err
}

func (s *Service) GetTraffic(ctx context.Context, targetRegion string) (float64, error) {
	result, err := s.CDT.Call(ctx, "ListCdtInternetTraffic", nil)
	if err != nil {
		return 0, err
	}
	details, ok := result["TrafficDetails"].([]any)
	if !ok {
		if raw, isString := result["TrafficDetails"].(string); isString {
			_ = json.Unmarshal([]byte(raw), &details)
		}
	}
	if !ok && len(details) == 0 {
		return 0, fmt.Errorf("CDT response missing TrafficDetails")
	}
	targetOverseas := overseasRegion(targetRegion)
	var bytes float64
	for _, raw := range details {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if overseasRegion(stringValue(item["BusinessRegionId"])) == targetOverseas {
			bytes += floatValue(item["Traffic"])
		}
	}
	return bytes / (1024 * 1024 * 1024), nil
}

func (s *Service) GetBilling(ctx context.Context, siteType, instanceID, billingCycle string) (float64, float64, string, error) {
	bss := s.WithSite(siteType).BSS
	balance, currency, err := s.GetAccountBalance(ctx, siteType)
	if err != nil {
		return 0, 0, "", err
	}
	billResult, err := bss.Call(ctx, "DescribeInstanceBill", map[string]string{"BillingCycle": billingCycle, "InstanceID": instanceID, "Granularity": "MONTHLY"})
	if err != nil {
		return balance, 0, currency, err
	}
	items := mapsAt(billResult, "Data.Items.Item")
	var total float64
	for _, item := range items {
		total += floatValue(item["PretaxAmount"])
	}
	return balance, total, currency, nil
}

// GetBillingDetails selects one daily bill view. Split bills remain preferred
// when available because they include split resources; the current instance
// bill is the billing-item fallback for accounts without split-bill rows.
func (s *Service) GetBillingDetails(ctx context.Context, siteType, billingCycle, requestedDate string) ([]BillingDetail, error) {
	if _, err := time.Parse("2006-01", billingCycle); err != nil {
		return nil, fmt.Errorf("invalid billing cycle %q", billingCycle)
	}
	if parsedDate, err := time.Parse("2006-01-02", requestedDate); err != nil || parsedDate.Format("2006-01") != billingCycle {
		return nil, fmt.Errorf("invalid billing date %q for billing cycle %q", requestedDate, billingCycle)
	}

	splitDetails, splitErr := s.getSplitItemBillingDetails(ctx, siteType, billingCycle, requestedDate)
	if splitErr == nil && len(splitDetails) > 0 {
		return splitDetails, nil
	}

	details, instanceErr := s.getInstanceBillingItemDetails(ctx, siteType, billingCycle, requestedDate)
	if instanceErr == nil && len(details) > 0 {
		return details, nil
	}
	if instanceErr != nil {
		return nil, instanceErr
	}
	// A successful instance-bill request with no rows is still a valid empty result.
	return details, nil
}

func (s *Service) getInstanceBillingItemDetails(ctx context.Context, siteType, billingCycle, requestedDate string) ([]BillingDetail, error) {
	return s.getPagedBillingDetails(ctx, siteType, "DescribeInstanceBill", map[string]string{
		"BillingCycle":     billingCycle,
		"BillingDate":      requestedDate,
		"Granularity":      "DAILY",
		"IsBillingItem":    "true",
		"IsHideZeroCharge": "true",
		"MaxResults":       "300",
	}, requestedDate)
}

func (s *Service) getSplitItemBillingDetails(ctx context.Context, siteType, billingCycle, requestedDate string) ([]BillingDetail, error) {
	return s.getPagedBillingDetails(ctx, siteType, "DescribeSplitItemBill", map[string]string{
		"BillingCycle":     billingCycle,
		"BillingDate":      requestedDate,
		"Granularity":      "DAILY",
		"IsHideZeroCharge": "true",
		"MaxResults":       "300",
	}, requestedDate)
}

func (s *Service) getPagedBillingDetails(ctx context.Context, siteType, action string, params map[string]string, fallbackDate string) ([]BillingDetail, error) {
	bss := s.WithSite(siteType).BSS
	details := make([]BillingDetail, 0)
	for page := 0; page < 100; page++ {
		result, err := bss.Call(ctx, action, params)
		if err != nil {
			return nil, err
		}
		items := mapsAt(result, "Data.Items")
		if len(items) == 0 {
			items = mapsAt(result, "Data.Items.Item")
		}
		for _, item := range items {
			details = append(details, billingDetailFromMap(item, "", billingCurrency(siteType), fallbackDate))
		}

		data, _ := result["Data"].(map[string]any)
		nextToken := stringValue(data["NextToken"])
		if nextToken == "" {
			return sortBillingDetails(details), nil
		}
		params["NextToken"] = nextToken
	}
	return nil, fmt.Errorf("%s pagination exceeded 100 pages", action)
}

func billingCurrency(siteType string) string {
	if siteType == "international" {
		return "USD"
	}
	return "CNY"
}

func sortBillingDetails(details []BillingDetail) []BillingDetail {
	sort.SliceStable(details, func(i, j int) bool {
		if details[i].Date == details[j].Date {
			if details[i].ProductName == details[j].ProductName {
				return details[i].BillingItem < details[j].BillingItem
			}
			return details[i].ProductName < details[j].ProductName
		}
		return details[i].Date > details[j].Date
	})
	return details
}

func billingDetailFromMap(item map[string]any, fallbackInstanceID, fallbackCurrency, fallbackDate string) BillingDetail {
	amount := floatValue(item["PretaxAmount"])
	if _, exists := item["PretaxAmount"]; !exists {
		amount = floatValue(item["PretaxGrossAmount"])
	}
	productCode := firstString(item, "ProductCode", "ProductCodeId")
	productName := firstString(item, "ProductName", "ProductNameEn", "ProductNameZh", "BillingItemName", "BillingItem")
	if productName == "" {
		productName = productCode
	}
	if productName == "" {
		productName = "其他费用"
	}
	instanceID := firstString(item, "InstanceID", "InstanceId")
	if instanceID == "" {
		instanceID = fallbackInstanceID
	}
	currency := firstString(item, "Currency")
	if currency == "" {
		currency = fallbackCurrency
	}
	if currency == "" {
		currency = "CNY"
	}
	productDetail := firstString(item, "SplitProductDetail", "ProductDetail")
	billingItem := firstString(item, "BillingItem", "BillingItemName")
	billingItemCode := firstString(item, "BillingItemCode")
	billingType := firstString(item, "BillingType")
	subscriptionType := firstString(item, "SubscriptionType")
	usage := floatValue(item["Usage"])
	if usage == 0 {
		usage = floatValue(item["UsageAmount"])
	}
	if usage == 0 {
		usage = floatValue(item["Quantity"])
	}
	date := billingDate(firstString(item, "BillingDate", "BillingDateString", "Date"))
	if date == "" {
		date = fallbackDate
	}
	return BillingDetail{
		Date:              date,
		ProductName:       productName,
		ProductCode:       productCode,
		ProductDetail:     productDetail,
		BillingItem:       billingItem,
		BillingItemCode:   billingItemCode,
		BillingType:       billingType,
		SubscriptionType:  subscriptionType,
		InstanceID:        instanceID,
		Amount:            amount,
		Currency:          currency,
		Usage:             usage,
		Unit:              firstString(item, "Unit", "UsageUnit"),
		InstanceConfig:    firstString(item, "InstanceConfig"),
		ServicePeriod:     int64(floatValue(item["ServicePeriod"])),
		ServicePeriodUnit: firstString(item, "ServicePeriodUnit"),
	}
}

func billingDate(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	for _, layout := range []string{
		"2006-01-02",
		"2006-01-02 15:04:05",
		time.RFC3339,
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.Format("2006-01-02")
		}
	}
	if len(value) >= len("2006-01-02") {
		candidate := value[:len("2006-01-02")]
		if _, err := time.Parse("2006-01-02", candidate); err == nil {
			return candidate
		}
	}
	return value
}

func (s *Service) GetAccountBalance(ctx context.Context, siteType string) (float64, string, error) {
	bss := s.WithSite(siteType).BSS
	result, err := bss.Call(ctx, "QueryAccountBalance", nil)
	if err != nil {
		return 0, "", err
	}
	data, _ := result["Data"].(map[string]any)
	currency := stringValue(data["Currency"])
	if currency == "" {
		currency = "CNY"
	}
	return floatValue(data["AvailableAmount"]), currency, nil
}

func (s *Service) GetBillOverview(ctx context.Context, siteType, billingCycle string) (float64, string, error) {
	bss := s.WithSite(siteType).BSS
	result, err := bss.Call(ctx, "QueryBillOverview", map[string]string{"BillingCycle": billingCycle, "Granularity": "MONTHLY"})
	if err != nil {
		return 0, "", err
	}
	data, _ := result["Data"].(map[string]any)
	total := floatValue(data["TotalCost"])
	if total == 0 {
		total = floatValue(data["TotalAmount"])
	}
	if total == 0 {
		for _, item := range mapsAt(result, "Data.Items.Item") {
			total += floatValue(item["PretaxAmount"])
		}
	}
	currency := stringValue(data["Currency"])
	if currency == "" {
		currency = "CNY"
	}
	return total, currency, nil
}

func (s *Service) GetOutboundTrafficDelta(ctx context.Context, region, instanceID, publicIP string, startMS, endMS int64) (float64, int64, int, string, error) {
	if endMS <= startMS {
		return 0, startMS, 0, "", nil
	}
	type candidate struct {
		name       string
		dimensions string
	}
	candidates := []candidate{{"InternetOutRate", `{"instanceId":"` + instanceID + `"}`}}
	if publicIP != "" {
		candidates = append([]candidate{{"VPC_PublicIP_InternetOutRate", `{"instanceId":"` + instanceID + `","ip":"` + publicIP + `"}`}}, candidates...)
	}
	var lastErr error
	for _, candidate := range candidates {
		result, err := s.CMS.Call(ctx, "DescribeMetricList", map[string]string{"Namespace": "acs_ecs_dashboard", "MetricName": candidate.name, "Period": "60", "StartTime": strconv.FormatInt(startMS, 10), "EndTime": strconv.FormatInt(endMS, 10), "Dimensions": candidate.dimensions, "Length": "1440"})
		if err != nil {
			lastErr = err
			continue
		}
		points, err := metricPoints(result["Datapoints"])
		if err != nil {
			lastErr = err
			continue
		}
		var bytes float64
		var last int64
		count := 0
		for _, point := range points {
			ts := int64(floatValue(point["timestamp"]))
			if ts <= startMS || ts > endMS {
				continue
			}
			rate := floatValue(point["Average"])
			if rate == 0 {
				rate = floatValue(point["Maximum"])
			}
			if rate == 0 {
				rate = floatValue(point["Minimum"])
			}
			if rate < 0 {
				rate = 0
			}
			bytes += rate * 60 / 8
			if ts > last {
				last = ts
			}
			count++
		}
		// A successful CMS response with no points means this metric/dimension
		// did not produce a usable sample. Let the next candidate or CDT fallback
		// handle it instead of recording a false zero.
		if count > 0 {
			return bytes, last, count, candidate.name, nil
		}
		lastErr = fmt.Errorf("%w: CMS metric %s returned no datapoints", ErrMetricNoData, candidate.name)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("CMS returned no usable datapoints")
	}
	return 0, startMS, 0, "", lastErr
}

func (s *Service) GetInstanceMonthlyTraffic(ctx context.Context, region, instanceID, publicIP string, startMS, endMS int64) (float64, int, error) {
	return s.getInstanceTrafficWindow(ctx, region, instanceID, publicIP, startMS, endMS, "1440")
}

func (s *Service) GetInstanceDailyTraffic(ctx context.Context, region, instanceID, publicIP string, startMS, endMS int64) (float64, int, error) {
	return s.getInstanceTrafficWindow(ctx, region, instanceID, publicIP, startMS, endMS, "48")
}

func (s *Service) getInstanceTrafficWindow(ctx context.Context, region, instanceID, publicIP string, startMS, endMS int64, length string) (float64, int, error) {
	if endMS <= startMS {
		return 0, 0, nil
	}
	type candidate struct {
		name       string
		dimensions string
	}
	candidates := []candidate{{"InternetOutRate", `{"instanceId":"` + instanceID + `"}`}}
	if publicIP != "" {
		candidates = append([]candidate{{"VPC_PublicIP_InternetOutRate", `{"instanceId":"` + instanceID + `","ip":"` + publicIP + `"}`}}, candidates...)
	}
	var lastErr error
	const periodSeconds = 3600
	for _, candidate := range candidates {
		result, err := s.CMS.Call(ctx, "DescribeMetricList", map[string]string{
			"Namespace":  "acs_ecs_dashboard",
			"MetricName": candidate.name,
			"Period":     strconv.Itoa(periodSeconds),
			"StartTime":  strconv.FormatInt(startMS, 10),
			"EndTime":    strconv.FormatInt(endMS, 10),
			"Dimensions": candidate.dimensions,
			"Length":     length,
		})
		if err != nil {
			lastErr = err
			continue
		}
		points, err := metricPoints(result["Datapoints"])
		if err != nil {
			lastErr = err
			continue
		}
		var bytes float64
		count := 0
		for _, point := range points {
			ts := int64(floatValue(point["timestamp"]))
			if ts <= startMS || ts > endMS {
				continue
			}
			rate := floatValue(point["Average"])
			if rate == 0 {
				rate = floatValue(point["Maximum"])
			}
			if rate == 0 {
				rate = floatValue(point["Minimum"])
			}
			if rate < 0 {
				rate = 0
			}
			bytes += rate * periodSeconds / 8
			count++
		}
		if count > 0 {
			return bytes, count, nil
		}
		lastErr = fmt.Errorf("%w: CMS metric %s returned no traffic datapoints", ErrMetricNoData, candidate.name)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w: CMS returned no traffic datapoints", ErrMetricNoData)
	}
	return 0, 0, lastErr
}

func metricPoints(value any) ([]map[string]any, error) {
	if raw, ok := value.(string); ok {
		var points []map[string]any
		if err := json.Unmarshal([]byte(raw), &points); err != nil {
			return nil, err
		}
		return points, nil
	}
	if raw, ok := value.([]any); ok {
		points := make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			if point, ok := item.(map[string]any); ok {
				points = append(points, point)
			}
		}
		return points, nil
	}
	return nil, fmt.Errorf("CMS response missing Datapoints")
}

func overseasRegion(region string) bool {
	region = strings.ToLower(strings.TrimSpace(region))
	if region == "" {
		return false
	}
	// Alibaba treats Hong Kong as an overseas CDT billing region even though
	// its identifier starts with cn-.
	return region == "cn-hongkong" || !strings.HasPrefix(region, "cn-")
}
func floatValue(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case string:
		n, _ := strconv.ParseFloat(v, 64)
		return n
	}
	return 0
}

func (s *Service) PrepareNetwork(ctx context.Context, region, cidr, zone, clientCIDR string) (string, string, string, error) {
	return s.PrepareNetworkForPort(ctx, region, cidr, zone, clientCIDR, 22)
}

func (s *Service) PrepareNetworkForPort(ctx context.Context, region, cidr, zone, clientCIDR string, port int) (string, string, string, error) {
	// Network provisioning is deliberately explicit. Existing resources can be
	// selected by the caller; this path creates a minimal isolated network.
	vpc, err := s.VPC.Call(ctx, "CreateVpc", map[string]string{"RegionId": region, "CidrBlock": cidr, "VpcName": "ecs-controller"})
	if err != nil {
		return "", "", "", err
	}
	vpcID := stringValue(vpc["VpcId"])
	vs, err := s.VPC.Call(ctx, "CreateVSwitch", map[string]string{"RegionId": region, "VpcId": vpcID, "ZoneId": zone, "CidrBlock": "192.168.0.0/24", "VSwitchName": "ecs-controller"})
	if err != nil {
		return vpcID, "", "", err
	}
	vsID := stringValue(vs["VSwitchId"])
	sg, err := s.ECS.Call(ctx, "CreateSecurityGroup", map[string]string{"RegionId": region, "VpcId": vpcID, "SecurityGroupName": "ecs-controller"})
	if err != nil {
		return vpcID, vsID, "", err
	}
	sgID := stringValue(sg["SecurityGroupId"])
	if port <= 0 {
		port = 22
	}
	if clientCIDR != "" && clientCIDR != "0.0.0.0/0" {
		if _, authErr := s.ECS.Call(ctx, "AuthorizeSecurityGroup", map[string]string{"RegionId": region, "SecurityGroupId": sgID, "IpProtocol": "tcp", "PortRange": strconv.Itoa(port) + "/" + strconv.Itoa(port), "SourceCidrIp": clientCIDR, "Policy": "accept", "Priority": "1", "Description": "ecs-controller remote access"}); authErr != nil {
			return vpcID, vsID, sgID, authErr
		}
	}
	return vpcID, vsID, sgID, nil
}

func (s *Service) CleanupNetwork(ctx context.Context, region, vpcID, vswitchID, securityGroupID string) error {
	var firstErr error
	if securityGroupID != "" {
		if _, err := s.ECS.Call(ctx, "DeleteSecurityGroup", map[string]string{"RegionId": region, "SecurityGroupId": securityGroupID}); err != nil && !IsNotFound(err) {
			firstErr = err
		}
	}
	if vswitchID != "" {
		if _, err := s.VPC.Call(ctx, "DeleteVSwitch", map[string]string{"RegionId": region, "VSwitchId": vswitchID}); err != nil && !IsNotFound(err) && firstErr == nil {
			firstErr = err
		}
	}
	if vpcID != "" {
		if _, err := s.VPC.Call(ctx, "DeleteVpc", map[string]string{"RegionId": region, "VpcId": vpcID}); err != nil && !IsNotFound(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func mapsAt(root map[string]any, path string) []map[string]any {
	var current any = root
	for _, part := range splitPath(path) {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[part]
	}
	if m, ok := current.(map[string]any); ok {
		if items, ok := m["items"].([]any); ok {
			return anyMaps(items)
		}
		return []map[string]any{m}
	}
	if items, ok := current.([]any); ok {
		return anyMaps(items)
	}
	return nil
}
func splitPath(path string) []string {
	var out []string
	for _, p := range stringsSplit(path, ".") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
func stringsSplit(s, sep string) []string {
	var out []string
	for s != "" {
		i := indexString(s, sep)
		if i < 0 {
			return append(out, s)
		}
		out = append(out, s[:i])
		s = s[i+len(sep):]
	}
	return out
}
func indexString(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
func anyMaps(items []any) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, v := range items {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func diskOptionsFromResponse(root map[string]any) []map[string]any {
	options := make([]map[string]any, 0)
	seen := map[string]bool{}
	add := func(category string, min, max int, raw map[string]any) {
		category = strings.TrimSpace(category)
		if category == "" || seen[category] || !isDiskCategory(category) {
			return
		}
		if min <= 0 {
			min = 20
		}
		if max <= 0 {
			max = 32768
		}
		seen[category] = true
		options = append(options, map[string]any{"value": category, "label": diskCategoryLabel(category), "min": min, "max": max, "unit": "GB", "raw": raw})
	}
	for _, item := range collectMaps(root, "AvailableResource") {
		category := firstString(item, "Category", "Value", "SystemDiskCategory")
		if nested, ok := item["SystemDisk"].(map[string]any); ok && category == "" {
			category = stringValue(nested["Category"])
		}
		add(category, firstInt(item, "MinSystemDiskSize", "MinSize", "Min"), firstInt(item, "MaxSystemDiskSize", "MaxSize", "Max"), item)
	}
	for _, item := range collectMaps(root, "SupportedResource") {
		add(firstString(item, "Category", "Value", "Name"), firstInt(item, "MinSize", "MinSystemDiskSize", "Min"), firstInt(item, "MaxSize", "MaxSystemDiskSize", "Max"), item)
	}
	for _, item := range collectMaps(root, "DiskCategory") {
		category := firstString(item, "Category", "Value", "Name")
		add(category, firstInt(item, "MinSize", "MinSystemDiskSize", "Min"), firstInt(item, "MaxSize", "MaxSystemDiskSize", "Max"), item)
	}
	return options
}

func isDiskCategory(category string) bool {
	category = strings.ToLower(category)
	return category == "cloud" || strings.HasPrefix(category, "cloud_") || strings.HasPrefix(category, "ephemeral") || strings.Contains(category, "disk")
}

func collectMaps(value any, key string) []map[string]any {
	result := make([]map[string]any, 0)
	var visit func(any)
	visit = func(current any) {
		switch item := current.(type) {
		case map[string]any:
			for name, child := range item {
				if name == key {
					switch matched := child.(type) {
					case map[string]any:
						result = append(result, matched)
					case []any:
						for _, entry := range matched {
							if mapped, ok := entry.(map[string]any); ok {
								result = append(result, mapped)
							}
						}
					}
				}
				visit(child)
			}
		case []any:
			for _, child := range item {
				visit(child)
			}
		}
	}
	visit(value)
	return result
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(m[key]); value != "" && value != "<nil>" {
			return value
		}
	}
	return ""
}

func firstInt(m map[string]any, keys ...string) int {
	for _, key := range keys {
		value := intValue(m[key])
		if value > 0 {
			return value
		}
	}
	return 0
}

func diskCategoryLabel(category string) string {
	labels := map[string]string{
		"cloud_essd_entry": "ESSD Entry",
		"cloud_essd":       "ESSD",
		"cloud_auto":       "ESSD AutoPL",
		"cloud_ssd":        "SSD 云盘",
		"cloud_efficiency": "高效云盘",
		"cloud":            "普通云盘",
	}
	if label := labels[category]; label != "" {
		return label
	}
	// Keep newly introduced Alibaba disk categories selectable instead of
	// rendering an option with an empty label.
	return category
}

func instanceFromMap(m map[string]any) Instance {
	publicIP := firstIP(m, "PublicIpAddress", "EipAddress", "EIPAddress")
	privateIP := firstNestedIP(m, "VpcAttributes", "PrivateIpAddress", "PrivateIpAddressSet")
	return Instance{
		ID:                stringValue(m["InstanceId"]),
		Name:              stringValue(m["InstanceName"]),
		Status:            stringValue(m["Status"]),
		PublicIP:          publicIP,
		PrivateIP:         privateIP,
		InstanceType:      firstString(m, "InstanceType", "InstanceTypeId"),
		CPU:               firstInt(m, "Cpu", "CpuCoreCount", "CPU", "CoreCount"),
		Memory:            firstInt(m, "Memory", "MemorySize", "MemoryMB", "MemorySizeInMB"),
		OSName:            firstString(m, "OSName", "OSNameEn", "OSNameZh"),
		InternetBandwidth: firstInt(m, "InternetMaxBandwidthOut"),
	}
}

func firstIP(root map[string]any, keys ...string) string {
	for _, key := range keys {
		if ip := ipValue(root[key]); ip != "" {
			return ip
		}
	}
	return ""
}

func firstNestedIP(root map[string]any, parent string, keys ...string) string {
	nested, ok := root[parent].(map[string]any)
	if !ok {
		return ""
	}
	return firstIP(nested, keys...)
}

func ipValue(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		for _, item := range v {
			if ip := ipValue(item); ip != "" {
				return ip
			}
		}
	case map[string]any:
		for _, key := range []string{"IpAddress", "EipAddress", "PublicIpAddress", "Address"} {
			if ip := ipValue(v[key]); ip != "" {
				return ip
			}
		}
	}
	return ""
}
func intValue(v any) int { n, _ := strconv.Atoi(stringValue(v)); return n }
