<?php

require 'vendor/autoload.php';
require_once 'Database.php';
require_once 'ConfigManager.php';
require_once 'AliyunService.php';
require_once 'NotificationService.php';
require_once 'DdnsService.php';

use AlibabaCloud\Client\Exception\ClientException;
use AlibabaCloud\Client\Exception\ServerException;

class AliyunTrafficCheck
{
    private $db;
    private $configManager;
    private $aliyunService;
    private $notificationService;
    private $ddnsService;
    private $initError = null;



    public function __construct()
    {
        try {
            $this->db = new Database();
            $this->configManager = new ConfigManager($this->db);
            $this->aliyunService = new AliyunService();
            $this->notificationService = new NotificationService();
            $this->ddnsService = new DdnsService($this->configManager->getAllSettings());

            // 注入配置到通知服务
            $this->notificationService->setConfig($this->configManager->getAllSettings());

        } catch (Exception $e) {
            $this->initError = $e->getMessage();
        }
    }

    public function getInitError()
    {
        return $this->initError;
    }

    public function isInitialized()
    {
        if ($this->initError)
            return false;
        return $this->configManager->isInitialized();
    }

    public function getAdminPassword()
    {
        return $this->configManager->get('admin_password', '');
    }

    public function getMonitorKey()
    {
        $key = $this->configManager->get('monitor_key', '');
        if (empty($key)) {
            $key = bin2hex(random_bytes(32));
            $this->configManager->saveMonitorKey($key);
        }
        return $key;
    }

    public function login($password)
    {
        $ip = $_SERVER['REMOTE_ADDR'] ?? '0.0.0.0';
        if (isset($_SERVER['HTTP_X_FORWARDED_FOR'])) {
            $ips = explode(',', $_SERVER['HTTP_X_FORWARDED_FOR']);
            $ip = trim($ips[0]);
        }

        $attempts = $this->db->getRecentFailedAttempts($ip, 900);
        if ($attempts >= 5) {
            $this->db->addLog('warning', "登录被锁定: 地址 {$ip} 尝试次数过多");
            throw new Exception("错误次数过多，请 15 分钟后再试。");
        }

        $adminPass = $this->getAdminPassword();
        if (empty($adminPass))
            return false;

        $passwordValid = false;

        if ($this->isPasswordHashed($adminPass)) {
            $passwordValid = password_verify($password, $adminPass);
        } else {
            $passwordValid = hash_equals($adminPass, $password);
            if ($passwordValid) {
                $this->configManager->upgradePasswordHash($password);
            }
        }

        if ($passwordValid) {
            $this->db->clearLoginAttempts($ip);
            $this->db->addLog('info', "管理员登录成功 [地址: {$ip}]");
            return true;
        }

        $this->db->recordLoginAttempt($ip);
        $this->db->addLog('warning', "管理员登录失败 [地址: {$ip}]");
        return false;
    }

    private function isPasswordHashed($password)
    {
        return preg_match('/^\$2[aby]?\$/', $password) === 1 || preg_match('/^\$argon2[aid]\$/', $password) === 1;
    }

    private function getAccountLogLabel($account)
    {
        $remark = trim((string) ($account['remark'] ?? ''));
        if ($remark !== '') {
            return $remark;
        }

        $instanceName = trim((string) ($account['instance_name'] ?? ''));
        if ($instanceName !== '') {
            return $instanceName;
        }

        $instanceId = trim((string) ($account['instance_id'] ?? ''));
        if ($instanceId !== '') {
            return $instanceId;
        }

        return substr((string) ($account['access_key_id'] ?? ''), 0, 7) . '***';
    }

    private function resolveSecretFromDatabase($accessKeyId, $regionId)
    {
        $pdo = $this->db->getPdo();
        $stmt = $pdo->prepare("SELECT access_key_secret FROM accounts WHERE access_key_id = ? AND region_id = ? LIMIT 1");
        $stmt->execute([$accessKeyId, $regionId]);
        $row = $stmt->fetch();

        if ($row && !empty($row['access_key_secret'])) {
            $secret = $this->configManager->decryptAccountSecret($row['access_key_secret']);
            if (!empty($secret)) {
                return $secret;
            }
        }

        foreach ($this->configManager->getAccountGroups() as $group) {
            if (
                ($group['AccessKeyId'] ?? '') === $accessKeyId
                && ($group['regionId'] ?? '') === $regionId
                && !empty($group['AccessKeySecret'])
                && $group['AccessKeySecret'] !== '********'
            ) {
                return $group['AccessKeySecret'];
            }
        }

        throw new Exception('无法读取该账号的AK Secret，请重新输入后保存');
    }

    public function setup($data)
    {
        if ($this->initError)
            throw new Exception($this->initError);
        if ($this->isInitialized())
            return false;
        return $this->configManager->updateConfig($data);
    }

    public function updateConfig($data)
    {
        $success = $this->configManager->updateConfig($data);
        if ($success) {
            $this->notificationService->setConfig($this->configManager->getAllSettings());
        }
        return $success;
    }

    public function getConfigForFrontend()
    {
        if ($this->initError)
            return [];

        $settings = $this->configManager->getAllSettings();
        $accountGroups = $this->configManager->getAccountGroups();
        $groupMetrics = $this->configManager->getAccountGroupMetrics();
        $billingMetrics = $this->getAccountGroupBillingMetrics();

        $config = [
            'admin_password' => !empty($settings['admin_password']) ? '********' : '',
            'admin_password_set' => !empty($settings['admin_password']),
            'traffic_threshold' => (int) ($settings['traffic_threshold'] ?? 95),
            'shutdown_mode' => $settings['shutdown_mode'] ?? 'KeepCharging',
            'threshold_action' => $settings['threshold_action'] ?? 'stop_and_notify',
            'keep_alive' => ($settings['keep_alive'] ?? '0') === '1',
            'monthly_auto_start' => ($settings['monthly_auto_start'] ?? '0') === '1',
            'api_interval' => (int) ($settings['api_interval'] ?? 600),
            'enable_billing' => ($settings['enable_billing'] ?? '0') === '1',
            'Notification' => [
                'email_enabled' => ($settings['notify_email_enabled'] ?? '1') === '1',
                'email' => $settings['notify_email'] ?? '',
                'host' => $settings['notify_host'] ?? '',
                'port' => $settings['notify_port'] ?? 465,
                'username' => $settings['notify_username'] ?? '',
                'password' => !empty($settings['notify_password']) ? '********' : '',
                'secure' => $settings['notify_secure'] ?? 'ssl',
                'telegram' => [
                    'enabled' => ($settings['notify_tg_enabled'] ?? '0') === '1',
                    'token' => !empty($settings['notify_tg_token']) ? '********' : '',
                    'chat_id' => $settings['notify_tg_chat_id'] ?? '',
                    'proxy_type' => $settings['notify_tg_proxy_type'] ?? 'none',
                    'proxy_url' => $settings['notify_tg_proxy_url'] ?? '',
                    'proxy_ip' => $settings['notify_tg_proxy_ip'] ?? '',
                    'proxy_port' => $settings['notify_tg_proxy_port'] ?? '',
                    'proxy_user' => $settings['notify_tg_proxy_user'] ?? '',
                    'proxy_pass' => !empty($settings['notify_tg_proxy_pass']) ? '********' : ''
                ],
                'webhook' => [
                    'enabled' => ($settings['notify_wh_enabled'] ?? '0') === '1',
                    'url' => $settings['notify_wh_url'] ?? '',
                    'method' => $settings['notify_wh_method'] ?? 'GET',
                    'request_type' => $settings['notify_wh_request_type'] ?? 'JSON',
                    'headers' => $settings['notify_wh_headers'] ?? '',
                    'body' => $settings['notify_wh_body'] ?? ''
                ]
            ],
            'Ddns' => [
                'enabled' => ($settings['ddns_enabled'] ?? '0') === '1',
                'provider' => $settings['ddns_provider'] ?? 'cloudflare',
                'domain' => $settings['ddns_domain'] ?? '',
                'cloudflare' => [
                    'zone_id' => $settings['ddns_cf_zone_id'] ?? '',
                    'token' => !empty($settings['ddns_cf_token']) ? '********' : '',
                    'proxied' => ($settings['ddns_cf_proxied'] ?? '0') === '1'
                ]
            ],
            'Accounts' => []
        ];

        foreach ($accountGroups as $row) {
            $metrics = $groupMetrics[$row['groupKey']] ?? [
                'usageUsed' => 0,
                'usageRemaining' => (float) ($row['maxTraffic'] ?? 0),
                'usagePercent' => 0,
                'instanceCount' => 0,
                'lastUpdated' => 0
            ];
            $config['Accounts'][] = [
                'AccessKeyId' => $row['AccessKeyId'],
                'AccessKeySecret' => '********',
                'AccessKeySecretSet' => !empty($row['AccessKeySecret']),
                'regionId' => $row['regionId'],
                'maxTraffic' => (float) $row['maxTraffic'],
                'remark' => $row['remark'] ?? '',
                'siteType' => $row['siteType'] ?? 'international',
                'groupKey' => $row['groupKey'] ?? '',
                'usageUsed' => round((float) ($metrics['usageUsed'] ?? 0), 6),
                'usageRemaining' => round((float) ($metrics['usageRemaining'] ?? 0), 6),
                'usagePercent' => round((float) ($metrics['usagePercent'] ?? 0), 2),
                'instanceCount' => (int) ($metrics['instanceCount'] ?? 0),
                'usageLastUpdated' => !empty($metrics['lastUpdated']) ? date('Y-m-d H:i:s', (int) $metrics['lastUpdated']) : '',
                'billing' => $billingMetrics[$row['groupKey']] ?? [
                    'enabled' => ($settings['enable_billing'] ?? '0') === '1',
                    'monthly_cost' => null,
                    'balance' => null,
                    'currency' => ($row['siteType'] ?? 'international') === 'international' ? 'USD' : 'CNY',
                    'last_updated' => null,
                    'error' => null
                ]
            ];
        }

        return $config;
    }

    // --- 修改：支持按 Tab 获取日志 ---
    public function getSystemLogs($tab = 'action')
    {
        if ($this->initError)
            return [];

        if ($tab === 'heartbeat') {
            // 心跳日志：只看 heartbeat 类型
            $types = ['heartbeat'];
        } else {
            // 动作日志：只看 info 和 warning，排除 error (超时/接口错误)
            $types = ['info', 'warning'];
        }

        // 仅返回最近 20 条
        $logs = $this->db->getLogsByTypes($types, 20);
        $accounts = $this->configManager->getAccounts();
        $accessKeyMap = [];

        foreach ($accounts as $account) {
            $label = $this->getAccountLogLabel($account);
            $accessKeyId = trim((string) ($account['access_key_id'] ?? ''));
            if ($accessKeyId === '') {
                continue;
            }

            $accessKeyMap[$accessKeyId] = $label;
            $accessKeyMap[substr($accessKeyId, 0, 7) . '***'] = $label;
        }

        foreach ($logs as &$log) {
            foreach ($accessKeyMap as $key => $label) {
                $log['message'] = str_replace("[$key]", "[$label]", $log['message']);
                $log['message'] = str_replace($key, $label, $log['message']);
            }
            $log['time_str'] = date('Y-m-d H:i:s', $log['created_at']);
        }
        return $logs;
    }

    // --- 新增：清空日志并重排 ID ---
    public function clearSystemLogs($tab = 'action')
    {
        if ($this->initError)
            return false;

        $result = false;
        if ($tab === 'heartbeat') {
            $result = $this->db->clearLogsByTypes(['heartbeat']);
        } else {
            $result = $this->db->clearLogsByTypes(['info', 'warning', 'error']);
        }

        // 关键改动：清空后立即重排剩余 ID
        if ($result) {
            $this->db->reorderLogsIds();
        }

        return $result;
    }

    public function getAccountHistory($id)
    {
        if ($this->initError)
            return [];

        $account = $this->configManager->getAccountById($id);
        if (!$account)
            return ['error' => 'Account not found'];

        // Use account ID for stats query
        $rawHourly = $this->db->getHourlyStats($id);
        $chartHourly = [];
        foreach ($rawHourly as $row) {
            $chartHourly[] = [
                'time' => date('H:00', $row['recorded_at']),
                'full_time' => date('Y-m-d H:i', $row['recorded_at']),
                'value' => round($row['traffic'], 3)
            ];
        }

        $rawDaily = $this->db->getDailyStats($id);
        $chartDaily = [];
        foreach ($rawDaily as $row) {
            $chartDaily[] = [
                'date' => date('Y-m-d', $row['recorded_at']),
                'value' => round($row['traffic'], 3)
            ];
        }

        return [
            'history_24h' => $chartHourly,
            'history_30d' => $chartDaily
        ];
    }

    // --- 核心监控逻辑 ---

    public function monitor()
    {
        if ($this->initError)
            return "错误: " . $this->initError;

        // 优化：分级清理日志
        // 普通/重要日志保留 30 天，高频心跳日志仅保留 3 天
        $this->db->pruneLogs(30, 3);

        // 关键改动：每次清理后重排 ID，保证 ID 永远紧凑
        $this->db->reorderLogsIds();

        $this->db->pruneStats();

        // 优化：每天凌晨 04:xx 执行一次 VACUUM 整理数据库碎片
        if (date('H') === '04' && date('i') === '00') {
            $this->db->vacuum();
        }

        $logs = [];
        $currentTime = time();

        $threshold = (int) $this->configManager->get('traffic_threshold', 95);
        $shutdownMode = $this->configManager->get('shutdown_mode', 'KeepCharging');
        $thresholdAction = $this->configManager->get('threshold_action', 'stop_and_notify');
        $keepAlive = $this->configManager->get('keep_alive', '0') === '1';
        $monthlyAutoStart = $this->configManager->get('monthly_auto_start', '0') === '1';
        $userInterval = (int) $this->configManager->get('api_interval', 600);

        $accounts = $this->configManager->getAccounts();

        foreach ($accounts as $account) {
            $accountLabel = $this->getAccountLogLabel($account);
            $logPrefix = "[{$accountLabel}]";
            $actions = [];
            $forceRefresh = false;

            // 1. 自适应心跳
            $lastUpdate = $account['updated_at'] ?? 0;
            $cachedStatus = $account['instance_status'] ?? 'Unknown';
            $isTransientState = in_array($cachedStatus, ['Starting', 'Stopping', 'Pending', 'Unknown']);
            $currentInterval = $isTransientState ? 60 : $userInterval;

            $shouldCheckApi = $forceRefresh || (($currentTime - $lastUpdate) > $currentInterval);

            if (date('i') === '00') {
                $shouldCheckApi = true;
            }

            $newUpdateTime = $currentTime;

            if ($shouldCheckApi) {
                $newTraffic = $this->safeGetTraffic($account);
                $status = $this->safeGetInstanceStatus($account);

                if ($status === 'Unknown') {
                    usleep(500000);
                    $status = $this->safeGetInstanceStatus($account);
                }

                if ($newTraffic < 0) {
                    $traffic = $account['traffic_used'];
                    $apiStatusLog = "流量接口异常";
                    $newUpdateTime = $lastUpdate;
                } else {
                    $traffic = $newTraffic;
                    $apiStatusLog = "已更新";

                    $this->db->addHourlyStat($account['id'], $traffic);
                    $this->db->addDailyStat($account['id'], $traffic);
                }

                if ($status === 'Unknown') {
                    $newUpdateTime = $lastUpdate;
                    $apiStatusLog .= "(状态Unknown)";
                } else {
                    $apiStatusLog .= in_array($status, ['Starting', 'Stopping', 'Pending']) ? " [过渡态]" : " [稳定态]";
                }

                $this->notifyStatusChangeIfNeeded($account, $cachedStatus, $status, '系统同步检测到实例状态变化。');
                $this->configManager->updateAccountStatus($account['id'], $traffic, $status, $newUpdateTime);
            } else {
                $traffic = $account['traffic_used'];
                $status = $account['instance_status'];
                $timeLeft = $currentInterval - ($currentTime - $lastUpdate);
                $apiStatusLog = "缓存({$timeLeft}s)";
            }

            $maxTraffic = $account['max_traffic'];
            $accountTraffic = $this->getGroupTrafficUsed($account);
            $usagePercent = ($maxTraffic > 0) ? round(($accountTraffic / $maxTraffic) * 100, 2) : 0;
            $trafficDesc = "账号出口流量:{$usagePercent}%";
            $isOverThreshold = $usagePercent >= $threshold;
            $isHardLimitExceeded = $maxTraffic > 0 && $accountTraffic >= $maxTraffic;
            $requiresTrafficProtection = $isOverThreshold || $isHardLimitExceeded;

            // 2. 流量熔断
            if ($requiresTrafficProtection) {
                $trafficDesc .= $isHardLimitExceeded ? "[已超出上限]" : "[接近上限]";

                if ($thresholdAction === 'stop_and_notify') {
                    $canAttemptStop = !in_array($status, ['Stopped', 'Stopping', 'Released'], true);

                    // 达到账号流量上限后必须立即保护，不再等待下一次接口刷新窗口。
                    if ($canAttemptStop) {
                        if ($this->safeControlInstance($account, 'stop', $shutdownMode)) {
                            $previousStatus = $status;
                            $actions[] = $isHardLimitExceeded ? "已超量自动停机" : "接近上限自动停机";
                            $this->db->addLog('warning', "账号出口流量达到保护线，已自动停机 [{$accountLabel}] 当前使用率:{$usagePercent}%");
                            $this->configManager->updateAccountStatus($account['id'], $traffic, 'Stopping', $currentTime);
                            $this->notifyStatusChangeIfNeeded($account, $previousStatus, 'Stopping', '流量达到保护线，已自动停机。');
                            $status = 'Stopping';
                        } else {
                            $actions[] = "自动停机失败";
                            $this->db->addLog('error', "账号出口流量达到保护线，但自动停机失败 [{$accountLabel}] 当前使用率:{$usagePercent}%");
                        }
                    }
                } elseif ($shouldCheckApi) {
                    $actions[] = "超量提醒";
                    $this->db->addLog('warning', "账号出口流量超限触发提醒 [{$accountLabel}] 当前使用率:{$usagePercent}%");
                }

                if (!empty($actions)) {
                    $mailRes = $this->notificationService->sendTrafficWarning($accountLabel, $accountTraffic, $usagePercent, implode(',', $actions), $threshold);
                    $this->logNotificationResult($mailRes, $accountLabel);
                }
            }

            // 3. 每月自动开机：只在每月 1 号执行，且不会启动已经触发流量保护的实例。
            $autoStartBlocked = !empty($account['auto_start_blocked']);
            if ($monthlyAutoStart && !$autoStartBlocked && !$requiresTrafficProtection && date('j', $currentTime) === '1') {
                $lastMonthlyStart = (int) ($account['last_keep_alive_at'] ?? 0);
                if ($status === 'Stopped' && !$this->isSameMonth($lastMonthlyStart, $currentTime)) {
                    if ($this->safeControlInstance($account, 'start')) {
                        $actions[] = "月初自动开机";
                        $this->db->addLog('info', "执行月初自动开机 [{$accountLabel}]");
                        $this->configManager->updateAccountStatus($account['id'], $traffic, 'Starting', $currentTime);
                        $this->configManager->updateLastKeepAlive($account['id'], $currentTime);
                        $this->notifyStatusChangeIfNeeded($account, 'Stopped', 'Starting', '每月 1 号自动开机已执行。');
                        $status = 'Starting';
                    } else {
                        $apiStatusLog .= " [月初自动开机失败,下次重试]";
                    }
                }
            }

            // 4. 保活逻辑
            if ($keepAlive && !$autoStartBlocked && !$requiresTrafficProtection) {
                if ($status === 'Stopped') {
                    if ($this->safeControlInstance($account, 'start')) {
                        $actions[] = "保活启动";
                        $this->db->addLog('info', "执行保活启动 [{$accountLabel}]");

                        $mailRes = $this->notificationService->notifySchedule("保活启动", $account, "检测到实例非预期关机，已尝试自动启动。");
                        $this->logNotificationResult($mailRes, $accountLabel);

                        $this->configManager->updateAccountStatus($account['id'], $traffic, 'Starting', $currentTime);
                        $this->configManager->updateLastKeepAlive($account['id'], $currentTime);
                        $this->notifyStatusChangeIfNeeded($account, 'Stopped', 'Starting', '检测到实例非预期关机，保活已尝试自动启动。');
                        $status = 'Starting';
                    } else {
                        $apiStatusLog .= " [保活启动失败,下次重试]";
                    }
                }
            }


            $actionLog = empty($actions) ? "无动作" : implode(", ", $actions);
            $logLine = sprintf("%s %s | %s | %s | %s", $logPrefix, $actionLog, $trafficDesc, $status, $apiStatusLog);

            // --- 修改：将心跳日志写入数据库 ---
            $this->db->addLog('heartbeat', $logLine);
            $logs[] = $logLine;
        }

        $this->configManager->updateLastRunTime(time());

        // 执行异步彻底销毁循环
        $this->processPendingReleases();

        return implode(PHP_EOL, $logs);
    }

    public function getStatusForFrontend($includeSensitive = false)
    {
        if ($this->initError)
            return ['error' => $this->initError];

        $this->configManager->syncAccountGroups();

        $data = [];
        $threshold = (int) $this->configManager->get('traffic_threshold', 95);
        $userInterval = (int) $this->configManager->get('api_interval', 600);
        $billingEnabled = $this->configManager->get('enable_billing', '0') === '1';
        $accounts = array_values(array_filter($this->configManager->getAccounts(), function ($account) {
            return !empty($account['instance_id']);
        }));

        foreach ($accounts as $account) {
            $data[] = $this->buildInstanceSnapshot($account, $threshold, $userInterval, $billingEnabled, $includeSensitive);
        }

        $pendingAccounts = $this->configManager->getPendingReleaseAccounts();
        foreach ($pendingAccounts as $account) {
            $snap = $this->buildInstanceSnapshot($account, $threshold, $userInterval, $billingEnabled, $includeSensitive);
            $snap['instanceStatus'] = 'Releasing';
            $snap['status'] = 'Releasing';
            $data[] = $snap;
        }

        return [
            'data' => $data,
            'system_last_run' => $this->configManager->getLastRunTime(),
            'sync_interval' => $userInterval,
            'sensitive_visible' => $includeSensitive
        ];
    }

    public function refreshAccount($id)
    {
        if ($this->initError)
            return false;

        $targetAccount = $this->configManager->getAccountById($id);
        if (!$targetAccount)
            return false;

        $currentTime = time();
        $traffic = $this->safeGetTraffic($targetAccount);
        $status = $this->safeGetInstanceStatus($targetAccount);

        if ($traffic < 0) {
            $traffic = $targetAccount['traffic_used'];
        } else {
            $this->db->addHourlyStat($targetAccount['id'], $traffic);
            $this->db->addDailyStat($targetAccount['id'], $traffic);
        }

        $this->notifyStatusChangeIfNeeded($targetAccount, $targetAccount['instance_status'] ?? 'Unknown', $status, '手动同步检测到实例状态变化。');
        $this->configManager->updateAccountStatus($id, $traffic, $status, $currentTime);

        // 刷新账单数据：仅在启用费用监控 且 无有效缓存时调用 费用中心 接口
        $billingError = null;
        $billingEnabled = $this->configManager->get('enable_billing', '0') === '1';
        if ($billingEnabled) {
            $billingCycle = date('Y-m');

            // 余额：无有效缓存时重新获取
            $balanceCache = $this->db->getBillingCache($targetAccount['id'], 'balance', '', 21600);
            if (!$balanceCache) {
                try {
                    $balance = $this->aliyunService->getAccountBalance(
                        $targetAccount['access_key_id'],
                        $targetAccount['access_key_secret'],
                        $targetAccount['site_type'] ?? 'china'
                    );
                    $this->db->setBillingCache($targetAccount['id'], 'balance', '', $balance);
                } catch (\Exception $e) {
                    $billingError = '余额查询失败: ' . $e->getMessage();
                }
            }

            // 实例账单：无有效缓存时重新获取
            if (!empty($targetAccount['instance_id'])) {
                $billCache = $this->db->getBillingCache($targetAccount['id'], 'instance_bill', $billingCycle, 21600);
                if (!$billCache) {
                    try {
                        $bill = $this->aliyunService->getInstanceBill(
                            $targetAccount['access_key_id'],
                            $targetAccount['access_key_secret'],
                            $targetAccount['instance_id'],
                            $billingCycle,
                            $targetAccount['site_type'] ?? 'china'
                        );
                        $this->db->setBillingCache($targetAccount['id'], 'instance_bill', $billingCycle, $bill);
                    } catch (\Exception $e) {
                        $billingError = ($billingError ? $billingError . '; ' : '') . '账单查询失败: ' . $e->getMessage();
                    }
                }
            }
        }

        if ($billingError) {
            $this->db->addLog('warning', "账单刷新异常 [{$this->getAccountLogLabel($targetAccount)}]: {$billingError}");
            return ['success' => true, 'billing_error' => $billingError];
        }

        return true;
    }

    public function fetchInstances($accessKeyId, $accessKeySecret, $regionId = '')
    {
        if ($this->initError) {
            throw new Exception($this->initError);
        }

        if (empty($accessKeyId) || empty($accessKeySecret)) {
            throw new Exception('请先填写AK ID和AK Secret');
        }

        try {
            $instances = $this->aliyunService->getInstances($accessKeyId, $accessKeySecret, $regionId ?: null);
            $maskedKey = substr($accessKeyId, 0, 7) . '***';
            $this->db->addLog('info', "实例列表获取成功 [{$maskedKey}] 共 " . count($instances) . " 台");
            return $instances;
        } catch (ClientException $e) {
            $this->db->addLog('warning', "实例列表获取失败: 鉴权错误");
            throw new Exception('阿里云鉴权失败，请检查AK权限或密钥是否正确');
        } catch (ServerException $e) {
            $this->db->addLog('warning', "实例列表获取失败: " . $e->getErrorCode() . " - " . strip_tags($e->getErrorMessage()));
            throw new Exception('阿里云接口错误 [' . $e->getErrorCode() . ']: ' . $e->getErrorMessage());
        } catch (\Exception $e) {
            $this->db->addLog('warning', "实例列表获取失败: " . strip_tags($e->getMessage()));
            throw $e;
        }
    }

    public function testAccountCredentials($account)
    {
        if ($this->initError) {
            throw new Exception($this->initError);
        }

        $accessKeyId = trim((string) ($account['AccessKeyId'] ?? ''));
        $accessKeySecret = trim((string) ($account['AccessKeySecret'] ?? ''));
        $regionId = trim((string) ($account['regionId'] ?? ''));
        $maxTraffic = (float) ($account['maxTraffic'] ?? 0);
        $accountLabel = trim((string) ($account['remark'] ?? '')) ?: (substr($accessKeyId, 0, 7) . '***');

        if ($accessKeyId === '' || $accessKeySecret === '' || $regionId === '') {
            throw new Exception('请先填写完整的AK、区域和账号流量');
        }

        if ($accessKeySecret === '********') {
            $accessKeySecret = $this->resolveSecretFromDatabase($accessKeyId, $regionId);
        }

        try {
            $regions = $this->aliyunService->getRegions($accessKeyId, $accessKeySecret);
            $regionIds = array_column($regions, 'regionId');
            if (!in_array($regionId, $regionIds, true)) {
                throw new Exception('当前AK无法访问所选区域，请检查权限范围');
            }

            $instances = $this->aliyunService->getInstances($accessKeyId, $accessKeySecret);
            $regionInstances = array_values(array_filter($instances, function ($instance) use ($regionId) {
                return ($instance['regionId'] ?? '') === $regionId;
            }));
            $instanceCount = count($regionInstances);

            $monitorWarning = '';
            $monitorChecked = false;
            if (!empty($regionInstances)) {
                $probe = $regionInstances[0];
                $endMs = (int) (floor((time() - 90) / 60) * 60 * 1000);
                $startMs = max($endMs - (10 * 60 * 1000), 0);
                try {
                    $this->aliyunService->getInstanceOutboundTrafficDelta([
                        'access_key_id' => $accessKeyId,
                        'access_key_secret' => $accessKeySecret,
                        'instance_id' => $probe['instanceId'] ?? '',
                        'public_ip' => $probe['publicIp'] ?? ''
                    ], $startMs, $endMs);
                    $monitorChecked = true;
                } catch (\Exception $metricException) {
                    $monitorWarning = '云监控流量探测未通过：' . strip_tags($metricException->getMessage());
                    $this->db->addLog('warning', "账号云监控探测异常 [{$accountLabel}]: {$monitorWarning}");
                }
            }

            $trafficUsed = (float) ($account['usageUsed'] ?? 0);
            $trafficRemaining = max(round($maxTraffic - $trafficUsed, 2), 0);
            $trafficPercent = $maxTraffic > 0 ? min(round(($trafficUsed / $maxTraffic) * 100, 2), 100) : 0;
            $this->db->addLog('info', "账号测试成功 [{$accountLabel}] {$regionId} 实例 {$instanceCount} 台");
            $message = 'AK可用，ECS API已接通';
            if ($monitorWarning !== '') {
                $message .= '；' . $monitorWarning;
            } elseif ($monitorChecked) {
                $message .= '，云监控 接口 已接通';
            } else {
                $message .= '；当前区域暂无实例，未执行云监控流量探测';
            }

            return [
                'success' => true,
                'message' => $message,
                'monitorWarning' => $monitorWarning,
                'usageUsed' => $trafficUsed,
                'usageRemaining' => $trafficRemaining,
                'usagePercent' => $trafficPercent,
                'instanceCount' => $instanceCount
            ];
        } catch (ClientException $e) {
            $message = '鉴权失败，请检查AK ID和AK Secret是否正确，或确认是否具备ECS 权限';
            $this->db->addLog('warning', "账号测试失败: {$message}");
            throw new Exception($message);
        } catch (ServerException $e) {
            $message = '阿里云接口错误 [' . $e->getErrorCode() . ']: ' . $e->getErrorMessage();
            $this->db->addLog('warning', "账号测试失败: {$message}");
            throw new Exception($message);
        } catch (Exception $e) {
            $this->db->addLog('warning', "账号测试失败: " . strip_tags($e->getMessage()));
            throw $e;
        }
    }

    public function previewEcsCreate($data)
    {
        if ($this->initError) {
            throw new Exception($this->initError);
        }

        $groupKey = trim((string) ($data['accountGroupKey'] ?? ''));
        if ($groupKey === '') {
            throw new Exception('请选择用于创建 ECS 的账号');
        }

        $account = $this->resolveAccountGroupForCreate($groupKey, $data['regionId'] ?? '');
        $preview = $this->aliyunService->buildEcsCreatePreview($account, $data, $this->detectClientPublicIp());
        $previewId = 'preview_' . bin2hex(random_bytes(12));

        $this->db->addLog('info', "ECS 创建预检完成 [{$preview['account']['label']}] {$preview['regionId']} {$preview['instanceType']}");

        return [
            'success' => true,
            'previewId' => $previewId,
            'summary' => $preview,
            'pricing' => $preview['pricing'],
            'warnings' => $preview['warnings']
        ];
    }

    public function createEcsFromPreview($previewId, array $preview)
    {
        if ($this->initError) {
            throw new Exception($this->initError);
        }

        if (empty($preview['account']['groupKey'])) {
            throw new Exception('创建预检已失效，请重新预检');
        }

        $groupKey = $preview['account']['groupKey'];
        $account = $this->resolveAccountGroupForCreate($groupKey, $preview['regionId'] ?? '');
        $taskId = 'ecs_' . bin2hex(random_bytes(10));

        // 创建新 ECS 不应顺手拉起客户已有的停机实例。先把当前已停机实例视为“有意停机”，保活逻辑会跳过它们。
        $this->configManager->blockCurrentlyStoppedInstances();

        $this->db->createEcsCreateTask(
            $taskId,
            $previewId,
            $groupKey,
            $preview['regionId'],
            $preview['instanceType'],
            $preview
        );

        $progress = function ($step) use ($taskId) {
            $this->db->updateEcsCreateTask($taskId, ['step' => $step]);
        };

        try {
            $result = $this->aliyunService->createManagedEcsFromPreview($account, $preview, $progress);
            $this->db->updateEcsCreateTask($taskId, [
                'zone_id' => $preview['zoneId'] ?? '',
                'image_id' => $preview['imageId'] ?? '',
                'os_label' => $preview['osLabel'] ?? '',
                'instance_name' => $preview['instanceName'] ?? '',
                'vpc_id' => $result['vpcId'] ?? '',
                'vswitch_id' => $result['vswitchId'] ?? '',
                'security_group_id' => $result['securityGroupId'] ?? '',
                'internet_max_bandwidth_out' => $result['internetMaxBandwidthOut'] ?? 0,
                'system_disk_category' => $result['systemDiskCategory'] ?? '',
                'system_disk_size' => $result['systemDiskSize'] ?? 0,
                'instance_id' => $result['instanceId'] ?? '',
                'public_ip' => $result['publicIp'] ?? '',
                'login_user' => $result['loginUser'] ?? '',
                'login_password' => '',
                'status' => 'success',
                'step' => '创建完成'
            ]);

            $this->configManager->syncAccountGroups(true);
            $this->configManager->load();
            $this->syncDdnsForAccounts($this->configManager->getAccounts(), "ECS 创建后");
            $this->db->addLog('info', "一键创建 ECS成功 [{$this->getAccountLogLabel($account)}] {$result['instanceId']} {$preview['instanceType']} {$preview['regionId']} {$result['internetMaxBandwidthOut']}Mbps");
            $notifyResult = $this->notificationService->notifyEcsCreated($this->getAccountLogLabel($account), $result, $preview);
            $this->logNotificationResult($notifyResult, $this->getAccountLogLabel($account));

            return [
                'success' => true,
                'taskId' => $taskId,
                'data' => $result
            ];
        } catch (Exception $e) {
            $this->db->updateEcsCreateTask($taskId, [
                'status' => 'failed',
                'step' => '创建失败',
                'error_message' => strip_tags($e->getMessage())
            ]);
            $this->db->addLog('error', "一键创建 ECS 失败 [{$this->getAccountLogLabel($account)}]: " . strip_tags($e->getMessage()));
            throw $e;
        }
    }

    public function syncAccountGroup($groupKey)
    {
        if ($this->initError) {
            throw new Exception($this->initError);
        }

        $groupKey = trim((string) $groupKey);
        if ($groupKey === '') {
            throw new Exception('缺少账号组标识');
        }

        $groups = $this->configManager->getAccountGroups();
        $targetGroup = null;
        foreach ($groups as $group) {
            if (($group['groupKey'] ?? '') === $groupKey) {
                $targetGroup = $group;
                break;
            }
        }

        if (!$targetGroup) {
            throw new Exception('账号组不存在，请刷新页面后重试');
        }

        // syncAccountGroups reconciles the full configured set, so use all groups here
        // and filter refresh work to the clicked group afterwards.
        $accountsBeforeSync = $this->configManager->getAccounts();
        $this->configManager->syncAccountGroups(true);
        $this->configManager->load();

        $threshold = (int) ($this->configManager->get('traffic_threshold', 95) ?? 95);
        $userInterval = (int) ($this->configManager->get('api_interval', 600) ?? 600);
        $billingEnabled = $this->configManager->get('enable_billing', '0') === '1';
        $instanceCount = 0;

        foreach ($this->configManager->getAccounts() as $account) {
            $accountGroupKey = $account['group_key'] ?: substr(sha1($account['access_key_id'] . '|' . $account['region_id']), 0, 16);
            if ($accountGroupKey !== $groupKey || empty($account['instance_id'])) {
                continue;
            }

            $this->buildInstanceSnapshot($account, $threshold, $userInterval, $billingEnabled, true, true);
            $instanceCount++;
        }

        if ($billingEnabled) {
            $this->getAccountGroupBillingMetrics(true);
        }

        $this->configManager->load();
        $this->reconcileDdnsAfterAccountSync($accountsBeforeSync, $this->configManager->getAccounts(), '账号同步');
        $this->db->addLog('info', "账号同步完成 [{$targetGroup['remark']}] {$targetGroup['regionId']} 实例 {$instanceCount} 台");

        return [
            'success' => true,
            'message' => "已同步 {$instanceCount} 台实例，流量和消费情况已刷新",
            'instanceCount' => $instanceCount
        ];
    }

    public function getEcsCreateTask($taskId)
    {
        if ($this->initError) {
            return null;
        }

        return $this->db->getEcsCreateTask($taskId);
    }

    private function resolveAccountGroupForCreate($groupKey, $regionId = '')
    {
        $groups = $this->configManager->getAccountGroups();
        foreach ($groups as $group) {
            if (($group['groupKey'] ?? '') !== $groupKey) {
                continue;
            }

            $resolvedRegion = trim((string) $regionId) ?: ($group['regionId'] ?? '');
            return [
                'id' => 0,
                'access_key_id' => $group['AccessKeyId'],
                'access_key_secret' => $group['AccessKeySecret'],
                'region_id' => $resolvedRegion,
                'group_key' => $group['groupKey'],
                'remark' => $group['remark'] ?? '',
                'site_type' => $group['siteType'] ?? 'international',
                'max_traffic' => (float) ($group['maxTraffic'] ?? 200),
                'instance_id' => '',
                'instance_name' => ''
            ];
        }

        throw new Exception('未找到对应账号，请先在账号管理中保存账号');
    }

    private function detectClientPublicIp()
    {
        $candidates = [];
        foreach (['HTTP_CF_CONNECTING_IP', 'HTTP_X_REAL_IP', 'REMOTE_ADDR'] as $key) {
            if (!empty($_SERVER[$key])) {
                $candidates[] = trim((string) $_SERVER[$key]);
            }
        }

        if (!empty($_SERVER['HTTP_X_FORWARDED_FOR'])) {
            foreach (explode(',', $_SERVER['HTTP_X_FORWARDED_FOR']) as $item) {
                $candidates[] = trim($item);
            }
        }

        foreach ($candidates as $ip) {
            if (filter_var($ip, FILTER_VALIDATE_IP, FILTER_FLAG_IPV4 | FILTER_FLAG_NO_PRIV_RANGE | FILTER_FLAG_NO_RES_RANGE)) {
                return $ip;
            }
        }

        $context = stream_context_create(['http' => ['timeout' => 3]]);
        $externalIp = @file_get_contents('https://api.ipify.org', false, $context);
        $externalIp = trim((string) $externalIp);
        if (filter_var($externalIp, FILTER_VALIDATE_IP, FILTER_FLAG_IPV4 | FILTER_FLAG_NO_PRIV_RANGE | FILTER_FLAG_NO_RES_RANGE)) {
            return $externalIp;
        }

        return '';
    }

    public function sendTestEmail($to)
    {
        return $this->notificationService->sendTestEmail($to);
    }

    public function sendTestTelegram($data)
    {
        return $this->notificationService->sendTestTelegram($data);
    }

    public function sendTestWebhook($data)
    {
        return $this->notificationService->sendTestWebhook($data);
    }

    private function logNotificationResult($result, $key)
    {
        if ($result === true) {
            $this->db->addLog('info', "通知推送成功 [$key]");
        } elseif ($result !== false && $result !== true) {
            $this->db->addLog('warning', "通知推送异常/失败 [$key]: " . strip_tags($result));
        }
    }

    private function notifyStatusChangeIfNeeded($account, $fromStatus, $toStatus, $reason = '')
    {
        $fromStatus = (string) ($fromStatus ?: 'Unknown');
        $toStatus = (string) ($toStatus ?: 'Unknown');

        // 核心过滤：
        // 1. 状态未变则不通知
        // 2. 只通知进入稳定态（Running/Stopped）的变化
        // 3. 过滤瞬态跳转，例如从 Starting 到 Running 是预期行为，但如果是在同步中由于 API 抖动导致的跳变则需谨慎
        if ($fromStatus === $toStatus || !in_array($toStatus, ['Running', 'Stopped'], true)) {
            return;
        }

        // 初次发现 (Unknown) 不通知，避免重启程序时大量刷屏
        // ECS 创建完成后的首次状态同步不通过此逻辑通知（已有专门的 notifyEcsCreated）
        if ($fromStatus === 'Unknown' || $this->isRecentlyCreatedInstance($account)) {
            return;
        }

        // 避免从过渡态到其目标态的冗余通知
        // 例如：刚刚手动触发了 Start，状态变为了 Starting，然后 API 检测到 Running。
        // 这时通常用户已经在界面看到了，或者已有操作成功的提示，可根据需要决定是否通知。
        // 这里保留过渡态到稳定态的通知，但过滤从一个稳定态快速切换到另一个稳定态（如通过脚本极速重启）时的中间干扰。

        $accountLabel = $this->getAccountLogLabel($account);
        $result = $this->notificationService->notifyInstanceStatusChanged($accountLabel, $account, $fromStatus, $toStatus, $reason);
        $this->logNotificationResult($result, $accountLabel);
    }

    private function isRecentlyCreatedInstance(array $account)
    {
        $instanceId = trim((string) ($account['instance_id'] ?? ''));
        if ($instanceId === '') {
            return false;
        }

        try {
            $stmt = $this->db->getPdo()->prepare("
                SELECT updated_at
                FROM ecs_create_tasks
                WHERE instance_id = ?
                    AND status = 'success'
                ORDER BY updated_at DESC
                LIMIT 1
            ");
            $stmt->execute([$instanceId]);
            $updatedAt = (int) $stmt->fetchColumn();
            return $updatedAt > 0 && (time() - $updatedAt) < 900;
        } catch (Exception $e) {
            return false;
        }
    }

    private function isSameMonth($timestamp, $currentTime)
    {
        if (empty($timestamp)) {
            return false;
        }
        return date('Y-m', (int) $timestamp) === date('Y-m', (int) $currentTime);
    }

    private function safeGetTraffic($account)
    {
        try {
            return $this->getMeteredOutboundTraffic($account);
        } catch (ClientException $e) {
            $code = $e->getErrorCode();
            $this->db->addLog('error', "公网出口流量查询配置错误 [{$this->getAccountLogLabel($account)}]: " . ($code ?: "鉴权失败") . "，请确认AK拥有云监控流量查询权限");
            return -1;
        } catch (ServerException $e) {
            $this->db->addLog('error', "公网出口流量查询失败 [{$this->getAccountLogLabel($account)}]: " . $e->getErrorCode() . " - " . $e->getErrorMessage());
            return -1;
        } catch (\Exception $e) {
            if (strpos($e->getMessage(), 'cURL error') !== false) {
                $this->db->addLog('error', "公网出口流量查询失败 [{$this->getAccountLogLabel($account)}]: 网络连接超时");
            } else {
                $this->db->addLog('error', "公网出口流量查询失败 [{$this->getAccountLogLabel($account)}]: " . strip_tags($e->getMessage()));
            }
            return -1;
        }
    }

    private function getGroupTrafficUsed($account)
    {
        $pdo = $this->db->getPdo();
        $groupKey = trim((string) ($account['group_key'] ?? ''));
        $billingMonth = date('Y-m');

        if ($groupKey !== '') {
            $stmt = $pdo->prepare("SELECT COALESCE(SUM(traffic_used), 0) FROM accounts WHERE group_key = ? AND traffic_billing_month = ?");
            $stmt->execute([$groupKey, $billingMonth]);
            return (float) $stmt->fetchColumn();
        }

        $stmt = $pdo->prepare("SELECT COALESCE(SUM(traffic_used), 0) FROM accounts WHERE access_key_id = ? AND region_id = ? AND traffic_billing_month = ?");
        $stmt->execute([$account['access_key_id'] ?? '', $account['region_id'] ?? '', $billingMonth]);
        return (float) $stmt->fetchColumn();
    }

    private function getMeteredOutboundTraffic($account)
    {
        if (empty($account['id']) || empty($account['instance_id'])) {
            throw new Exception('缺少账号 ID 或 Instance ID，无法按实例统计公网出口流量');
        }

        $billingMonth = date('Y-m');
        $monthStartMs = strtotime($billingMonth . '-01 00:00:00') * 1000;
        $record = $this->db->getInstanceTrafficUsage($account['id'], $account['instance_id'], $billingMonth);

        $trafficBytes = $record ? (float) ($record['traffic_bytes'] ?? 0) : 0.0;
        $lastSampleMs = $record ? (int) ($record['last_sample_ms'] ?? 0) : 0;
        if ($lastSampleMs < $monthStartMs) {
            $lastSampleMs = $monthStartMs;
            $trafficBytes = 0.0;
        }

        // 云监控分钟点有轻微延迟，只同步到上一个完整分钟，避免把未收敛的数据点算进去。
        $safeEndSeconds = max(strtotime($billingMonth . '-01 00:00:00'), time() - 90);
        $endMs = (int) (floor($safeEndSeconds / 60) * 60 * 1000);

        if ($endMs > $lastSampleMs) {
            $delta = $this->aliyunService->getInstanceOutboundTrafficDelta($account, $lastSampleMs, $endMs);
            $trafficBytes += (float) ($delta['bytes'] ?? 0);
            $lastSampleMs = max($lastSampleMs, (int) ($delta['lastSampleMs'] ?? $lastSampleMs));
        }

        $this->db->upsertInstanceTrafficUsage(
            (int) $account['id'],
            $account['instance_id'],
            $billingMonth,
            $trafficBytes,
            $lastSampleMs
        );

        return $trafficBytes / 1024 / 1024 / 1024;
    }

    private function safeGetInstanceStatus($account)
    {
        try {
            return $this->aliyunService->getInstanceStatus($account);
        } catch (\Exception $e) {
            return 'Unknown';
        }
    }

    private function safeGetInstanceFullStatus($account)
    {
        try {
            return $this->aliyunService->getInstanceFullStatus($account);
        } catch (\Exception $e) {
            return null;
        }
    }

    private function safeControlInstance($account, $action, $shutdownMode = 'KeepCharging')
    {
        try {
            return $this->aliyunService->controlInstance($account, $action, $shutdownMode);
        } catch (ClientException $e) {
            $this->db->addLog('error', "实例操作失败 [{$action}]: 权限不足或配置错误");
            return false;
        } catch (ServerException $e) {
            $this->db->addLog('error', "实例操作失败 [{$action}]: " . $e->getErrorCode() . " - " . $e->getErrorMessage());
            return false;
        } catch (\Exception $e) {
            $this->db->addLog('error', "实例操作失败 [{$action}]: 无法连接接口");
            return false;
        }
    }

    private function getRegionName($regionId)
    {
        $regions = [
            'cn-hongkong' => '中国香港',
            'ap-southeast-1' => '新加坡',
            'us-west-1' => '美国(硅谷)',
            'us-east-1' => '美国(弗吉尼亚)',
            'cn-hangzhou' => '华东1(杭州)',
            'cn-shanghai' => '华东2(上海)',
            'cn-qingdao' => '华北1(青岛)',
            'cn-beijing' => '华北2(北京)',
            'cn-zhangjiakou' => '华北3(张家口)',
            'cn-huhehaote' => '华北5(呼和浩特)',
            'cn-wulanchabu' => '华北6(乌兰察布)',
            'cn-shenzhen' => '华南1(深圳)',
            'cn-heyuan' => '华南2(河源)',
            'cn-guangzhou' => '华南3(广州)',
            'cn-chengdu' => '西南1(成都)',
            'ap-northeast-1' => '日本(东京)',
        ];
        return $regions[$regionId] ?? $regionId;
    }

    // ==================== 费用分析 ====================

    /**
     * 安全获取账户费用摘要信息 (带缓存)
     * 用于实例卡片上显示
     */
    private function safeGetBillingInfo($account, $billingCycle)
    {
        $costInfo = [
            'enabled' => true,
            'monthly_cost' => null,
            'balance' => null,
            'currency' => 'CNY',
            'last_updated' => null,
            'error' => null
        ];

        // 1. 尝试读取余额缓存
        $balanceCache = $this->db->getBillingCache($account['id'], 'balance', '', 21600);
        if ($balanceCache) {
            $costInfo['balance'] = $balanceCache['AvailableAmount'];
            $costInfo['currency'] = $balanceCache['Currency'] ?? 'CNY';
        } else {
            try {
                $balance = $this->aliyunService->getAccountBalance(
                    $account['access_key_id'],
                    $account['access_key_secret'],
                    $account['site_type'] ?? 'china'
                );
                $costInfo['balance'] = $balance['AvailableAmount'];
                $costInfo['currency'] = $balance['Currency'] ?? 'CNY';
                $this->db->setBillingCache($account['id'], 'balance', '', $balance);
            } catch (\Exception $e) {
                $costInfo['error'] = '余额查询失败';
            }
        }

        // 2. 尝试读取实例账单缓存
        if (!empty($account['instance_id'])) {
            $billCache = $this->db->getBillingCache($account['id'], 'instance_bill', $billingCycle, 21600);
            if ($billCache) {
                $costInfo['monthly_cost'] = $billCache['TotalCost'];
            } else {
                try {
                    $bill = $this->aliyunService->getInstanceBill(
                        $account['access_key_id'],
                        $account['access_key_secret'],
                        $account['instance_id'],
                        $billingCycle,
                        $account['site_type'] ?? 'china'
                    );
                    $costInfo['monthly_cost'] = $bill['TotalCost'];
                    $this->db->setBillingCache($account['id'], 'instance_bill', $billingCycle, $bill);
                } catch (\Exception $e) {
                    if ($costInfo['error']) {
                        $costInfo['error'] = '费用中心权限不足';
                    } else {
                        $costInfo['error'] = '账单查询失败';
                    }
                }
            }
        }

        $costInfo['last_updated'] = date('Y-m-d H:i:s');
        return $costInfo;
    }

    private function getAccountGroupBillingMetrics($forceRefresh = false)
    {
        if ($this->configManager->get('enable_billing', '0') !== '1') {
            return [];
        }

        $billingCycle = date('Y-m');
        $groups = $this->configManager->getAccountGroups();
        $accounts = $this->configManager->getAccounts();
        $accountsByGroup = [];

        foreach ($accounts as $account) {
            $groupKey = $account['group_key'] ?: ($account['access_key_id'] . '@' . $account['region_id']);
            if (!isset($accountsByGroup[$groupKey])) {
                $accountsByGroup[$groupKey] = [];
            }
            $accountsByGroup[$groupKey][] = $account;
        }

        $metrics = [];

        foreach ($groups as $group) {
            $groupKey = $group['groupKey'] ?? '';
            $row = $accountsByGroup[$groupKey][0] ?? null;
            $currency = ($group['siteType'] ?? 'international') === 'international' ? 'USD' : 'CNY';
            $summary = [
                'enabled' => true,
                'monthly_cost' => null,
                'balance' => null,
                'currency' => $currency,
                'last_updated' => null,
                'error' => null
            ];

            if (!$row) {
                $summary['error'] = '尚未同步实例';
                $metrics[$groupKey] = $summary;
                continue;
            }

            try {
                $balanceCache = $forceRefresh ? null : $this->db->getBillingCache($row['id'], 'balance', '', 21600);
                if ($balanceCache) {
                    $summary['balance'] = $balanceCache['AvailableAmount'] ?? null;
                    $summary['currency'] = $balanceCache['Currency'] ?? $currency;
                } else {
                    $balance = $this->aliyunService->getAccountBalance(
                        $row['access_key_id'],
                        $row['access_key_secret'],
                        $row['site_type'] ?? ($group['siteType'] ?? 'international')
                    );
                    $summary['balance'] = $balance['AvailableAmount'] ?? null;
                    $summary['currency'] = $balance['Currency'] ?? $currency;
                    $this->db->setBillingCache($row['id'], 'balance', '', $balance);
                }
            } catch (\Exception $e) {
                $summary['error'] = '余额查询失败';
            }

            try {
                $overviewCache = $forceRefresh ? null : $this->db->getBillingCache($row['id'], 'bill_overview', $billingCycle, 21600);
                if ($overviewCache) {
                    $summary['monthly_cost'] = $overviewCache['TotalCost'] ?? null;
                } else {
                    $overview = $this->aliyunService->getBillOverview(
                        $row['access_key_id'],
                        $row['access_key_secret'],
                        $billingCycle,
                        $row['site_type'] ?? ($group['siteType'] ?? 'international')
                    );
                    $summary['monthly_cost'] = $overview['TotalCost'] ?? null;
                    $this->db->setBillingCache($row['id'], 'bill_overview', $billingCycle, $overview);
                }
            } catch (\Exception $e) {
                $summary['error'] = $summary['error'] ? '费用中心权限不足' : '账单查询失败';
            }

            $summary['last_updated'] = date('Y-m-d H:i:s');
            $metrics[$groupKey] = $summary;
        }

        return $metrics;
    }

    public function controlInstanceAction($accountId, $action, $shutdownMode = 'KeepCharging')
    {
        if ($this->initError)
            return false;

        $targetAccount = $this->configManager->getAccountById($accountId);
        if (!$targetAccount)
            return false;

        try {
            $result = $this->aliyunService->controlInstance($targetAccount, $action, $shutdownMode);
            if ($result) {
                $this->db->addLog('info', "实例操作 [{$action}] 成功 [{$this->getAccountLogLabel($targetAccount)}] {$targetAccount['instance_id']}");
                $newStatus = $action === 'stop' ? 'Stopping' : 'Starting';
                $this->configManager->updateAccountStatus($accountId, $targetAccount['traffic_used'], $newStatus, time());
                $this->configManager->updateAutoStartBlocked($accountId, $action === 'stop');
                if ($action === 'start') {
                    sleep(8);
                    $this->configManager->syncAccountGroups(true);
                    $this->configManager->load();
                    $syncedAccount = $this->configManager->getAccountById($accountId);
                    if (($syncedAccount['instance_status'] ?? '') === 'Running') {
                        $this->notifyStatusChangeIfNeeded($syncedAccount, $targetAccount['instance_status'] ?? 'Unknown', 'Running', '用户手动启动成功。');
                    }
                    $this->syncDdnsForAccounts($this->configManager->getAccounts(), '实例启动后');
                }
            }
            return true;
        } catch (ClientException $e) {
            $this->db->addLog('error', "实例操作失败 [{$action}]: 权限不足或配置错误");
            return false;
        } catch (ServerException $e) {
            $this->db->addLog('error', "实例操作失败 [{$action}]: " . $e->getErrorCode() . " - " . $e->getErrorMessage());
            return false;
        } catch (\Exception $e) {
            $this->db->addLog('error', "实例操作失败 [{$action}]: 无法连接接口");
            return false;
        }
    }

    public function deleteInstanceAction($accountId, $forceStop = false)
    {
        if ($this->initError)
            return false;

        $targetAccount = $this->configManager->getAccountById($accountId);
        if (!$targetAccount)
            return false;

        // 异步方案：仅标记为删除并记录日志
        $this->db->addLog('warning', "操作成功：秒级标记释放指令已提交，后台安全队列正在接管 [{$this->getAccountLogLabel($targetAccount)}] {$targetAccount['instance_id']}");
        $this->configManager->markAccountAsDeleted($accountId);

        return true;
    }

    private function processPendingReleases()
    {
        $pendingAccounts = $this->configManager->getPendingReleaseAccounts();
        foreach ($pendingAccounts as $account) {
            $accountLabel = $this->getAccountLogLabel($account);
            try {
                $status = $this->aliyunService->getInstanceStatus($account);
            } catch (\Exception $e) {
                if (stripos($e->getMessage(), 'NotFound') !== false || stripos($e->getMessage(), 'InvalidInstanceId') !== false) {
                    $status = 'NotFound';
                } else {
                    $this->db->addLog('error', "后台异步释放引擎探测异常 [{$accountLabel}]: " . $e->getMessage());
                    continue;
                }
            }

            try {
                if ($status === 'Stopped') {
                    $result = $this->aliyunService->deleteInstance($account, false);
                    if ($result) {
                        $this->db->addLog('warning', "后台异步彻底销毁成功 [{$accountLabel}] {$account['instance_id']}");
                        $releaseNotifyResult = $this->notificationService->notifyInstanceReleased(
                            $accountLabel,
                            $account,
                            '用户前端提交指令后，后台成功执行安全彻底销毁。'
                        );
                        $this->logNotificationResult($releaseNotifyResult, $accountLabel);
                        
                        $accountsBeforeDelete = $this->configManager->getAccounts();
                        $this->deleteDdnsForAccount($account, $accountsBeforeDelete, '后台实例彻底释放');
                        $this->configManager->physicallyDeleteAccount($account['id']);
                        $this->reconcileDdnsAfterAccountSync($accountsBeforeDelete, $this->configManager->getAccounts(), '异步释放后同步');
                    }
                } elseif ($status === 'NotFound' || $status === 'Unknown') {
                    $this->db->addLog('warning', "待释放实例云端已灭迹，自动擦除本地账本 [{$accountLabel}]");
                    $this->configManager->physicallyDeleteAccount($account['id']);
                } elseif (!in_array($status, ['Stopping'])) {
                    $this->db->addLog('info', "后台异步释放引擎：向活跃实例下发强制离线指令 [{$accountLabel}]");
                    // 仅调用 stop 并允许返回，不产生同步堵塞死循环
                    $this->aliyunService->controlInstance($account, 'stop'); 
                }
            } catch (\Exception $e) {
                // 如果 DeleteInstance 等遇到暂时性 API 禁止，让它下一分钟随 Cron 重新再轮询一次，不需要人工介入
                $this->db->addLog('error', "后台异步释放行动异常，将于下一分钟轮询重试 [{$accountLabel}]: " . $e->getMessage());
            }
        }
    }

    /**
     * 获取所有已配置账号的实例列表（合并去重）
     */
    public function getAllManagedInstances($sync = false)
    {
        if ($this->initError)
            return [];

        if ($sync) {
            $accountsBeforeSync = $this->configManager->getAccounts();
            $this->configManager->syncAccountGroups(true);
            $this->configManager->load();
            $this->reconcileDdnsAfterAccountSync($accountsBeforeSync, $this->configManager->getAccounts(), '实例手动同步');
        } else {
            $this->configManager->load();
        }

        $threshold = (int) ($this->configManager->get('traffic_threshold', 95) ?? 95);
        $userInterval = (int) ($this->configManager->get('api_interval', 600) ?? 600);
        $accounts = array_values(array_filter($this->configManager->getAccounts(), function ($account) {
            return !empty($account['instance_id']);
        }));
        $allInstances = [];

        foreach ($accounts as $account) {
            $allInstances[] = $this->buildInstanceSnapshot($account, $threshold, $userInterval, false, true, $sync);
        }

        $pendingAccounts = $this->configManager->getPendingReleaseAccounts();
        foreach ($pendingAccounts as $account) {
            $snap = $this->buildInstanceSnapshot($account, $threshold, $userInterval, false, true, $sync);
            $snap['instanceStatus'] = 'Releasing';
            $snap['status'] = 'Releasing';
            $allInstances[] = $snap;
        }

        return $allInstances;
    }

    private function syncDdnsForAccounts(array $accounts, $source = '同步')
    {
        if (!$this->ddnsService || !$this->ddnsService->isEnabled()) {
            return;
        }

        $groupCounts = $this->getDdnsGroupCounts($accounts);

        foreach ($accounts as $account) {
            if (empty($account['instance_id']) || empty($account['public_ip'])) {
                continue;
            }

            try {
                $recordName = $this->buildDdnsRecordNameForAccount($account, $groupCounts);

                $result = $this->ddnsService->syncARecord($recordName, $account['public_ip']);
                if (!empty($result['success']) && empty($result['skipped'])) {
                    $this->db->addLog('info', "DDNS 已同步 [{$this->getAccountLogLabel($account)}] {$recordName} -> {$account['public_ip']} ({$source})");
                } elseif (empty($result['success'])) {
                    $this->db->addLog('warning', "DDNS 同步失败 [{$this->getAccountLogLabel($account)}]: " . strip_tags($result['message'] ?? '未知错误'));
                }
            } catch (Exception $e) {
                $this->db->addLog('warning', "DDNS 同步失败 [{$this->getAccountLogLabel($account)}]: " . strip_tags($e->getMessage()));
            }
        }
    }

    private function reconcileDdnsAfterAccountSync(array $beforeAccounts, array $afterAccounts, $source = '同步')
    {
        if (!$this->ddnsService || !$this->ddnsService->isEnabled()) {
            return;
        }

        $beforeRecords = $this->getDdnsRecordNamesForAccounts($beforeAccounts);
        $afterRecords = $this->getDdnsRecordNamesForAccounts($afterAccounts);

        foreach ($beforeRecords as $instanceId => $recordName) {
            if ($recordName === '' || in_array($recordName, $afterRecords, true)) {
                continue;
            }
            $this->deleteDdnsRecord($recordName, $source . '清理');
        }

        $this->syncDdnsForAccounts($afterAccounts, $source);
    }

    private function deleteDdnsForAccount(array $account, array $accountsBeforeDelete, $source = '释放')
    {
        if (!$this->ddnsService || !$this->ddnsService->isEnabled()) {
            return;
        }

        try {
            $recordName = $this->buildDdnsRecordNameForAccount($account, $this->getDdnsGroupCounts($accountsBeforeDelete));
            $this->deleteDdnsRecord($recordName, $source);
        } catch (Exception $e) {
            $this->db->addLog('warning', "DDNS 清理失败 [{$this->getAccountLogLabel($account)}]: " . strip_tags($e->getMessage()));
        }
    }

    private function deleteDdnsRecord($recordName, $source = '清理')
    {
        try {
            $result = $this->ddnsService->deleteARecord($recordName);
            if (!empty($result['success']) && empty($result['skipped'])) {
                $this->db->addLog('info', "DDNS 已删除 {$recordName} ({$source})");
            } elseif (empty($result['success'])) {
                $this->db->addLog('warning', "DDNS 删除失败 {$recordName}: " . strip_tags($result['message'] ?? '未知错误'));
            }
        } catch (Exception $e) {
            $this->db->addLog('warning', "DDNS 删除失败 {$recordName}: " . strip_tags($e->getMessage()));
        }
    }

    private function getDdnsRecordNamesForAccounts(array $accounts)
    {
        $groupCounts = $this->getDdnsGroupCounts($accounts);
        $records = [];

        foreach ($accounts as $account) {
            if (empty($account['instance_id'])) {
                continue;
            }

            try {
                $records[$account['instance_id']] = $this->buildDdnsRecordNameForAccount($account, $groupCounts);
            } catch (Exception $e) {
                $this->db->addLog('warning', "DDNS 记录名生成失败 [{$this->getAccountLogLabel($account)}]: " . strip_tags($e->getMessage()));
            }
        }

        return $records;
    }

    private function buildDdnsRecordNameForAccount(array $account, array $groupCounts)
    {
        $groupKey = $this->getDdnsGroupKey($account);
        return $this->ddnsService->buildRecordName([
            'account_remark' => $this->resolveGroupRemark($account),
            'remark' => $account['remark'] ?? '',
            'instance_name' => $account['instance_name'] ?? '',
            'instance_id' => $account['instance_id'] ?? ''
        ], $groupCounts[$groupKey] ?? 1);
    }

    private function getDdnsGroupCounts(array $accounts)
    {
        $groupCounts = [];

        foreach ($accounts as $account) {
            if (empty($account['instance_id'])) {
                continue;
            }
            $groupKey = $this->getDdnsGroupKey($account);
            $groupCounts[$groupKey] = ($groupCounts[$groupKey] ?? 0) + 1;
        }

        return $groupCounts;
    }

    private function getDdnsGroupKey(array $account)
    {
        return $account['group_key'] ?: (($account['access_key_id'] ?? '') . '|' . ($account['region_id'] ?? ''));
    }

    private function resolveGroupRemark(array $account)
    {
        $groupKey = trim((string) ($account['group_key'] ?? ''));
        if ($groupKey !== '') {
            foreach ($this->configManager->getAccountGroups() as $group) {
                if (($group['groupKey'] ?? '') === $groupKey) {
                    return trim((string) ($group['remark'] ?? ''));
                }
            }
        }

        return trim((string) ($account['remark'] ?? ''));
    }

    private function buildInstanceSnapshot($account, $threshold, $userInterval, $billingEnabled, $includeSensitive = true, $forceRefresh = false)
    {
        $currentTime = time();
        $lastUpdate = (int) ($account['updated_at'] ?? 0);
        $cachedStatus = $account['instance_status'] ?? 'Unknown';
        $newUpdateTime = $currentTime;

        $isTransientState = in_array($cachedStatus, ['Starting', 'Stopping', 'Pending', 'Unknown'], true);
        $checkInterval = $isTransientState ? 60 : $userInterval;

        if ($forceRefresh || ($currentTime - $lastUpdate) > $checkInterval) {
            $newTraffic = $this->safeGetTraffic($account);
            $status = $this->safeGetInstanceStatus($account);

            if ($status === 'Unknown') {
                $status = $cachedStatus;
            }

            if ($newTraffic < 0) {
                $traffic = (float) ($account['traffic_used'] ?? 0);
                $newUpdateTime = $lastUpdate;
            } else {
                $traffic = $newTraffic;
                $this->db->addHourlyStat($account['id'], $traffic);
                $this->db->addDailyStat($account['id'], $traffic);
            }

            if ($newUpdateTime <= 0) {
                $newUpdateTime = $currentTime;
            }

            $this->notifyStatusChangeIfNeeded($account, $cachedStatus, $status, '页面刷新检测到实例状态变化。');

            $metadata = [];
            // 如果处于运行中且健康状态未知或非 OK，尝试获取详细状态以识别“操作系统启动中”
            if ($status === 'Running' && ($account['health_status'] ?? '') !== 'OK') {
                $full = $this->safeGetInstanceFullStatus($account);
                if ($full) {
                    $metadata['health_status'] = $full['healthStatus'];
                }
            }

            $this->configManager->updateAccountStatus($account['id'], $traffic, $status, $newUpdateTime, $metadata);
            $lastUpdate = $newUpdateTime;
        } else {
            $traffic = (float) ($account['traffic_used'] ?? 0);
            $status = $cachedStatus;
        }

        $maxTraffic = (float) ($account['max_traffic'] ?? 0);
        $usagePercent = $maxTraffic > 0 ? round(($traffic / $maxTraffic) * 100, 2) : 0;
        $instanceName = $account['instance_name'] ?? '';
        $remark = $account['remark'] ?? '';

        $accountDisplayLabel = $this->getAccountLogLabel($account);

        $item = [
            'id' => (int) $account['id'],
            'accountId' => (int) $account['id'],
            'groupKey' => $account['group_key'] ?? '',
            'account' => substr($account['access_key_id'], 0, 7) . '***',
            'accountMasked' => substr($account['access_key_id'], 0, 7) . '***',
            'accountLabel' => $accountDisplayLabel . ' / ' . $this->getRegionName($account['region_id']),
            'flow_total' => $maxTraffic,
                'flow_used' => round($traffic, 6),
            'percentageOfUse' => $usagePercent,
            'region' => $account['region_id'],
            'regionId' => $account['region_id'],
            'regionName' => $this->getRegionName($account['region_id']),
            'rate95' => $usagePercent >= $threshold,
            'threshold' => $threshold,
            'instanceStatus' => $status,
            'status' => $status,
            'healthStatus' => $account['health_status'] ?? 'Unknown',
            'stoppedMode' => $account['stopped_mode'] ?? 'KeepCharging',
            'cpu' => (int) ($account['cpu'] ?? 0),
            'memory' => (int) ($account['memory'] ?? 0),
            'lastUpdated' => date('Y-m-d H:i:s', $lastUpdate > 0 ? $lastUpdate : $currentTime),
            'remark' => $remark !== '' ? $remark : ($instanceName !== '' ? $instanceName : ($account['instance_id'] ?? '')),
            'instanceId' => $account['instance_id'] ?? '',
            'instanceName' => $instanceName,
            'instanceType' => $account['instance_type'] ?? '',
            'osName' => $account['os_name'] ?? '',
            'internetMaxBandwidthOut' => (int) ($account['internet_max_bandwidth_out'] ?? 0),
            'publicIp' => $includeSensitive ? ($account['public_ip'] ?? '') : '',
            'privateIp' => $includeSensitive ? ($account['private_ip'] ?? '') : '',
            'maxTraffic' => $maxTraffic,
            'siteType' => $account['site_type'] ?? 'international'
        ];

        if ($billingEnabled) {
            $item['cost'] = $this->safeGetBillingInfo($account, date('Y-m'));
        }

        return $item;
    }

    public function renderTemplate()
    {
        if (!file_exists('template.html'))
            return "File not found";
        ob_start();
        include 'template.html';
        return ob_get_clean();
    }
}
