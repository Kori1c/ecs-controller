<?php

use AlibabaCloud\Client\AlibabaCloud;
use AlibabaCloud\Client\Exception\ClientException;
use AlibabaCloud\Client\Exception\ServerException;

class AliyunService
{
    private $regionCache = [];
    private $managedTagKey = 'ecs-controller-managed';
    private $managedTagValue = 'true';

    /**
     * 智能重试执行器
     * 自动处理网络抖动、超时和服务端临时错误
     * * @param callable $func 业务逻辑闭包
     * @param string $action 操作名称
     * @param int $maxRetries 最大重试次数
     * @return mixed
     * @throws \Exception
     */
    private function executeWithRetry(callable $func, $action, $maxRetries = 3) // 优化点1: 将默认重试次数回调为 3 次，平衡前端等待体验
    {
        $attempt = 0;
        $lastException = null;

        while ($attempt < $maxRetries) {
            try {
                return $func();
            } catch (ClientException $e) {
                // 客户端错误(4xx)通常不重试，除非是流控限制(Throttling)
                $errorCode = $e->getErrorCode();
                if (stripos($errorCode, 'Throttling') !== false) {
                    $lastException = $e;
                    // 流控触发时，等待时间稍长
                    $this->backoff($attempt, true);
                    $attempt++;
                    continue;
                }
                throw $e; // 其他 4xx 错误直接抛出（如 AccessKey 错误）
            } catch (ServerException $e) {
                // 服务端错误(5xx)需要重试
                $lastException = $e;
            } catch (\Exception $e) {
                // 网络/cURL错误(超时、无法解析DNS等)需要重试
                $lastException = $e;
            }

            $attempt++;
            if ($attempt < $maxRetries) {
                // 记录简短日志到标准输出（可选，方便调试 Docker logs）
                // echo "Warning: Retrying $action (Attempt $attempt/$maxRetries)...\n";
                $this->backoff($attempt);
            }
        }

        throw $lastException;
    }

    /**
     * 指数退避策略
     * @param int $attempt 当前尝试次数
     * @param bool $isThrottling 是否因为流控
     */
    private function backoff($attempt, $isThrottling = false)
    {
        // 优化点2: 基础等待时间从 0.5s 提升至 1s
        // 序列变为: 1s, 2s, 4s... 3次重试总耗时控制在合理范围内
        $base = 1000000 * pow(2, $attempt);
        if ($isThrottling) {
            $base *= 2; // 流控时等待时间翻倍
        }
        // 增加随机抖动，避免多线程/多容器并发请求撞车
        $jitter = rand(0, 500000);
        usleep($base + $jitter);
    }

    private $trafficCache = [];

    private function setDefaultClient($key, $secret, $regionId)
    {
        AlibabaCloud::accessKeyClient($key, $secret)
            ->regionId($regionId)
            ->asDefaultClient();
    }

    private function ecsHost($regionId)
    {
        return "ecs.{$regionId}.aliyuncs.com";
    }

    private function vpcHost($regionId)
    {
        return "vpc.{$regionId}.aliyuncs.com";
    }

    /**
     * 判断是否为海外区域
     * 国内区域：cn-* (排除 cn-hongkong)
     * 海外区域：其他所有区域 + cn-hongkong
     */
    private function isOverseas($regionId)
    {
        // 简单判断：如果以 cn- 开头且不是 cn-hongkong，则是国内
        if (strpos($regionId, 'cn-') === 0 && $regionId !== 'cn-hongkong') {
            return false;
        }
        return true;
    }

    /**
     * 获取 BSS 费用中心 API 的 regionId 和 endpoint
     * 中国站: cn-hangzhou + business.aliyuncs.com
     * 国际站: ap-southeast-1 + business.ap-southeast-1.aliyuncs.com
     * @param string $siteType 'china' 或 'international'
     */
    private function getBssEndpoint($siteType = 'china')
    {
        if ($siteType === 'international') {
            return [
                'regionId' => 'ap-southeast-1',
                'host'     => 'business.ap-southeast-1.aliyuncs.com'
            ];
        }
        return [
            'regionId' => 'cn-hangzhou',
            'host'     => 'business.aliyuncs.com'
        ];
    }

    /**
     * 获取 CDT 流量
     * @param string $key AccessKey
     * @param string $secret Secret
     * @param string $targetRegion 目标实例的区域ID
     * @throws \Exception
     */
    public function getTraffic($key, $secret, $targetRegion)
    {
        // 1. 检查缓存
        $cacheKey = md5($key);
        if (isset($this->trafficCache[$cacheKey])) {
            $result = $this->trafficCache[$cacheKey];
        } else {
            // 2. 如果无缓存，发起 API 请求
            $result = $this->executeWithRetry(function () use ($key, $secret) {
                AlibabaCloud::accessKeyClient($key, $secret)
                    ->regionId('cn-hongkong') // CDT 接口通常用 cn-hongkong 或 cn-hangzhou 调用即可获取全局数据
                    ->asDefaultClient();

                return AlibabaCloud::rpc()
                    ->product('CDT')
                    ->scheme('https')
                    ->version('2021-08-13')
                    ->action('ListCdtInternetTraffic')
                    ->method('POST')
                    ->host('cdt.aliyuncs.com')
                    ->options([
                        'connect_timeout' => 10.0,
                        'timeout' => 20.0
                    ])
                    ->request();
            }, 'getTraffic');

            // 写入缓存
            $this->trafficCache[$cacheKey] = $result;
        }

        if (isset($result['TrafficDetails'])) {
            $isTargetOverseas = $this->isOverseas($targetRegion);
            $totalTraffic = 0;

            foreach ($result['TrafficDetails'] as $detail) {
                // 核心逻辑：区分国内/海外
                // 只有当流量产生区域的属性（国内/海外）与目标实例区域属性一致时，才计入
                $trafficRegion = $detail['BusinessRegionId'] ?? '';
                if ($this->isOverseas($trafficRegion) === $isTargetOverseas) {
                    $totalTraffic += $detail['Traffic'];
                }
            }

            return $totalTraffic / (1024 * 1024 * 1024);
        }

        throw new \Exception("API 响应缺少 TrafficDetails 字段");
    }

    /**
     * 获取 ECS 实例公网出口分钟带宽点，并换算为字节增量。
     * 阿里云 ECS 公网按出方向流量计费；这里优先使用 VPC 公网 IP 维度指标，经典网络回退到实例维度指标。
     *
     * @return array ['bytes' => float, 'lastSampleMs' => int, 'points' => int, 'metric' => string]
     * @throws \Exception
     */
    public function getInstanceOutboundTrafficDelta($account, $startMs, $endMs)
    {
        if (empty($account['instance_id'])) {
            throw new \Exception('未配置 Instance ID');
        }

        if ($endMs <= $startMs) {
            return [
                'bytes' => 0.0,
                'lastSampleMs' => (int) $startMs,
                'points' => 0,
                'metric' => ''
            ];
        }

        $metricCandidates = [];
        $publicIp = trim((string) ($account['public_ip'] ?? ''));
        if ($publicIp !== '') {
            $metricCandidates[] = [
                'name' => 'VPC_PublicIP_InternetOutRate',
                'dimensions' => [[
                    'instanceId' => $account['instance_id'],
                    'ip' => $publicIp
                ]]
            ];
        }

        $metricCandidates[] = [
            'name' => 'InternetOutRate',
            'dimensions' => [[
                'instanceId' => $account['instance_id']
            ]]
        ];

        $lastException = null;
        foreach ($metricCandidates as $candidate) {
            try {
                $result = $this->queryMetricRateAsBytes(
                    $account['access_key_id'],
                    $account['access_key_secret'],
                    $candidate['name'],
                    $candidate['dimensions'],
                    $startMs,
                    $endMs
                );

                if ($result['points'] > 0 || $candidate['name'] === 'InternetOutRate') {
                    $result['metric'] = $candidate['name'];
                    return $result;
                }
            } catch (\Exception $e) {
                $lastException = $e;
            }
        }

        if ($lastException) {
            throw $lastException;
        }

        return [
            'bytes' => 0.0,
            'lastSampleMs' => (int) $startMs,
            'points' => 0,
            'metric' => ''
        ];
    }

    private function queryMetricRateAsBytes($key, $secret, $metricName, array $dimensions, $startMs, $endMs)
    {
        $period = 60;
        $chunkMs = 24 * 3600 * 1000;
        $cursor = (int) $startMs;
        $totalBytes = 0.0;
        $lastSampleMs = (int) $startMs;
        $pointCount = 0;

        while ($cursor < $endMs) {
            $chunkEnd = min($cursor + $chunkMs, (int) $endMs);
            $nextToken = null;

            do {
                $query = [
                    'Namespace' => 'acs_ecs_dashboard',
                    'MetricName' => $metricName,
                    'Period' => (string) $period,
                    'StartTime' => (string) $cursor,
                    'EndTime' => (string) $chunkEnd,
                    'Dimensions' => json_encode($dimensions, JSON_UNESCAPED_SLASHES),
                    'Length' => '1440'
                ];

                if (!empty($nextToken)) {
                    $query['NextToken'] = $nextToken;
                }

                $result = $this->executeWithRetry(function () use ($key, $secret, $query) {
                    AlibabaCloud::accessKeyClient($key, $secret)
                        ->regionId('cn-hangzhou')
                        ->asDefaultClient();

                    return AlibabaCloud::rpc()
                        ->product('Cms')
                        ->scheme('https')
                        ->version('2019-01-01')
                        ->action('DescribeMetricList')
                        ->method('POST')
                        ->host('metrics.aliyuncs.com')
                        ->options([
                            'query' => $query,
                            'connect_timeout' => 10.0,
                            'timeout' => 25.0
                        ])
                        ->request();
                }, 'queryMetricRateAsBytes');

                $datapoints = $result['Datapoints'] ?? '[]';
                if (is_string($datapoints)) {
                    $datapoints = json_decode($datapoints, true);
                }
                if (!is_array($datapoints)) {
                    $datapoints = [];
                }

                usort($datapoints, function ($a, $b) {
                    return ((int) ($a['timestamp'] ?? 0)) <=> ((int) ($b['timestamp'] ?? 0));
                });

                foreach ($datapoints as $point) {
                    $timestamp = (int) ($point['timestamp'] ?? 0);
                    if ($timestamp <= $startMs || $timestamp > $endMs) {
                        continue;
                    }

                    $rateBitsPerSecond = (float) ($point['Average'] ?? $point['Maximum'] ?? $point['Minimum'] ?? 0);
                    if ($rateBitsPerSecond < 0) {
                        $rateBitsPerSecond = 0;
                    }

                    $totalBytes += ($rateBitsPerSecond * $period) / 8;
                    $lastSampleMs = max($lastSampleMs, $timestamp);
                    $pointCount++;
                }

                $nextToken = $result['NextToken'] ?? null;
            } while (!empty($nextToken));

            $cursor = $chunkEnd;
        }

        return [
            'bytes' => $totalBytes,
            'lastSampleMs' => $lastSampleMs,
            'points' => $pointCount,
            'metric' => $metricName
        ];
    }

    /**
     * 获取实例状态
     * @throws \Exception
     */
    public function getInstanceStatus($account)
    {
        return $this->executeWithRetry(function () use ($account) {
            AlibabaCloud::accessKeyClient($account['access_key_id'], $account['access_key_secret'])
                ->regionId($account['region_id'])
                ->asDefaultClient();

            $options = [
                'query' => ['RegionId' => $account['region_id']],
                // 优化点3: 同样缩短实例状态查询的超时
                'connect_timeout' => 10.0,
                'timeout' => 20.0
            ];

            if (!empty($account['instance_id'])) {
                // 修改：阿里云 RPC 风格接口对于列表类参数（如 InstanceId.N）需要明确的索引
                $options['query']['InstanceId.1'] = $account['instance_id'];
            }

            $result = AlibabaCloud::rpc()
                ->product('Ecs')
                ->scheme('https')
                ->version('2014-05-26')
                ->action('DescribeInstanceStatus')
                ->method('POST')
                ->host("ecs.{$account['region_id']}.aliyuncs.com")
                ->options($options)
                ->request();

            $statuses = $result['InstanceStatuses']['InstanceStatus'] ?? [];
            foreach ($statuses as $item) {
                if (($item['InstanceId'] ?? '') === $account['instance_id']) {
                    return $item['Status'];
                }
            }

            // 如果没找到匹配的 ID，且返回了列表（说明过滤参数没生效），且当前账号只有一个实例 ID，则抛出异常
            if (empty($statuses) || count($statuses) > 1) {
                throw new \Exception("API 响应未找到匹配的实例状态 (ID: {$account['instance_id']})");
            }

            return 'Unknown';
        }, 'getInstanceStatus');
    }

    /**
     * 获取实例详细健康状态 (用于识别操作系统启动中等状态)
     */
    public function getInstanceFullStatus($account)
    {
        return $this->executeWithRetry(function () use ($account) {
            AlibabaCloud::accessKeyClient($account['access_key_id'], $account['access_key_secret'])
                ->regionId($account['region_id'])
                ->asDefaultClient();

            $options = [
                'query' => [
                    'RegionId' => $account['region_id'],
                    'InstanceId.1' => $account['instance_id']
                ],
                'connect_timeout' => 10.0,
                'timeout' => 20.0
            ];

            $result = AlibabaCloud::rpc()
                ->product('Ecs')
                ->scheme('https')
                ->version('2014-05-26')
                ->action('DescribeInstancesFullStatus')
                ->method('POST')
                ->host("ecs.{$account['region_id']}.aliyuncs.com")
                ->options($options)
                ->request();

            $statusSet = $result['InstanceFullStatusSet']['InstanceFullStatus'][0] ?? null;
            if ($statusSet && ($statusSet['InstanceId'] ?? '') === $account['instance_id']) {
                return [
                    'status' => $statusSet['Status']['Name'] ?? 'Unknown',
                    'healthStatus' => $statusSet['HealthStatus']['Name'] ?? 'Unknown',
                ];
            }

            return null;
        }, 'getInstanceFullStatus');
    }

    /**
     * 释放（删除）实例
     * @throws \Exception
     */
    public function deleteInstance($account, $forceStop = false)
    {
        if (empty($account['instance_id'])) {
            throw new \Exception("未配置 Instance ID");
        }

        try {
            return $this->executeWithRetry(function () use ($account) {
                AlibabaCloud::accessKeyClient($account['access_key_id'], $account['access_key_secret'])
                    ->regionId($account['region_id'])
                    ->asDefaultClient();

                AlibabaCloud::rpc()
                    ->product('Ecs')
                    ->scheme('https')
                    ->version('2014-05-26')
                    ->action('DeleteInstance')
                    ->method('POST')
                    ->host("ecs.{$account['region_id']}.aliyuncs.com")
                    ->options([
                        'query' => [
                            'RegionId' => $account['region_id'],
                            'InstanceId' => $account['instance_id'],
                            'Force' => true,
                        ],
                        'connect_timeout' => 10.0,
                        'timeout' => 25.0
                    ])
                    ->request();

                return true;
            }, 'deleteInstance');
        } catch (ServerException $e) {
            $code = $e->getErrorCode();
            if (stripos($code, 'NotFound') !== false || stripos($code, 'InvalidInstanceId') !== false) {
                return true;
            }
            throw $e;
        }
    }
    /**
     * 控制实例开关机
     * @throws \Exception
     */
    public function controlInstance($account, $action, $shutdownMode = 'KeepCharging')
    {
        return $this->executeWithRetry(function () use ($account, $action, $shutdownMode) {
            AlibabaCloud::accessKeyClient($account['access_key_id'], $account['access_key_secret'])
                ->regionId($account['region_id'])
                ->asDefaultClient();

            if (empty($account['instance_id'])) {
                throw new \Exception("未配置 Instance ID");
            }

            $options = [
                'query' => [
                    'RegionId' => $account['region_id'],
                    'InstanceId' => $account['instance_id']
                ],
                // 优化点4: 控制操作保持一致，确保用户操作不卡死
                'connect_timeout' => 10.0,
                'timeout' => 20.0
            ];

            if ($action === 'stop') {
                $options['query']['StoppedMode'] = $shutdownMode;
            }

            AlibabaCloud::rpc()
                ->product('Ecs')
                ->scheme('https')
                ->version('2014-05-26')
                ->action($action === 'stop' ? 'StopInstance' : 'StartInstance')
                ->method('POST')
                ->host("ecs.{$account['region_id']}.aliyuncs.com")
                ->options($options)
                ->request();

            return true;
        }, 'controlInstance');
    }

    /**
     * 获取当前账号可访问的地域列表
     * @param string $key
     * @param string $secret
     * @return array
     * @throws \Exception
     */
    public function getRegions($key, $secret)
    {
        $cacheKey = md5($key);
        if (isset($this->regionCache[$cacheKey])) {
            return $this->regionCache[$cacheKey];
        }

        $result = $this->executeWithRetry(function () use ($key, $secret) {
            AlibabaCloud::accessKeyClient($key, $secret)
                ->regionId('cn-hangzhou')
                ->asDefaultClient();

            return AlibabaCloud::rpc()
                ->product('Ecs')
                ->scheme('https')
                ->version('2014-05-26')
                ->action('DescribeRegions')
                ->method('POST')
                ->host('ecs.cn-hangzhou.aliyuncs.com')
                ->options([
                    'connect_timeout' => 5.0,
                    'timeout' => 10.0
                ])
                ->request();
        }, 'getRegions');

        $regions = [];
        foreach (($result['Regions']['Region'] ?? []) as $region) {
            if (empty($region['RegionId'])) {
                continue;
            }

            $regions[] = [
                'regionId' => $region['RegionId'],
                'localName' => $region['LocalName'] ?? $region['RegionId']
            ];
        }

        $this->regionCache[$cacheKey] = $regions;
        return $regions;
    }

    /**
     * 列出当前账号下所有 ECS 实例
     * @param string $key
     * @param string $secret
     * @return array
     * @throws \Exception
     */
    public function getInstances($key, $secret, $targetRegionId = null)
    {
        $regions = $this->getRegions($key, $secret);
        if (!empty($targetRegionId)) {
            $matchedRegion = null;
            foreach ($regions as $region) {
                if (($region['regionId'] ?? '') === $targetRegionId) {
                    $matchedRegion = $region;
                    break;
                }
            }

            $regions = [[
                'regionId' => $targetRegionId,
                'localName' => $matchedRegion['localName'] ?? $targetRegionId
            ]];
        }

        $instances = [];

        foreach ($regions as $region) {
            $pageNumber = 1;

            try {
                do {
                    $result = $this->executeWithRetry(function () use ($key, $secret, $region, $pageNumber) {
                        AlibabaCloud::accessKeyClient($key, $secret)
                            ->regionId($region['regionId'])
                            ->asDefaultClient();

                        return AlibabaCloud::rpc()
                            ->product('Ecs')
                            ->scheme('https')
                            ->version('2014-05-26')
                            ->action('DescribeInstances')
                            ->method('POST')
                            ->host("ecs.{$region['regionId']}.aliyuncs.com")
                            ->options([
                                'query' => [
                                    'RegionId' => $region['regionId'],
                                    'PageSize' => 100,
                                    'PageNumber' => $pageNumber
                                ],
                                'connect_timeout' => 15.0,
                                'timeout' => 30.0
                            ])
                            ->request();
                    }, 'getInstances');

                    $items = $result['Instances']['Instance'] ?? [];
                    foreach ($items as $instance) {
                        $instances[] = [
                            'instanceId' => $instance['InstanceId'] ?? '',
                            'instanceName' => $instance['InstanceName'] ?? '',
                            'status' => $instance['Status'] ?? 'Unknown',
                            'regionId' => $region['regionId'],
                            'regionName' => $region['localName'],
                            'instanceType' => $instance['InstanceType'] ?? '',
                            'cpu' => $instance['Cpu'] ?? 0,
                            'memory' => $instance['Memory'] ?? 0,
                            'internetMaxBandwidthOut' => (int) (($instance['EipAddress']['Bandwidth'] ?? 0) ?: ($instance['InternetMaxBandwidthOut'] ?? 0)),
                            'osName' => $instance['OSName'] ?? '',
                            'publicIp' => $instance['PublicIpAddress']['IpAddress'][0] ?? $instance['EipAddress']['IpAddress'] ?? '',
                            'eipAllocationId' => $instance['EipAddress']['AllocationId'] ?? '',
                            'eipAddress' => $instance['EipAddress']['IpAddress'] ?? '',
                            'privateIp' => $instance['VpcAttributes']['PrivateIpAddress']['IpAddress'][0] ?? '',
                            'stoppedMode' => $instance['StoppedMode'] ?? '',
                            'chargeType' => $instance['InstanceChargeType'] ?? ''
                        ];
                    }

                    $totalCount = (int) ($result['TotalCount'] ?? count($items));
                    $pageSize = (int) ($result['PageSize'] ?? 100);
                    $pageNumber++;
                } while ($totalCount > 0 && (($pageNumber - 1) * $pageSize) < $totalCount);
            } catch (\Exception $e) {
                if (!empty($targetRegionId)) {
                    throw $e;
                }
                continue;
            }
        }

        usort($instances, function ($a, $b) {
            $regionCompare = strcmp($a['regionId'], $b['regionId']);
            if ($regionCompare !== 0) {
                return $regionCompare;
            }

            return strcmp($a['instanceId'], $b['instanceId']);
        });

        return $instances;
    }

    public function buildEcsCreatePreview($account, array $request, $clientIp = '')
    {
        $regionId = trim((string) ($request['regionId'] ?? $account['region_id'] ?? ''));
        $instanceType = trim((string) ($request['instanceType'] ?? '')) ?: 'ecs.e-c4m1.large';
        $osKey = trim((string) ($request['osKey'] ?? 'debian_12'));
        $publicIpMode = trim((string) ($request['publicIpMode'] ?? 'ecs_public_ip'));
        if (!in_array($publicIpMode, ['ecs_public_ip', 'eip'], true)) {
            $publicIpMode = 'ecs_public_ip';
        }
        $instanceName = trim((string) ($request['instanceName'] ?? ''));
        if ($instanceName === '') {
            $instanceName = 'launch-' . date('Ymd-His');
        }
        $requestedDiskSize = (int) ($request['systemDiskSize'] ?? 20);
        $requestedDiskCategory = trim((string) ($request['systemDiskCategory'] ?? 'cloud_essd_entry'));

        if ($regionId === '') {
            throw new \Exception('请选择区域');
        }

        $key = $account['access_key_id'];
        $secret = $account['access_key_secret'];

        $zone = $this->selectAvailableZone($key, $secret, $regionId, $instanceType);
        $instanceTypeInfo = $this->describeInstanceType($key, $secret, $regionId, $instanceType);
        $image = $this->selectSystemImage($key, $secret, $regionId, $osKey, $instanceTypeInfo['CpuArchitecture'] ?? '');
        $diskCategory = $this->selectDiskCategory($zone, $requestedDiskCategory);
        $diskRange = $this->getSystemDiskSizeRange($key, $secret, $regionId, $zone['zoneId'], $instanceType, $diskCategory);
        $diskSize = $this->normalizeSystemDiskSize($requestedDiskSize, $diskRange);
        $bandwidth = $this->estimateMaxBandwidthOut($instanceType, $regionId);
        $loginPort = ($image['osType'] ?? 'linux') === 'windows' ? 3389 : 22;
        $loginUser = ($image['osType'] ?? 'linux') === 'windows' ? 'Administrator' : 'root';
        $securityRule = '默认全开：允许 0.0.0.0/0 入方向 TCP/UDP/ICMP';

        return [
            'account' => [
                'groupKey' => $account['group_key'] ?? '',
                'label' => trim((string) ($account['remark'] ?? '')) ?: substr($key, 0, 7) . '***'
            ],
            'regionId' => $regionId,
            'zoneId' => $zone['zoneId'],
            'instanceType' => $instanceType,
            'instanceName' => $instanceName,
            'osKey' => $osKey,
            'osLabel' => $image['label'],
            'imageId' => $image['imageId'],
            'imageSize' => (int) ($image['size'] ?? 0),
            'loginUser' => $loginUser,
            'loginPort' => $loginPort,
            'clientCidrIp' => '0.0.0.0/0',
            'chargeType' => 'PostPaid',
            'internetChargeType' => 'PayByTraffic',
            'internetMaxBandwidthOut' => $bandwidth,
            'publicIpMode' => $publicIpMode,
            'publicIpModeLabel' => $publicIpMode === 'eip' ? 'EIP 弹性公网 IP' : 'ECS 普通公网 IP',
            'systemDisk' => [
                'category' => $diskCategory,
                'size' => $diskSize,
                'min' => $diskRange['min'],
                'max' => $diskRange['max'],
                'unit' => $diskRange['unit']
            ],
            'network' => [
                'vpc' => [
                    'mode' => 'auto',
                    'name' => "ecs-controller-vpc-{$regionId}",
                    'cidr' => '172.31.0.0/16'
                ],
                'vswitch' => [
                    'mode' => 'auto',
                    'name' => "ecs-controller-vsw-{$zone['zoneId']}",
                    'cidr' => $this->cidrForZone($zone['zoneId'])
                ],
                'securityGroup' => [
                    'mode' => 'auto',
                    'name' => "ecs-controller-sg-{$regionId}",
                    'rules' => [$securityRule]
                ]
            ],
            'cdtCompatible' => true,
            'backupEnabled' => false,
            'pricing' => [
                'available' => false,
                'currency' => ($account['site_type'] ?? 'international') === 'international' ? 'USD' : 'CNY',
                'message' => '费用预估暂不可用。实例按量计费，公网按实际出口流量计费，最终以阿里云账单为准。',
                'trafficNote' => '公网按使用流量计费，并按 CDT 兼容方式创建。'
            ],
            'warnings' => array_values(array_filter([
                $publicIpMode === 'eip'
                    ? 'EIP 模式会先创建无普通公网 IP 的 ECS，再申请并绑定 EIP；停机不会释放 EIP，释放实例时会自动释放系统创建的 EIP。'
                    : 'ECS 普通公网 IP 由实例直接分配，停机后再启动可能变化；如需可控更换 IP，建议选择 EIP 模式。',
                '公网带宽峰值会自动尝试最高可用值，若账号配额或规格限制不支持，会自动降级重试。',
                "系统盘将严格按 {$diskSize} GB 创建；当前 API 返回范围为 {$diskRange['min']}-{$diskRange['max']} {$diskRange['unit']}，超出范围会直接报错。",
                '文件备份默认不启用；如需备份，请创建后在阿里云控制台单独开启。',
                '安全组默认全开，便于测试和交付；生产环境建议创建后收紧来源 IP 和端口。'
            ]))
        ];
    }

    public function getAvailableSystemDiskOptions($account, array $request)
    {
        $regionId = trim((string) ($request['regionId'] ?? $account['region_id'] ?? ''));
        $instanceType = trim((string) ($request['instanceType'] ?? '')) ?: 'ecs.e-c4m1.large';

        if ($regionId === '') {
            throw new \Exception('请选择区域');
        }

        $key = $account['access_key_id'];
        $secret = $account['access_key_secret'];
        $zone = $this->selectAvailableZone($key, $secret, $regionId, $instanceType);
        $rawCategories = $zone['raw']['AvailableDiskCategories']['DiskCategories'] ?? $zone['raw']['AvailableDiskCategories']['DiskCategory'] ?? [];
        $rawCategories = is_array($rawCategories) ? $rawCategories : [];
        $candidates = $rawCategories ?: ['cloud_essd_entry', 'cloud_essd', 'cloud_efficiency', 'cloud'];
        $candidates = array_values(array_filter($candidates, function ($category) {
            return $category !== 'cloud_auto';
        }));
        $preferredOrder = ['cloud_essd_entry', 'cloud_essd', 'cloud_efficiency', 'cloud'];
        $candidates = array_values(array_unique(array_filter($candidates)));
        usort($candidates, function ($a, $b) use ($preferredOrder) {
            $aIndex = array_search($a, $preferredOrder, true);
            $bIndex = array_search($b, $preferredOrder, true);
            $aIndex = $aIndex === false ? 99 : $aIndex;
            $bIndex = $bIndex === false ? 99 : $bIndex;
            return $aIndex <=> $bIndex ?: strcmp($a, $b);
        });

        $options = [];
        $errors = [];
        foreach ($candidates as $category) {
            try {
                $range = $this->getSystemDiskSizeRange($key, $secret, $regionId, $zone['zoneId'], $instanceType, $category);
                $options[] = [
                    'value' => $category,
                    'label' => $this->diskCategoryLabel($category),
                    'min' => $range['min'],
                    'max' => $range['max'],
                    'unit' => $range['unit'],
                    'zoneId' => $zone['zoneId'],
                    'status' => $range['status'] ?? '',
                    'statusCategory' => $range['statusCategory'] ?? ''
                ];
            } catch (\Exception $e) {
                $errors[$category] = $e->getMessage();
            }
        }

        if (empty($options)) {
            throw new \Exception('当前账号区域和实例规格没有可用的系统盘类型，请更换实例规格后重试');
        }

        return [
            'regionId' => $regionId,
            'zoneId' => $zone['zoneId'],
            'instanceType' => $instanceType,
            'options' => $options,
            'errors' => $errors
        ];
    }

    public function createManagedEcsFromPreview($account, array $preview, callable $progress = null)
    {
        $key = $account['access_key_id'];
        $secret = $account['access_key_secret'];
        $regionId = $preview['regionId'];
        $zoneId = $preview['zoneId'];
        $instanceType = $preview['instanceType'];
        $password = $this->generateInstancePassword();

        $this->emitProgress($progress, '准备 VPC');
        $vpc = $this->ensureVpc($key, $secret, $regionId, $preview['network']['vpc']['name'], $preview['network']['vpc']['cidr']);

        $this->emitProgress($progress, '准备交换机');
        $vswitch = $this->ensureVSwitch(
            $key,
            $secret,
            $regionId,
            $zoneId,
            $vpc['VpcId'],
            $preview['network']['vswitch']['name'],
            $preview['network']['vswitch']['cidr']
        );

        $this->emitProgress($progress, '准备安全组');
        $securityGroup = $this->ensureSecurityGroup($key, $secret, $regionId, $vpc['VpcId'], $preview['network']['securityGroup']['name']);
        $this->authorizeOpenSecurityGroupRules($key, $secret, $regionId, $securityGroup['SecurityGroupId']);

        $bandwidthCandidates = $this->bandwidthCandidates((int) ($preview['internetMaxBandwidthOut'] ?? 100));
        $diskCategories = array_unique(array_filter([$preview['systemDisk']['category'] ?? 'cloud_essd_entry']));
        $publicIpMode = ($preview['publicIpMode'] ?? 'ecs_public_ip') === 'eip' ? 'eip' : 'ecs_public_ip';
        // 系统盘成本敏感，严格使用用户确认的值；若阿里云拒绝，不自动放大。
        $diskSize = $this->normalizeSystemDiskSize($preview['systemDisk']['size'] ?? 20, $preview['systemDisk'] ?? []);
        $lastError = null;

        foreach ($bandwidthCandidates as $bandwidth) {
            foreach ($diskCategories as $diskCategory) {
                $allocatedEip = null;
                try {
                    $this->emitProgress($progress, "创建 ECS（{$bandwidth} Mbps / {$diskCategory}）");
                    $instanceIds = $this->runInstance(
                        $key,
                        $secret,
                        $regionId,
                        [
                            'zoneId' => $zoneId,
                            'instanceType' => $instanceType,
                            'imageId' => $preview['imageId'],
                            'securityGroupId' => $securityGroup['SecurityGroupId'],
                            'vSwitchId' => $vswitch['VSwitchId'],
                            'instanceName' => $preview['instanceName'],
                            'password' => $password,
                            'internetMaxBandwidthOut' => $publicIpMode === 'eip' ? 0 : $bandwidth,
                            'systemDiskCategory' => $diskCategory,
                            'systemDiskSize' => $diskSize
                        ]
                    );

                    $instanceId = $instanceIds[0] ?? '';
                    if ($instanceId === '') {
                        throw new \Exception('RunInstances 未返回 InstanceId');
                    }

                    $this->emitProgress($progress, '等待实例启动');
                    $instance = $this->waitInstanceReady($key, $secret, $regionId, $instanceId);

                    if ($publicIpMode === 'eip') {
                        $this->emitProgress($progress, "申请 EIP（{$bandwidth} Mbps）");
                        $allocatedEip = $this->allocateEipAddress($key, $secret, $regionId, $bandwidth, $preview['instanceName']);
                        $this->emitProgress($progress, '绑定 EIP');
                        $this->associateEipAddress($key, $secret, $regionId, $allocatedEip['allocationId'], $instanceId);
                        $this->waitEipStatus($key, $secret, $regionId, $allocatedEip['allocationId'], 'InUse');
                        $instance = $this->waitInstanceReady($key, $secret, $regionId, $instanceId);
                        $instance['publicIp'] = $allocatedEip['ipAddress'] ?: ($instance['publicIp'] ?? '');
                        $instance['eipAllocationId'] = $allocatedEip['allocationId'];
                        $instance['eipAddress'] = $allocatedEip['ipAddress'];
                    }

                    return [
                        'instanceId' => $instanceId,
                        'publicIp' => $instance['publicIp'] ?? '',
                        'privateIp' => $instance['privateIp'] ?? '',
                        'publicIpMode' => $publicIpMode,
                        'eipAllocationId' => $instance['eipAllocationId'] ?? '',
                        'eipAddress' => $instance['eipAddress'] ?? '',
                        'eipManaged' => $publicIpMode === 'eip',
                        'status' => $instance['status'] ?? 'Unknown',
                        'instanceName' => $preview['instanceName'],
                        'instanceType' => $instanceType,
                        'vpcId' => $vpc['VpcId'],
                        'vswitchId' => $vswitch['VSwitchId'],
                        'securityGroupId' => $securityGroup['SecurityGroupId'],
                        'internetMaxBandwidthOut' => $bandwidth,
                        'systemDiskCategory' => $diskCategory,
                        'systemDiskSize' => $diskSize,
                        'loginUser' => $preview['loginUser'] ?? 'root',
                        'loginPassword' => $password
                    ];
                } catch (\Exception $e) {
                    if ($allocatedEip && !empty($allocatedEip['allocationId'])) {
                        $this->releaseEipAddressSilently($key, $secret, $regionId, $allocatedEip['allocationId']);
                    }
                    $lastError = $e;
                    $message = $e->getMessage();
                    if ($this->isDiskSizeError($message)) {
                        throw new \Exception("系统盘 {$diskSize} GB 不被当前镜像或实例规格支持，请手动调整系统盘大小后重新创建。阿里云返回：" . $message);
                    }
                    if (stripos($message, 'InvalidInstanceType.NotSupported') !== false || stripos($message, 'image architecture') !== false) {
                        $instanceTypeInfo = $this->describeInstanceType($key, $secret, $regionId, $instanceType);
                        $image = $this->selectSystemImage($key, $secret, $regionId, $preview['osKey'] ?? 'ubuntu_22', $instanceTypeInfo['CpuArchitecture'] ?? '');
                        $preview['imageId'] = $image['imageId'];
                        $preview['osLabel'] = $image['label'];
                    }
                    continue;
                }
            }
        }

        throw new \Exception($lastError ? $lastError->getMessage() : 'ECS 创建失败');
    }

    private function selectAvailableZone($key, $secret, $regionId, $instanceType)
    {
        $result = $this->executeWithRetry(function () use ($key, $secret, $regionId, $instanceType) {
            $this->setDefaultClient($key, $secret, $regionId);

            return AlibabaCloud::rpc()
                ->product('Ecs')
                ->scheme('https')
                ->version('2014-05-26')
                ->action('DescribeZones')
                ->method('POST')
                ->host($this->ecsHost($regionId))
                ->options([
                    'query' => [
                        'RegionId' => $regionId,
                        'InstanceType' => $instanceType,
                        'AvailableResourceCreation.1' => 'Instance'
                    ],
                    'connect_timeout' => 5.0,
                    'timeout' => 15.0
                ])
                ->request();
        }, 'selectAvailableZone');

        $zones = $result['Zones']['Zone'] ?? [];
        foreach ($zones as $zone) {
            if (!empty($zone['ZoneId'])) {
                return [
                    'zoneId' => $zone['ZoneId'],
                    'raw' => $zone
                ];
            }
        }

        throw new \Exception("当前区域 {$regionId} 下未找到规格 {$instanceType} 的可用区库存");
    }

    private function describeInstanceType($key, $secret, $regionId, $instanceType)
    {
        $result = $this->executeWithRetry(function () use ($key, $secret, $regionId, $instanceType) {
            $this->setDefaultClient($key, $secret, $regionId);

            return AlibabaCloud::rpc()
                ->product('Ecs')
                ->scheme('https')
                ->version('2014-05-26')
                ->action('DescribeInstanceTypes')
                ->method('POST')
                ->host($this->ecsHost($regionId))
                ->options([
                    'query' => [
                        'RegionId' => $regionId,
                        'InstanceTypes' => json_encode([$instanceType])
                    ],
                    'connect_timeout' => 5.0,
                    'timeout' => 15.0
                ])
                ->request();
        }, 'describeInstanceType');

        $types = $result['InstanceTypes']['InstanceType'] ?? [];
        foreach ($types as $type) {
            if (($type['InstanceTypeId'] ?? '') === $instanceType) {
                return $type;
            }
        }

        return $types[0] ?? [];
    }

    private function selectSystemImage($key, $secret, $regionId, $osKey, $cpuArchitecture = '')
    {
        $profiles = [
            'alibaba_cloud_linux_3' => ['label' => 'Alibaba Cloud Linux 3', 'osType' => 'linux', 'platform' => 'Aliyun', 'patterns' => ['aliyun_3', 'alibaba cloud linux 3']],
            'ubuntu_22' => ['label' => 'Ubuntu 22.04', 'osType' => 'linux', 'platform' => 'Ubuntu', 'patterns' => ['ubuntu_22', 'ubuntu 22', '22_04']],
            'ubuntu_24' => ['label' => 'Ubuntu 24.04', 'osType' => 'linux', 'platform' => 'Ubuntu', 'patterns' => ['ubuntu_24', 'ubuntu 24', '24_04']],
            'debian_12' => ['label' => 'Debian 12', 'osType' => 'linux', 'platform' => 'Debian', 'patterns' => ['debian_12', 'debian 12']],
            'centos_stream_9' => ['label' => 'CentOS Stream 9', 'osType' => 'linux', 'platform' => 'CentOS', 'patterns' => ['centos_stream_9', 'centos stream 9']],
            'windows_2022' => ['label' => 'Windows Server 2022', 'osType' => 'windows', 'platform' => 'Windows Server', 'patterns' => ['win2022', 'windows server 2022']]
        ];
        $profile = $profiles[$osKey] ?? $profiles['debian_12'];
        $architecture = $this->normalizeImageArchitecture($cpuArchitecture);

        $result = $this->executeWithRetry(function () use ($key, $secret, $regionId, $profile, $architecture) {
            $this->setDefaultClient($key, $secret, $regionId);

            $query = [
                'RegionId' => $regionId,
                'ImageOwnerAlias' => 'system',
                'OSType' => $profile['osType'],
                'Status' => 'Available',
                'PageSize' => 100
            ];

            if (!empty($profile['platform'])) {
                $query['Platform'] = $profile['platform'];
            }
            if ($architecture !== '') {
                $query['Architecture'] = $architecture;
            }

            return AlibabaCloud::rpc()
                ->product('Ecs')
                ->scheme('https')
                ->version('2014-05-26')
                ->action('DescribeImages')
                ->method('POST')
                ->host($this->ecsHost($regionId))
                ->options([
                    'query' => $query,
                    'connect_timeout' => 5.0,
                    'timeout' => 15.0
                ])
                ->request();
        }, 'selectSystemImage');

        $images = $result['Images']['Image'] ?? [];
        usort($images, function ($a, $b) {
            return strcmp((string) ($b['CreationTime'] ?? ''), (string) ($a['CreationTime'] ?? ''));
        });

        foreach ($images as $image) {
            $haystack = strtolower(($image['ImageId'] ?? '') . ' ' . ($image['ImageName'] ?? '') . ' ' . ($image['Description'] ?? ''));
            foreach ($profile['patterns'] as $pattern) {
                if (strpos($haystack, strtolower($pattern)) !== false) {
                    return [
                        'imageId' => $image['ImageId'],
                        'label' => $profile['label'],
                        'osType' => $profile['osType'],
                        'size' => (int) ($image['Size'] ?? 0)
                    ];
                }
            }
        }

        throw new \Exception("当前区域未找到可用系统镜像：{$profile['label']}");
    }

    private function normalizeImageArchitecture($cpuArchitecture)
    {
        $value = strtolower((string) $cpuArchitecture);
        if (strpos($value, 'arm') !== false || strpos($value, 'aarch64') !== false) {
            return 'arm64';
        }
        if (strpos($value, 'x86') !== false || strpos($value, 'amd64') !== false || strpos($value, 'i386') !== false) {
            return 'x86_64';
        }
        return '';
    }

    private function ensureVpc($key, $secret, $regionId, $name, $cidr)
    {
        $existing = $this->describeManagedVpcs($key, $secret, $regionId);
        if (!empty($existing)) {
            return $existing[0];
        }

        $result = $this->executeWithRetry(function () use ($key, $secret, $regionId, $name, $cidr) {
            $this->setDefaultClient($key, $secret, $regionId);

            return AlibabaCloud::rpc()
                ->product('Vpc')
                ->scheme('https')
                ->version('2016-04-28')
                ->action('CreateVpc')
                ->method('POST')
                ->host($this->vpcHost($regionId))
                ->options([
                    'query' => [
                        'RegionId' => $regionId,
                        'VpcName' => $name,
                        'CidrBlock' => $cidr,
                        'Tag.1.Key' => $this->managedTagKey,
                        'Tag.1.Value' => $this->managedTagValue
                    ],
                    'connect_timeout' => 5.0,
                    'timeout' => 15.0
                ])
                ->request();
        }, 'ensureVpc');

        return ['VpcId' => $result['VpcId'] ?? '', 'VpcName' => $name, 'CidrBlock' => $cidr];
    }

    private function ensureVSwitch($key, $secret, $regionId, $zoneId, $vpcId, $name, $cidr)
    {
        $existing = $this->describeManagedVSwitches($key, $secret, $regionId, $vpcId, $zoneId);
        if (!empty($existing)) {
            return $existing[0];
        }

        $result = $this->executeWithRetry(function () use ($key, $secret, $regionId, $zoneId, $vpcId, $name, $cidr) {
            $this->setDefaultClient($key, $secret, $regionId);

            return AlibabaCloud::rpc()
                ->product('Vpc')
                ->scheme('https')
                ->version('2016-04-28')
                ->action('CreateVSwitch')
                ->method('POST')
                ->host($this->vpcHost($regionId))
                ->options([
                    'query' => [
                        'RegionId' => $regionId,
                        'ZoneId' => $zoneId,
                        'VpcId' => $vpcId,
                        'VSwitchName' => $name,
                        'CidrBlock' => $cidr,
                        'Tag.1.Key' => $this->managedTagKey,
                        'Tag.1.Value' => $this->managedTagValue
                    ],
                    'connect_timeout' => 5.0,
                    'timeout' => 20.0
                ])
                ->request();
        }, 'ensureVSwitch');

        return ['VSwitchId' => $result['VSwitchId'] ?? '', 'VSwitchName' => $name, 'ZoneId' => $zoneId, 'CidrBlock' => $cidr];
    }

    private function ensureSecurityGroup($key, $secret, $regionId, $vpcId, $name)
    {
        $existing = $this->describeManagedSecurityGroups($key, $secret, $regionId, $vpcId);
        if (!empty($existing)) {
            return $existing[0];
        }

        $result = $this->executeWithRetry(function () use ($key, $secret, $regionId, $vpcId, $name) {
            $this->setDefaultClient($key, $secret, $regionId);

            return AlibabaCloud::rpc()
                ->product('Ecs')
                ->scheme('https')
                ->version('2014-05-26')
                ->action('CreateSecurityGroup')
                ->method('POST')
                ->host($this->ecsHost($regionId))
                ->options([
                    'query' => [
                        'RegionId' => $regionId,
                        'VpcId' => $vpcId,
                        'SecurityGroupName' => $name,
                        'Description' => 'Managed by CDT Monitor',
                        'SecurityGroupType' => 'normal',
                        'Tag.1.Key' => $this->managedTagKey,
                        'Tag.1.Value' => $this->managedTagValue
                    ],
                    'connect_timeout' => 5.0,
                    'timeout' => 15.0
                ])
                ->request();
        }, 'ensureSecurityGroup');

        return ['SecurityGroupId' => $result['SecurityGroupId'] ?? '', 'SecurityGroupName' => $name];
    }

    private function authorizeSecurityGroupRule($key, $secret, $regionId, $securityGroupId, $port, $sourceCidrIp)
    {
        try {
            $this->executeWithRetry(function () use ($key, $secret, $regionId, $securityGroupId, $port, $sourceCidrIp) {
                $this->setDefaultClient($key, $secret, $regionId);

                return AlibabaCloud::rpc()
                    ->product('Ecs')
                    ->scheme('https')
                    ->version('2014-05-26')
                    ->action('AuthorizeSecurityGroup')
                    ->method('POST')
                    ->host($this->ecsHost($regionId))
                    ->options([
                        'query' => [
                            'RegionId' => $regionId,
                            'SecurityGroupId' => $securityGroupId,
                            'IpProtocol' => 'tcp',
                            'PortRange' => "{$port}/{$port}",
                            'SourceCidrIp' => $sourceCidrIp,
                            'Policy' => 'accept',
                            'Priority' => '1',
                            'Description' => 'CDT Monitor managed remote access'
                        ],
                        'connect_timeout' => 5.0,
                        'timeout' => 15.0
                    ])
                    ->request();
            }, 'authorizeSecurityGroupRule');
        } catch (\Exception $e) {
            if (stripos($e->getMessage(), 'InvalidPermission.Duplicate') === false) {
                throw $e;
            }
        }
    }

    private function authorizeOpenSecurityGroupRules($key, $secret, $regionId, $securityGroupId)
    {
        $rules = [
            ['protocol' => 'tcp', 'port' => '1/65535'],
            ['protocol' => 'udp', 'port' => '1/65535'],
            ['protocol' => 'icmp', 'port' => '-1/-1']
        ];

        foreach ($rules as $rule) {
            try {
                $this->executeWithRetry(function () use ($key, $secret, $regionId, $securityGroupId, $rule) {
                    $this->setDefaultClient($key, $secret, $regionId);

                    return AlibabaCloud::rpc()
                        ->product('Ecs')
                        ->scheme('https')
                        ->version('2014-05-26')
                        ->action('AuthorizeSecurityGroup')
                        ->method('POST')
                        ->host($this->ecsHost($regionId))
                        ->options([
                            'query' => [
                                'RegionId' => $regionId,
                                'SecurityGroupId' => $securityGroupId,
                                'IpProtocol' => $rule['protocol'],
                                'PortRange' => $rule['port'],
                                'SourceCidrIp' => '0.0.0.0/0',
                                'Policy' => 'accept',
                                'Priority' => '1',
                                'Description' => 'CDT Monitor open access'
                            ],
                            'connect_timeout' => 5.0,
                            'timeout' => 15.0
                        ])
                        ->request();
                }, 'authorizeOpenSecurityGroupRules');
            } catch (\Exception $e) {
                if (stripos($e->getMessage(), 'InvalidPermission.Duplicate') === false) {
                    throw $e;
                }
            }
        }
    }

    private function runInstance($key, $secret, $regionId, array $params)
    {
        $result = $this->executeWithRetry(function () use ($key, $secret, $regionId, $params) {
            $this->setDefaultClient($key, $secret, $regionId);

            return AlibabaCloud::rpc()
                ->product('Ecs')
                ->scheme('https')
                ->version('2014-05-26')
                ->action('RunInstances')
                ->method('POST')
                ->host($this->ecsHost($regionId))
                ->options([
                    'query' => [
                        'RegionId' => $regionId,
                        'ZoneId' => $params['zoneId'],
                        'InstanceType' => $params['instanceType'],
                        'ImageId' => $params['imageId'],
                        'SecurityGroupId' => $params['securityGroupId'],
                        'VSwitchId' => $params['vSwitchId'],
                        'InstanceName' => $params['instanceName'],
                        'HostName' => preg_replace('/[^a-zA-Z0-9-]/', '-', strtolower($params['instanceName'])),
                        'Password' => $params['password'],
                        'InstanceChargeType' => 'PostPaid',
                        'InternetChargeType' => 'PayByTraffic',
                        'InternetMaxBandwidthOut' => (int) $params['internetMaxBandwidthOut'],
                        'SystemDisk.Category' => $params['systemDiskCategory'],
                        'SystemDisk.Size' => (int) $params['systemDiskSize'],
                        'DeletionProtection' => 'false',
                        'IoOptimized' => 'optimized',
                        'Amount' => 1,
                        'Tag.1.Key' => $this->managedTagKey,
                        'Tag.1.Value' => $this->managedTagValue
                    ],
                    'connect_timeout' => 5.0,
                    'timeout' => 25.0
                ])
                ->request();
        }, 'runInstance', 1);

        return $result['InstanceIdSets']['InstanceIdSet'] ?? [];
    }

    private function allocateEipAddress($key, $secret, $regionId, $bandwidth, $instanceName)
    {
        $result = $this->executeWithRetry(function () use ($key, $secret, $regionId, $bandwidth, $instanceName) {
            $this->setDefaultClient($key, $secret, $regionId);

            return AlibabaCloud::rpc()
                ->product('Vpc')
                ->scheme('https')
                ->version('2016-04-28')
                ->action('AllocateEipAddress')
                ->method('POST')
                ->host($this->vpcHost($regionId))
                ->options([
                    'query' => [
                        'RegionId' => $regionId,
                        'Bandwidth' => max(1, (int) $bandwidth),
                        'InternetChargeType' => 'PayByTraffic',
                        'Name' => $instanceName . '-eip',
                        'Tag.1.Key' => $this->managedTagKey,
                        'Tag.1.Value' => $this->managedTagValue
                    ],
                    'connect_timeout' => 5.0,
                    'timeout' => 20.0
                ])
                ->request();
        }, 'allocateEipAddress');

        $allocationId = $result['AllocationId'] ?? '';
        if ($allocationId === '') {
            throw new \Exception('EIP 申请成功但未返回 AllocationId');
        }

        $ipAddress = $result['EipAddress'] ?? '';
        if ($ipAddress === '') {
            $detail = $this->waitEipStatus($key, $secret, $regionId, $allocationId, 'Available', 6);
            $ipAddress = $detail['IpAddress'] ?? '';
        }

        return [
            'allocationId' => $allocationId,
            'ipAddress' => $ipAddress
        ];
    }

    private function associateEipAddress($key, $secret, $regionId, $allocationId, $instanceId)
    {
        if ($allocationId === '' || $instanceId === '') {
            throw new \Exception('EIP 绑定参数缺失');
        }

        return $this->executeWithRetry(function () use ($key, $secret, $regionId, $allocationId, $instanceId) {
            $this->setDefaultClient($key, $secret, $regionId);

            return AlibabaCloud::rpc()
                ->product('Vpc')
                ->scheme('https')
                ->version('2016-04-28')
                ->action('AssociateEipAddress')
                ->method('POST')
                ->host($this->vpcHost($regionId))
                ->options([
                    'query' => [
                        'RegionId' => $regionId,
                        'AllocationId' => $allocationId,
                        'InstanceId' => $instanceId,
                        'InstanceType' => 'EcsInstance'
                    ],
                    'connect_timeout' => 5.0,
                    'timeout' => 20.0
                ])
                ->request();
        }, 'associateEipAddress');
    }

    private function unassociateEipAddress($key, $secret, $regionId, $allocationId, $instanceId)
    {
        if ($allocationId === '') {
            return true;
        }

        try {
            $this->executeWithRetry(function () use ($key, $secret, $regionId, $allocationId, $instanceId) {
                $this->setDefaultClient($key, $secret, $regionId);

                $query = [
                    'RegionId' => $regionId,
                    'AllocationId' => $allocationId,
                    'InstanceType' => 'EcsInstance'
                ];
                if ($instanceId !== '') {
                    $query['InstanceId'] = $instanceId;
                }

                return AlibabaCloud::rpc()
                    ->product('Vpc')
                    ->scheme('https')
                    ->version('2016-04-28')
                    ->action('UnassociateEipAddress')
                    ->method('POST')
                    ->host($this->vpcHost($regionId))
                    ->options([
                        'query' => $query,
                        'connect_timeout' => 5.0,
                        'timeout' => 20.0
                    ])
                    ->request();
            }, 'unassociateEipAddress');
        } catch (\Exception $e) {
            $message = $e->getMessage();
            if (
                stripos($message, 'IncorrectEipStatus') === false
                && stripos($message, 'InvalidAllocationId.NotFound') === false
                && stripos($message, 'not exist') === false
            ) {
                throw $e;
            }
        }

        return true;
    }

    private function releaseEipAddress($key, $secret, $regionId, $allocationId)
    {
        if ($allocationId === '') {
            return true;
        }

        try {
            $this->executeWithRetry(function () use ($key, $secret, $regionId, $allocationId) {
                $this->setDefaultClient($key, $secret, $regionId);

                return AlibabaCloud::rpc()
                    ->product('Vpc')
                    ->scheme('https')
                    ->version('2016-04-28')
                    ->action('ReleaseEipAddress')
                    ->method('POST')
                    ->host($this->vpcHost($regionId))
                    ->options([
                        'query' => [
                            'RegionId' => $regionId,
                            'AllocationId' => $allocationId
                        ],
                        'connect_timeout' => 5.0,
                        'timeout' => 20.0
                    ])
                    ->request();
            }, 'releaseEipAddress');
        } catch (\Exception $e) {
            $message = $e->getMessage();
            if (stripos($message, 'InvalidAllocationId.NotFound') === false && stripos($message, 'not exist') === false) {
                throw $e;
            }
        }

        return true;
    }

    private function releaseEipAddressSilently($key, $secret, $regionId, $allocationId)
    {
        try {
            $this->unassociateEipAddress($key, $secret, $regionId, $allocationId, '');
            $this->waitEipStatus($key, $secret, $regionId, $allocationId, 'Available', 6);
            $this->releaseEipAddress($key, $secret, $regionId, $allocationId);
        } catch (\Exception $e) {
            // 创建失败回滚场景不覆盖原始错误，后台日志由调用方记录。
        }
    }

    private function describeEipAddress($key, $secret, $regionId, $allocationId)
    {
        $result = $this->executeWithRetry(function () use ($key, $secret, $regionId, $allocationId) {
            $this->setDefaultClient($key, $secret, $regionId);

            return AlibabaCloud::rpc()
                ->product('Vpc')
                ->scheme('https')
                ->version('2016-04-28')
                ->action('DescribeEipAddresses')
                ->method('POST')
                ->host($this->vpcHost($regionId))
                ->options([
                    'query' => [
                        'RegionId' => $regionId,
                        'AllocationId' => $allocationId
                    ],
                    'connect_timeout' => 5.0,
                    'timeout' => 15.0
                ])
                ->request();
        }, 'describeEipAddress');

        return $result['EipAddresses']['EipAddress'][0] ?? null;
    }

    private function waitEipStatus($key, $secret, $regionId, $allocationId, $targetStatus, $maxAttempts = 12)
    {
        $last = null;
        for ($i = 0; $i < $maxAttempts; $i++) {
            sleep($i === 0 ? 2 : 4);
            $last = $this->describeEipAddress($key, $secret, $regionId, $allocationId);
            if (!$last) {
                continue;
            }
            if (($last['Status'] ?? '') === $targetStatus) {
                return $last;
            }
        }

        return $last;
    }

    public function releaseManagedEip($account)
    {
        $allocationId = trim((string) ($account['eip_allocation_id'] ?? ''));
        if (($account['public_ip_mode'] ?? '') !== 'eip' || empty($account['eip_managed']) || $allocationId === '') {
            return false;
        }

        $key = $account['access_key_id'];
        $secret = $account['access_key_secret'];
        $regionId = $account['region_id'];
        $this->unassociateEipAddress($key, $secret, $regionId, $allocationId, $account['instance_id'] ?? '');
        $this->waitEipStatus($key, $secret, $regionId, $allocationId, 'Available', 8);
        $this->releaseEipAddress($key, $secret, $regionId, $allocationId);
        return true;
    }

    public function replaceManagedEip($account)
    {
        $oldAllocationId = trim((string) ($account['eip_allocation_id'] ?? ''));
        if (($account['public_ip_mode'] ?? '') !== 'eip' || empty($account['eip_managed']) || $oldAllocationId === '') {
            throw new \Exception('当前实例不是系统托管 EIP，无法更换公网 IP');
        }

        $key = $account['access_key_id'];
        $secret = $account['access_key_secret'];
        $regionId = $account['region_id'];
        $instanceId = $account['instance_id'] ?? '';
        $bandwidth = max(1, (int) ($account['internet_max_bandwidth_out'] ?? 100));

        $newEip = $this->allocateEipAddress($key, $secret, $regionId, $bandwidth, ($account['instance_name'] ?? $instanceId) . '-replace');

        try {
            $this->unassociateEipAddress($key, $secret, $regionId, $oldAllocationId, $instanceId);
            $this->waitEipStatus($key, $secret, $regionId, $oldAllocationId, 'Available', 8);
            $this->associateEipAddress($key, $secret, $regionId, $newEip['allocationId'], $instanceId);
            $this->waitEipStatus($key, $secret, $regionId, $newEip['allocationId'], 'InUse', 12);
            $this->releaseEipAddress($key, $secret, $regionId, $oldAllocationId);
        } catch (\Exception $e) {
            $this->releaseEipAddressSilently($key, $secret, $regionId, $newEip['allocationId'] ?? '');
            throw $e;
        }

        return [
            'publicIp' => $newEip['ipAddress'] ?? '',
            'publicIpMode' => 'eip',
            'eipAllocationId' => $newEip['allocationId'] ?? '',
            'eipAddress' => $newEip['ipAddress'] ?? '',
            'eipManaged' => true,
            'internetMaxBandwidthOut' => $bandwidth
        ];
    }

    private function waitInstanceReady($key, $secret, $regionId, $instanceId)
    {
        $last = null;
        for ($i = 0; $i < 18; $i++) {
            sleep($i === 0 ? 2 : 5);
            $instances = $this->describeInstancesByIds($key, $secret, $regionId, [$instanceId]);
            if (!empty($instances)) {
                $last = $instances[0];
                if (in_array($last['status'], ['Running', 'Stopped'], true)) {
                    return $last;
                }
            }
        }

        return $last ?: ['status' => 'Unknown'];
    }

    private function describeInstancesByIds($key, $secret, $regionId, array $instanceIds)
    {
        $result = $this->executeWithRetry(function () use ($key, $secret, $regionId, $instanceIds) {
            $this->setDefaultClient($key, $secret, $regionId);

            return AlibabaCloud::rpc()
                ->product('Ecs')
                ->scheme('https')
                ->version('2014-05-26')
                ->action('DescribeInstances')
                ->method('POST')
                ->host($this->ecsHost($regionId))
                ->options([
                    'query' => [
                        'RegionId' => $regionId,
                        'InstanceIds' => json_encode(array_values($instanceIds))
                    ],
                    'connect_timeout' => 5.0,
                    'timeout' => 15.0
                ])
                ->request();
        }, 'describeInstancesByIds');

        $items = $result['Instances']['Instance'] ?? [];
        return array_map(function ($instance) {
            return [
                'instanceId' => $instance['InstanceId'] ?? '',
                'instanceName' => $instance['InstanceName'] ?? '',
                'status' => $instance['Status'] ?? 'Unknown',
                'instanceType' => $instance['InstanceType'] ?? '',
                'internetMaxBandwidthOut' => (int) (($instance['EipAddress']['Bandwidth'] ?? 0) ?: ($instance['InternetMaxBandwidthOut'] ?? 0)),
                'publicIp' => $instance['PublicIpAddress']['IpAddress'][0] ?? $instance['EipAddress']['IpAddress'] ?? '',
                'eipAllocationId' => $instance['EipAddress']['AllocationId'] ?? '',
                'eipAddress' => $instance['EipAddress']['IpAddress'] ?? '',
                'privateIp' => $instance['VpcAttributes']['PrivateIpAddress']['IpAddress'][0] ?? ''
            ];
        }, $items);
    }

    private function describeManagedVpcs($key, $secret, $regionId)
    {
        $result = $this->executeWithRetry(function () use ($key, $secret, $regionId) {
            $this->setDefaultClient($key, $secret, $regionId);

            return AlibabaCloud::rpc()
                ->product('Vpc')
                ->scheme('https')
                ->version('2016-04-28')
                ->action('DescribeVpcs')
                ->method('POST')
                ->host($this->vpcHost($regionId))
                ->options([
                    'query' => [
                        'RegionId' => $regionId,
                        'Tag.1.Key' => $this->managedTagKey,
                        'Tag.1.Value' => $this->managedTagValue
                    ],
                    'connect_timeout' => 5.0,
                    'timeout' => 15.0
                ])
                ->request();
        }, 'describeManagedVpcs');

        return $result['Vpcs']['Vpc'] ?? [];
    }

    private function describeManagedVSwitches($key, $secret, $regionId, $vpcId, $zoneId)
    {
        $result = $this->executeWithRetry(function () use ($key, $secret, $regionId, $vpcId, $zoneId) {
            $this->setDefaultClient($key, $secret, $regionId);

            return AlibabaCloud::rpc()
                ->product('Vpc')
                ->scheme('https')
                ->version('2016-04-28')
                ->action('DescribeVSwitches')
                ->method('POST')
                ->host($this->vpcHost($regionId))
                ->options([
                    'query' => [
                        'RegionId' => $regionId,
                        'VpcId' => $vpcId,
                        'ZoneId' => $zoneId,
                        'Tag.1.Key' => $this->managedTagKey,
                        'Tag.1.Value' => $this->managedTagValue
                    ],
                    'connect_timeout' => 5.0,
                    'timeout' => 15.0
                ])
                ->request();
        }, 'describeManagedVSwitches');

        return $result['VSwitches']['VSwitch'] ?? [];
    }

    private function describeManagedSecurityGroups($key, $secret, $regionId, $vpcId)
    {
        $result = $this->executeWithRetry(function () use ($key, $secret, $regionId, $vpcId) {
            $this->setDefaultClient($key, $secret, $regionId);

            return AlibabaCloud::rpc()
                ->product('Ecs')
                ->scheme('https')
                ->version('2014-05-26')
                ->action('DescribeSecurityGroups')
                ->method('POST')
                ->host($this->ecsHost($regionId))
                ->options([
                    'query' => [
                        'RegionId' => $regionId,
                        'VpcId' => $vpcId,
                        'Tag.1.Key' => $this->managedTagKey,
                        'Tag.1.Value' => $this->managedTagValue
                    ],
                    'connect_timeout' => 5.0,
                    'timeout' => 15.0
                ])
                ->request();
        }, 'describeManagedSecurityGroups');

        return $result['SecurityGroups']['SecurityGroup'] ?? [];
    }

    private function defaultMinSystemDiskSize($osKey)
    {
        return 20;
    }

    private function getSystemDiskSizeRange($key, $secret, $regionId, $zoneId, $instanceType, $diskCategory)
    {
        $result = $this->executeWithRetry(function () use ($key, $secret, $regionId, $zoneId, $instanceType, $diskCategory) {
            $this->setDefaultClient($key, $secret, $regionId);

            return AlibabaCloud::rpc()
                ->product('Ecs')
                ->scheme('https')
                ->version('2014-05-26')
                ->action('DescribeAvailableResource')
                ->method('POST')
                ->host($this->ecsHost($regionId))
                ->options([
                    'query' => [
                        'RegionId' => $regionId,
                        'ZoneId' => $zoneId,
                        'DestinationResource' => 'SystemDisk',
                        'ResourceType' => 'instance',
                        'InstanceType' => $instanceType,
                        'SystemDiskCategory' => $diskCategory,
                        'IoOptimized' => 'optimized',
                        'NetworkCategory' => 'vpc',
                        'InstanceChargeType' => 'PostPaid'
                    ],
                    'connect_timeout' => 5.0,
                    'timeout' => 15.0
                ])
                ->request();
        }, 'getSystemDiskSizeRange');

        $zones = $result['AvailableZones']['AvailableZone'] ?? [];
        foreach ($zones as $zone) {
            $resources = $zone['AvailableResources']['AvailableResource'] ?? [];
            foreach ($resources as $resource) {
                if (($resource['Type'] ?? '') !== 'SystemDisk') {
                    continue;
                }

                $supported = $resource['SupportedResources']['SupportedResource'] ?? [];
                foreach ($supported as $item) {
                    $value = $item['Value'] ?? '';
                    if ($value !== '' && $value !== $diskCategory) {
                        continue;
                    }

                    return [
                        'min' => max(1, (int) ($item['Min'] ?? 20)),
                        'max' => max(1, (int) ($item['Max'] ?? 2048)),
                        'unit' => $item['Unit'] ?? 'GiB',
                        'status' => $item['Status'] ?? '',
                        'statusCategory' => $item['StatusCategory'] ?? ''
                    ];
                }
            }
        }

        throw new \Exception("当前可用区/规格/磁盘类型未返回系统盘容量范围，请更换磁盘类型或实例规格后重试");
    }

    private function normalizeSystemDiskSize($value, array $range = [])
    {
        $size = (int) $value;
        $min = (int) ($range['min'] ?? 20);
        $max = (int) ($range['max'] ?? 2048);
        $unit = $range['unit'] ?? 'GiB';

        if ($size < $min || $size > $max) {
            throw new \Exception("系统盘大小必须在当前 API 返回范围 {$min}-{$max} {$unit} 之间");
        }
        return $size;
    }

    private function isDiskSizeError($message)
    {
        $message = strtolower((string) $message);
        return strpos($message, 'systemdisk.size') !== false
            || strpos($message, 'invalidsystemdisksize') !== false
            || (strpos($message, 'disk') !== false && strpos($message, 'size') !== false);
    }

    private function selectDiskCategory($zone, $requested = 'cloud_essd_entry')
    {
        $raw = $zone['raw']['AvailableDiskCategories']['DiskCategories'] ?? $zone['raw']['AvailableDiskCategories']['DiskCategory'] ?? [];
        $categories = is_array($raw) ? $raw : [];
        $requested = trim((string) $requested);
        if ($requested !== '') {
            if (empty($categories) || in_array($requested, $categories, true)) {
                return $requested;
            }
            throw new \Exception("当前可用区不支持所选硬盘类型 {$requested}，请更换硬盘类型或实例规格后重试");
        }

        foreach (['cloud_essd_entry', 'cloud_essd', 'cloud_efficiency', 'cloud'] as $preferred) {
            if (empty($categories) || in_array($preferred, $categories, true)) {
                return $preferred;
            }
        }
        return 'cloud_essd_entry';
    }

    private function diskCategoryLabel($category)
    {
        $map = [
            'cloud_essd_entry' => 'ESSD Entry 云盘',
            'cloud_essd' => 'ESSD 云盘',
            'cloud_efficiency' => '高效云盘',
            'cloud' => '普通云盘'
        ];

        return $map[$category] ?? $category;
    }

    private function estimateMaxBandwidthOut($instanceType, $regionId)
    {
        return 200;
    }

    private function bandwidthCandidates($max)
    {
        $base = [200, 100, 50, 30, 20, 10, 5, 1];
        $candidates = array_values(array_filter($base, function ($value) use ($max) {
            return $value <= max(1, $max);
        }));
        if (!in_array($max, $candidates, true)) {
            array_unshift($candidates, $max);
        }
        return array_values(array_unique($candidates));
    }

    private function normalizePublicCidr($ip)
    {
        $ip = trim((string) $ip);
        if (filter_var($ip, FILTER_VALIDATE_IP, FILTER_FLAG_IPV4 | FILTER_FLAG_NO_PRIV_RANGE | FILTER_FLAG_NO_RES_RANGE)) {
            return $ip . '/32';
        }
        return '';
    }

    private function cidrForZone($zoneId)
    {
        $hash = abs(crc32($zoneId));
        $third = 1 + ($hash % 200);
        return "172.31.{$third}.0/24";
    }

    private function generateInstancePassword()
    {
        $lower = 'abcdefghijkmnopqrstuvwxyz';
        $upper = 'ABCDEFGHJKLMNPQRSTUVWXYZ';
        $digits = '23456789';
        $symbols = '!@#%^*';
        $all = $lower . $upper . $digits . $symbols;
        $password = $lower[random_int(0, strlen($lower) - 1)]
            . $upper[random_int(0, strlen($upper) - 1)]
            . $digits[random_int(0, strlen($digits) - 1)]
            . $symbols[random_int(0, strlen($symbols) - 1)];
        for ($i = strlen($password); $i < 16; $i++) {
            $password .= $all[random_int(0, strlen($all) - 1)];
        }
        return str_shuffle($password);
    }

    private function emitProgress($progress, $step)
    {
        if (is_callable($progress)) {
            $progress($step);
        }
    }

    // ==================== BSS 费用中心 API ====================

    private $balanceCache = [];

    /**
     * 查询账户可用余额
     * @param string $key AccessKey
     * @param string $secret Secret
     * @return array ['AvailableAmount' => '...', 'Currency' => 'CNY']
     * @throws \Exception
     */
    public function getAccountBalance($key, $secret, $siteType = 'china')
    {
        $cacheKey = md5($key);
        if (isset($this->balanceCache[$cacheKey])) {
            return $this->balanceCache[$cacheKey];
        }

        $bss = $this->getBssEndpoint($siteType);

        $result = $this->executeWithRetry(function () use ($key, $secret, $bss) {
            AlibabaCloud::accessKeyClient($key, $secret)
                ->regionId($bss['regionId'])
                ->asDefaultClient();

            return AlibabaCloud::rpc()
                ->product('BssOpenApi')
                ->scheme('https')
                ->version('2017-12-14')
                ->action('QueryAccountBalance')
                ->method('POST')
                ->host($bss['host'])
                ->options([
                    'connect_timeout' => 5.0,
                    'timeout' => 10.0
                ])
                ->request();
        }, 'getAccountBalance');

        $data = [
            'AvailableAmount' => $result['Data']['AvailableAmount'] ?? '0',
            'Currency' => $result['Data']['Currency'] ?? 'CNY'
        ];

        $this->balanceCache[$cacheKey] = $data;
        return $data;
    }

    /**
     * 查询指定实例的当月账单明细
     * @param string $key AccessKey
     * @param string $secret Secret
     * @param string $instanceId 实例ID
     * @param string $billingCycle 账期 (格式: 2026-03)
     * @return array ['TotalCost' => float, 'Items' => [...]]
     * @throws \Exception
     */
    public function getInstanceBill($key, $secret, $instanceId, $billingCycle, $siteType = 'china')
    {
        $bss = $this->getBssEndpoint($siteType);

        $result = $this->executeWithRetry(function () use ($key, $secret, $instanceId, $billingCycle, $bss) {
            AlibabaCloud::accessKeyClient($key, $secret)
                ->regionId($bss['regionId'])
                ->asDefaultClient();

            return AlibabaCloud::rpc()
                ->product('BssOpenApi')
                ->scheme('https')
                ->version('2017-12-14')
                ->action('DescribeInstanceBill')
                ->method('POST')
                ->host($bss['host'])
                ->options([
                    'query' => [
                        'BillingCycle' => $billingCycle,
                        'InstanceID' => $instanceId,
                        'Granularity' => 'MONTHLY'
                    ],
                    'connect_timeout' => 5.0,
                    'timeout' => 15.0
                ])
                ->request();
        }, 'getInstanceBill');

        $items = $result['Data']['Items'] ?? [];
        $totalCost = 0;
        $details = [];

        foreach ($items as $item) {
            $cost = (float) ($item['PretaxAmount'] ?? 0);
            $totalCost += $cost;
            $details[] = [
                'ProductName' => $item['ProductName'] ?? '',
                'ProductCode' => $item['ProductCode'] ?? '',
                'BillingType' => $item['BillingType'] ?? '',
                'PretaxAmount' => $cost,
                'DeductedByCashCoupons' => (float) ($item['DeductedByCashCoupons'] ?? 0),
                'DeductedByPrepaidCard' => (float) ($item['DeductedByPrepaidCard'] ?? 0),
                'PaymentAmount' => (float) ($item['PaymentAmount'] ?? 0),
            ];
        }

        return [
            'TotalCost' => round($totalCost, 2),
            'Items' => $details
        ];
    }

    /**
     * 查询账单总览 (按产品分类的月度费用)
     * @param string $key AccessKey
     * @param string $secret Secret
     * @param string $billingCycle 账期 (格式: 2026-03)
     * @return array ['TotalCost' => float, 'Products' => [...]]
     * @throws \Exception
     */
    public function getBillOverview($key, $secret, $billingCycle, $siteType = 'china')
    {
        $bss = $this->getBssEndpoint($siteType);

        $result = $this->executeWithRetry(function () use ($key, $secret, $billingCycle, $bss) {
            AlibabaCloud::accessKeyClient($key, $secret)
                ->regionId($bss['regionId'])
                ->asDefaultClient();

            return AlibabaCloud::rpc()
                ->product('BssOpenApi')
                ->scheme('https')
                ->version('2017-12-14')
                ->action('QueryBillOverview')
                ->method('POST')
                ->host($bss['host'])
                ->options([
                    'query' => [
                        'BillingCycle' => $billingCycle
                    ],
                    'connect_timeout' => 5.0,
                    'timeout' => 15.0
                ])
                ->request();
        }, 'getBillOverview');

        $items = $result['Data']['Items']['Item'] ?? [];
        $totalCost = 0;
        $products = [];

        foreach ($items as $item) {
            $cost = (float) ($item['PretaxAmount'] ?? 0);
            if ($cost <= 0) continue;
            $totalCost += $cost;
            $products[] = [
                'ProductName' => $item['ProductName'] ?? '',
                'ProductCode' => $item['ProductCode'] ?? '',
                'PretaxAmount' => round($cost, 2),
                'PaymentAmount' => round((float) ($item['PaymentAmount'] ?? 0), 2)
            ];
        }

        // 按费用降序排列
        usort($products, function ($a, $b) {
            return $b['PretaxAmount'] <=> $a['PretaxAmount'];
        });

        return [
            'TotalCost' => round($totalCost, 2),
            'Products' => $products
        ];
    }
}
