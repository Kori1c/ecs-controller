<?php

class ConfigManager
{
    private $database;
    private $db;
    private $configCache = [];
    private $accountsCache = [];
    private $encryptionKey = null;

    public function __construct(Database $db)
    {
        $this->database = $db;
        $this->db = $db->getPdo();
        $this->encryptionKey = $this->getEncryptionKey();
        $this->load();
    }

    private function getEncryptionKey()
    {
        $keyDir = __DIR__ . '/data';
        $keyFile = $keyDir . '/.secret_encryption.key';

        if (!is_dir($keyDir)) {
            @mkdir($keyDir, 0755, true);
        }

        if (@file_exists($keyFile)) {
            $key = @file_get_contents($keyFile);
            if ($key !== false && strlen($key) === SODIUM_CRYPTO_SECRETBOX_KEYBYTES) {
                return $key;
            }
        }

        $key = random_bytes(SODIUM_CRYPTO_SECRETBOX_KEYBYTES);
        @file_put_contents($keyFile, $key, LOCK_EX);
        @chmod($keyFile, 0600);
        return $key;
    }

    private function encryptValue($value)
    {
        if (!function_exists('sodium_crypto_secretbox') || empty($value)) {
            return $value;
        }
        $nonce = random_bytes(SODIUM_CRYPTO_SECRETBOX_NONCEBYTES);
        $encrypted = sodium_crypto_secretbox($value, $nonce, $this->encryptionKey);
        return 'ENC1' . base64_encode($nonce . $encrypted);
    }

    private function decryptValue($value)
    {
        if (!function_exists('sodium_crypto_secretbox') || empty($value) || strlen($value) < 8 || substr($value, 0, 4) !== 'ENC1') {
            return $value;
        }
        $raw = base64_decode(substr($value, 4));
        if ($raw === false) {
            return $value;
        }
        $nonce = substr($raw, 0, SODIUM_CRYPTO_SECRETBOX_NONCEBYTES);
        $ciphertext = substr($raw, SODIUM_CRYPTO_SECRETBOX_NONCEBYTES);
        $decrypted = sodium_crypto_secretbox_open($ciphertext, $nonce, $this->encryptionKey);
        return $decrypted !== false ? $decrypted : $value;
    }

    private function isEncryptedValue($value)
    {
        return strlen($value) >= 8 && substr($value, 0, 4) === 'ENC1';
    }

    public function load()
    {
        $this->configCache = [];
        $stmt = $this->db->query("SELECT key, value FROM settings");
        while ($row = $stmt->fetch()) {
            $this->configCache[$row['key']] = $row['value'];
        }

        $this->resetMonthlyTrafficCacheIfNeeded();

        $stmt = $this->db->query("SELECT * FROM accounts WHERE is_deleted = 0 ORDER BY region_id ASC, remark ASC, id ASC");
        $this->accountsCache = $stmt->fetchAll();

        foreach ($this->accountsCache as &$row) {
            if (!empty($row['access_key_secret']) && $this->isEncryptedValue($row['access_key_secret'])) {
                $row['access_key_secret'] = $this->decryptValue($row['access_key_secret']);
            }
        }
        unset($row);
    }

    private function resetMonthlyTrafficCacheIfNeeded()
    {
        $currentMonth = date('Y-m');

        // 首次升级时，已有缓存默认视为当前月，避免上线当天误清空。
        $stmt = $this->db->prepare("UPDATE accounts SET traffic_billing_month = ? WHERE traffic_billing_month IS NULL OR traffic_billing_month = ''");
        $stmt->execute([$currentMonth]);

        // 阿里云 CDT/公网流量按自然月结算。月切换后，展示值和熔断判断都必须从当月重新开始。
        $stmt = $this->db->prepare("
            UPDATE accounts
            SET traffic_used = 0,
                traffic_billing_month = ?,
                updated_at = 0
            WHERE traffic_billing_month <> ?
        ");
        $stmt->execute([$currentMonth, $currentMonth]);
    }

    public function get($key, $default = null)
    {
        return $this->configCache[$key] ?? $default;
    }

    public function getAllSettings()
    {
        return $this->configCache;
    }

    public function getAccounts()
    {
        return $this->accountsCache;
    }

    public function getAccountById($id)
    {
        foreach ($this->accountsCache as $acc) {
            if ((int) $acc['id'] === (int) $id) {
                return $acc;
            }
        }
        return null;
    }

    public function getAccountByInstanceId($instanceId)
    {
        foreach ($this->accountsCache as $acc) {
            if (($acc['instance_id'] ?? '') === $instanceId) {
                return $acc;
            }
        }
        return null;
    }

    public function decryptAccountSecret($secretFromDb)
    {
        if (empty($secretFromDb)) {
            return '';
        }
        return $this->decryptValue($secretFromDb);
    }

    public function getAccountGroups()
    {
        $raw = $this->configCache['account_groups'] ?? '';
        if (!empty($raw)) {
            $decoded = json_decode($raw, true);
            if (is_array($decoded)) {
                return $this->normalizeAccountGroups($decoded, true);
            }
        }

        return $this->deriveAccountGroupsFromAccounts();
    }

    public function getAccountGroupMetrics()
    {
        $groups = $this->getAccountGroups();
        $metrics = [];

        foreach ($groups as $group) {
            $groupKey = $group['groupKey'];
            $maxTraffic = (float) ($group['maxTraffic'] ?? 0);
            $metrics[$groupKey] = [
                'usageUsed' => 0.0,
                'usageRemaining' => $maxTraffic,
                'usagePercent' => 0.0,
                'instanceCount' => 0,
                'lastUpdated' => 0
            ];
        }

        foreach ($this->accountsCache as $row) {
            $groupKey = $row['group_key'] ?: $this->buildGroupKey($row['access_key_id'], $row['region_id']);
            if (!isset($metrics[$groupKey])) {
                $maxTraffic = (float) ($row['max_traffic'] ?? 0);
                $metrics[$groupKey] = [
                    'usageUsed' => 0.0,
                    'usageRemaining' => $maxTraffic,
                    'usagePercent' => 0.0,
                    'instanceCount' => 0,
                    'lastUpdated' => 0
                ];
            }

            if (!empty($row['instance_id'])) {
                $metrics[$groupKey]['instanceCount']++;
            }

            $isCurrentMonthTraffic = ($row['traffic_billing_month'] ?? '') === date('Y-m');
            $metrics[$groupKey]['usageUsed'] += $isCurrentMonthTraffic ? (float) ($row['traffic_used'] ?? 0) : 0.0;
            $metrics[$groupKey]['lastUpdated'] = max($metrics[$groupKey]['lastUpdated'], (int) ($row['updated_at'] ?? 0));
        }

        foreach ($groups as $group) {
            $groupKey = $group['groupKey'];
            $maxTraffic = (float) ($group['maxTraffic'] ?? 0);
            $used = (float) ($metrics[$groupKey]['usageUsed'] ?? 0);
            $metrics[$groupKey]['usageRemaining'] = max($maxTraffic - $used, 0);
            $metrics[$groupKey]['usagePercent'] = $maxTraffic > 0 ? min(round(($used / $maxTraffic) * 100, 2), 100) : 0;
        }

        return $metrics;
    }

    public function isInitialized()
    {
        return !empty($this->configCache['admin_password']);
    }

    private function saveSetting($key, $value)
    {
        $stmt = $this->db->prepare("INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)");
        $stmt->execute([$key, $value]);
        $this->configCache[$key] = $value;
    }

    public function upgradePasswordHash($plainPassword)
    {
        $hashed = password_hash($plainPassword, PASSWORD_BCRYPT);
        $this->saveSetting('admin_password', $hashed);
    }

    public function getMonitorKey()
    {
        return $this->get('monitor_key', '');
    }

    public function saveMonitorKey($key)
    {
        $this->saveSetting('monitor_key', $key);
    }

    public function updateLastRunTime($time)
    {
        $this->saveSetting('last_monitor_run', $time);
    }

    public function getLastRunTime()
    {
        return (int) ($this->configCache['last_monitor_run'] ?? 0);
    }

    public function updateLastInstanceSyncTime($time)
    {
        $this->saveSetting('last_instance_sync', $time);
    }

    public function getLastInstanceSyncTime()
    {
        return (int) ($this->configCache['last_instance_sync'] ?? 0);
    }

    public function updateConfig($data)
    {
        try {
            $this->db->beginTransaction();

            $adminPassword = $data['admin_password'] ?? '';
            if (!empty($adminPassword) && $adminPassword !== '********') {
                if (!preg_match('/^\$2[aby]?\$/', $adminPassword) && !preg_match('/^\$argon2[aid]\$/', $adminPassword)) {
                    $adminPassword = password_hash($adminPassword, PASSWORD_BCRYPT);
                }
                $this->saveSetting('admin_password', $adminPassword);
            }
            $this->saveSetting('traffic_threshold', $data['traffic_threshold'] ?? 95);
            $this->saveSetting('shutdown_mode', $data['shutdown_mode'] ?? 'KeepCharging');
            $this->saveSetting('threshold_action', $data['threshold_action'] ?? 'stop_and_notify');
            $this->saveSetting('keep_alive', !empty($data['keep_alive']) ? '1' : '0');
            $this->saveSetting('monthly_auto_start', !empty($data['monthly_auto_start']) ? '1' : '0');
            $this->saveSetting('api_interval', $data['api_interval'] ?? 600);
            $this->saveSetting('enable_billing', !empty($data['enable_billing']) ? '1' : '0');
            $appBrand = is_array($data['AppBrand'] ?? null) ? $data['AppBrand'] : [];
            $this->saveSetting('app_logo_url', trim((string) ($appBrand['logo_url'] ?? '')));
            $this->saveDdnsSettings($data['Ddns'] ?? []);

            if (isset($data['Notification'])) {
                $this->saveNotificationSettings($data['Notification']);
            }

            $groups = $this->normalizeAccountGroups($data['Accounts'] ?? []);
            $this->saveSetting('account_groups', json_encode($groups, JSON_UNESCAPED_UNICODE));
            $this->syncAccountGroups(true, $groups);

            $this->db->commit();
            $this->load();
            return true;
        } catch (Exception $e) {
            if ($this->db->inTransaction()) {
                $this->db->rollBack();
            }
            $this->database->addLog('error', "配置保存失败: " . strip_tags($e->getMessage()));
            return false;
        }
    }

    public function syncAccountGroups($force = false, $groups = null)
    {
        $lastSync = $this->getLastInstanceSyncTime();
        if (!$force && (time() - $lastSync) < 60) {
            $this->load();
            return;
        }

        $groups = $groups === null ? $this->getAccountGroups() : $this->normalizeAccountGroups($groups, true);

        $existingRows = $this->db->query("SELECT * FROM accounts ORDER BY id ASC")->fetchAll(PDO::FETCH_ASSOC);
        $existingByGroup = [];
        $existingByComposite = [];

        foreach ($existingRows as $row) {
            $groupKey = $row['group_key'] ?: $this->buildGroupKey($row['access_key_id'], $row['region_id']);
            $existingByGroup[$groupKey][] = $row;
            $existingByComposite[$groupKey . '|' . $row['instance_id']] = $row;
        }

        $configuredGroupKeys = [];

        $insertStmt = $this->db->prepare("
            INSERT INTO accounts (
                access_key_id,
                access_key_secret,
                region_id,
                instance_id,
                max_traffic,
                schedule_enabled,
                start_time,
                stop_time,
                traffic_used,
                traffic_billing_month,
                instance_status,
                updated_at,
                last_keep_alive_at,
                remark,
                site_type,
                group_key,
                instance_name,
                instance_type,
                internet_max_bandwidth_out,
                public_ip,
                public_ip_mode,
                eip_allocation_id,
                eip_address,
                eip_managed,
                private_ip,
                cpu,
                memory,
                os_name,
                stopped_mode
            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, 0, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ");

        $updateStmt = $this->db->prepare("
            UPDATE accounts
            SET access_key_id = ?,
                access_key_secret = ?,
                region_id = ?,
                instance_id = ?,
                max_traffic = ?,
                schedule_enabled = ?,
                start_time = ?,
                stop_time = ?,
                instance_status = ?,
                remark = ?,
                site_type = ?,
                group_key = ?,
                instance_name = ?,
                instance_type = ?,
                internet_max_bandwidth_out = ?,
                public_ip = ?,
                public_ip_mode = ?,
                eip_allocation_id = ?,
                eip_address = ?,
                eip_managed = ?,
                private_ip = ?,
                cpu = ?,
                memory = ?,
                os_name = ?,
                stopped_mode = ?
            WHERE id = ?
        ");

        foreach ($groups as $group) {
            $configuredGroupKeys[] = $group['groupKey'];

            try {
                $service = new AliyunService();
                $instances = $service->getInstances($group['AccessKeyId'], $group['AccessKeySecret'], $group['regionId']);
            } catch (Exception $e) {
                $maskedKey = substr($group['AccessKeyId'], 0, 7) . '***';
                $this->database->addLog('warning', "实例同步失败 [{$maskedKey}] {$group['regionId']}: " . strip_tags($e->getMessage()));
                $this->updateGroupBaseSettings($group['groupKey'], $group);
                continue;
            }

            $remoteInstanceIds = [];

            foreach ($instances as $instance) {
                $remoteInstanceIds[] = $instance['instanceId'];
                $compositeKey = $group['groupKey'] . '|' . $instance['instanceId'];
                $existingRow = $existingByComposite[$compositeKey] ?? null;
                $remark = $this->resolveRemark($group, $instance, $existingRow);
                $networkMeta = $this->resolveNetworkMetadata($instance, $existingRow);

                if ($existingRow) {
                    $updateStmt->execute([
                        $group['AccessKeyId'],
                        $this->encryptValue($group['AccessKeySecret']),
                        $group['regionId'],
                        $instance['instanceId'],
                        $group['maxTraffic'],
                        0,
                        '',
                        '',
                        $instance['status'] ?: ($existingRow['instance_status'] ?? 'Unknown'),
                        $remark,
                        $group['siteType'],
                        $group['groupKey'],
                        $instance['instanceName'] ?? '',
                        $instance['instanceType'] ?? '',
                        (int) ($instance['internetMaxBandwidthOut'] ?? 0),
                        $instance['publicIp'] ?? '',
                        $networkMeta['public_ip_mode'],
                        $networkMeta['eip_allocation_id'],
                        $networkMeta['eip_address'],
                        $networkMeta['eip_managed'],
                        $instance['privateIp'] ?? '',
                        (int) ($instance['cpu'] ?? 0),
                        (int) ($instance['memory'] ?? 0),
                        $instance['osName'] ?? '',
                        $instance['stoppedMode'] ?? '',
                        $existingRow['id']
                    ]);
                } else {
                    $insertStmt->execute([
                        $group['AccessKeyId'],
                        $this->encryptValue($group['AccessKeySecret']),
                        $group['regionId'],
                        $instance['instanceId'],
                        $group['maxTraffic'],
                        0,
                        '',
                        '',
                        date('Y-m'),
                        $instance['status'] ?? 'Unknown',
                        $remark,
                        $group['siteType'],
                        $group['groupKey'],
                        $instance['instanceName'] ?? '',
                        $instance['instanceType'] ?? '',
                        (int) ($instance['internetMaxBandwidthOut'] ?? 0),
                        $instance['publicIp'] ?? '',
                        $networkMeta['public_ip_mode'],
                        $networkMeta['eip_allocation_id'],
                        $networkMeta['eip_address'],
                        $networkMeta['eip_managed'],
                        $instance['privateIp'] ?? '',
                        (int) ($instance['cpu'] ?? 0),
                        (int) ($instance['memory'] ?? 0),
                        $instance['osName'] ?? '',
                        $instance['stoppedMode'] ?? ''
                    ]);
                }
            }

            if (!empty($existingByGroup[$group['groupKey']])) {
                foreach ($existingByGroup[$group['groupKey']] as $row) {
                    if (!in_array($row['instance_id'], $remoteInstanceIds, true)) {
                        $deleteStmt = $this->db->prepare("DELETE FROM accounts WHERE id = ?");
                        $deleteStmt->execute([$row['id']]);
                    }
                }
            }
        }

        if (!empty($existingRows)) {
            foreach ($existingRows as $row) {
                $groupKey = $row['group_key'] ?: $this->buildGroupKey($row['access_key_id'], $row['region_id']);
                if (!in_array($groupKey, $configuredGroupKeys, true)) {
                    $deleteStmt = $this->db->prepare("DELETE FROM accounts WHERE id = ?");
                    $deleteStmt->execute([$row['id']]);
                }
            }
        }

        $this->updateLastInstanceSyncTime(time());
        $this->load();
    }

    public function updateAppLogoUrl($url)
    {
        $this->saveSetting('app_logo_url', trim((string) $url));
        $this->load();
    }

    private function saveNotificationSettings($notification)
    {
        $this->saveSetting('notify_email_enabled', !empty($notification['email_enabled']) ? '1' : '0');
        $this->saveSetting('notify_email', $notification['email'] ?? '');
        $this->saveSetting('notify_host', $notification['host'] ?? '');
        $this->saveSetting('notify_port', $notification['port'] ?? 465);
        $this->saveSetting('notify_username', $notification['username'] ?? '');

        if (isset($notification['password']) && $notification['password'] !== '********') {
            $this->saveSetting('notify_password', $notification['password'] ?? '');
        }

        $this->saveSetting('notify_secure', $notification['secure'] ?? 'ssl');

        $telegram = $notification['telegram'] ?? [];
        $this->saveSetting('notify_tg_enabled', !empty($telegram['enabled']) ? '1' : '0');

        if (isset($telegram['token']) && $telegram['token'] !== '********') {
            $this->saveSetting('notify_tg_token', $telegram['token'] ?? '');
        }

        $this->saveSetting('notify_tg_chat_id', $telegram['chat_id'] ?? '');
        $this->saveSetting('notify_tg_proxy_type', $telegram['proxy_type'] ?? 'none');
        $this->saveSetting('notify_tg_proxy_url', $telegram['proxy_url'] ?? '');
        $this->saveSetting('notify_tg_proxy_ip', $telegram['proxy_ip'] ?? '');
        $this->saveSetting('notify_tg_proxy_port', $telegram['proxy_port'] ?? '');
        $this->saveSetting('notify_tg_proxy_user', $telegram['proxy_user'] ?? '');

        if (isset($telegram['proxy_pass']) && $telegram['proxy_pass'] !== '********') {
            $this->saveSetting('notify_tg_proxy_pass', $telegram['proxy_pass'] ?? '');
        }

        $webhook = $notification['webhook'] ?? [];
        $this->saveSetting('notify_wh_enabled', !empty($webhook['enabled']) ? '1' : '0');
        $this->saveSetting('notify_wh_url', $webhook['url'] ?? '');
        $this->saveSetting('notify_wh_method', $webhook['method'] ?? 'GET');
        $this->saveSetting('notify_wh_request_type', $webhook['request_type'] ?? 'JSON');
        $this->saveSetting('notify_wh_headers', $webhook['headers'] ?? '');
        $this->saveSetting('notify_wh_body', $webhook['body'] ?? '');
    }

    private function saveDdnsSettings($ddns)
    {
        $cloudflare = is_array($ddns['cloudflare'] ?? null) ? $ddns['cloudflare'] : [];
        $this->saveSetting('ddns_enabled', !empty($ddns['enabled']) ? '1' : '0');
        $this->saveSetting('ddns_provider', $ddns['provider'] ?? 'cloudflare');
        $this->saveSetting('ddns_domain', trim((string) ($ddns['domain'] ?? '')));
        $this->saveSetting('ddns_cf_zone_id', trim((string) ($cloudflare['zone_id'] ?? '')));

        $token = $cloudflare['token'] ?? '';
        if ($token !== '********') {
            $this->saveSetting('ddns_cf_token', trim((string) $token));
        }

        $this->saveSetting('ddns_cf_proxied', !empty($cloudflare['proxied']) ? '1' : '0');
    }

    private function normalizeAccountGroups(array $groups, $allowEmpty = false)
    {
        $normalized = [];

        foreach ($groups as $group) {
            $accessKeyId = trim((string) ($group['AccessKeyId'] ?? ''));
            $accessKeySecret = trim((string) ($group['AccessKeySecret'] ?? ''));
            $regionId = trim((string) ($group['regionId'] ?? ''));

            $isPlaceholder = $accessKeySecret === '********';
            if ($isPlaceholder) {
                $accessKeySecret = $this->resolveExistingSecret($accessKeyId, $regionId);
            }

            if (!$allowEmpty && $accessKeyId === '' && $accessKeySecret === '' && $regionId === '') {
                continue;
            }

            if ($accessKeyId === '' || $accessKeySecret === '' || $regionId === '') {
                if ($isPlaceholder && !$allowEmpty) {
                    throw new Exception('账号配置缺少 AccessKeySecret');
                }
                if ($allowEmpty) {
                    continue;
                }
                throw new Exception('账号配置缺少必填项');
            }

            $groupKey = trim((string) ($group['groupKey'] ?? ''));
            if ($groupKey === '') {
                $groupKey = $this->buildGroupKey($accessKeyId, $regionId);
            }

            $normalized[] = [
                'groupKey' => $groupKey,
                'AccessKeyId' => $accessKeyId,
                'AccessKeySecret' => $accessKeySecret,
                'regionId' => $regionId,
                'siteType' => $group['siteType'] ?? $this->inferSiteType($regionId),
                'maxTraffic' => (float) ($group['maxTraffic'] ?? 200),
                'remark' => trim((string) ($group['remark'] ?? ''))
            ];
        }

        return array_values($normalized);
    }

    private function resolveExistingSecret($accessKeyId, $regionId)
    {
        $accessKeyId = trim((string) $accessKeyId);
        $regionId = trim((string) $regionId);

        foreach ($this->accountsCache as $row) {
            if ($row['access_key_id'] === $accessKeyId && $row['region_id'] === $regionId) {
                return $row['access_key_secret'];
            }
        }

        $groupKey = $this->buildGroupKey($accessKeyId, $regionId);
        foreach ($this->accountsCache as $row) {
            if (($row['group_key'] === $groupKey) && !empty($row['access_key_secret'])) {
                return $row['access_key_secret'];
            }
        }

        $rawGroups = json_decode((string) ($this->configCache['account_groups'] ?? ''), true);
        if (is_array($rawGroups)) {
            foreach ($rawGroups as $group) {
                $savedAccessKeyId = trim((string) ($group['AccessKeyId'] ?? ''));
                $savedRegionId = trim((string) ($group['regionId'] ?? ''));
                $savedSecret = trim((string) ($group['AccessKeySecret'] ?? ''));

                if (
                    $savedAccessKeyId === $accessKeyId
                    && $savedRegionId === $regionId
                    && $savedSecret !== ''
                    && $savedSecret !== '********'
                ) {
                    return $savedSecret;
                }
            }
        }

        return '';
    }

    private function deriveAccountGroupsFromAccounts()
    {
        $groups = [];

        foreach ($this->accountsCache as $row) {
            $accessKeyId = trim((string) ($row['access_key_id'] ?? ''));
            $regionId = trim((string) ($row['region_id'] ?? ''));
            if ($accessKeyId === '' || $regionId === '') {
                continue;
            }

            $groupKey = $row['group_key'] ?: $this->buildGroupKey($accessKeyId, $regionId);
            if (isset($groups[$groupKey])) {
                continue;
            }

            $groups[$groupKey] = [
                'groupKey' => $groupKey,
                'AccessKeyId' => $accessKeyId,
                'AccessKeySecret' => $row['access_key_secret'] ?? '',
                'regionId' => $regionId,
                'siteType' => $row['site_type'] ?? $this->inferSiteType($regionId),
                'maxTraffic' => (float) ($row['max_traffic'] ?? 200),
                'remark' => $row['remark'] ?? ''
            ];
        }

        return array_values($groups);
    }

    private function buildGroupKey($accessKeyId, $regionId)
    {
        return substr(sha1($accessKeyId . '|' . $regionId), 0, 16);
    }

    private function inferSiteType($regionId)
    {
        if (strpos($regionId, 'cn-') === 0 && $regionId !== 'cn-hongkong') {
            return 'china';
        }
        return 'international';
    }

    private function resolveRemark($group, $instance, $existingRow = null)
    {
        if (!empty($group['remark'])) {
            return $group['remark'];
        }

        if ($existingRow) {
            $existingRemark = trim((string) ($existingRow['remark'] ?? ''));
            $existingName = trim((string) ($existingRow['instance_name'] ?? ''));
            if ($existingRemark !== '' && $existingRemark !== $existingName) {
                return $existingRemark;
            }
        }

        if (!empty($instance['instanceName'])) {
            return $instance['instanceName'];
        }

        return $instance['instanceId'] ?? '';
    }

    private function resolveNetworkMetadata($instance, $existingRow = null)
    {
        $eipAllocationId = trim((string) ($instance['eipAllocationId'] ?? ''));
        $eipAddress = trim((string) ($instance['eipAddress'] ?? ''));
        $existingMode = trim((string) ($existingRow['public_ip_mode'] ?? ''));
        $existingManaged = (int) ($existingRow['eip_managed'] ?? 0);

        $mode = $eipAllocationId !== '' ? 'eip' : 'ecs_public_ip';
        if ($existingMode === 'eip' && $eipAllocationId !== '') {
            $mode = 'eip';
        }

        return [
            'public_ip_mode' => $mode,
            'eip_allocation_id' => $eipAllocationId !== '' ? $eipAllocationId : trim((string) ($existingRow['eip_allocation_id'] ?? '')),
            'eip_address' => $eipAddress !== '' ? $eipAddress : ($mode === 'eip' ? trim((string) ($instance['publicIp'] ?? '')) : ''),
            'eip_managed' => $existingManaged
        ];
    }

    public function updateAccountNetworkMetadata($id, array $metadata)
    {
        $stmt = $this->db->prepare("
            UPDATE accounts
            SET public_ip = ?,
                public_ip_mode = ?,
                eip_allocation_id = ?,
                eip_address = ?,
                eip_managed = ?,
                internet_max_bandwidth_out = ?
            WHERE id = ?
        ");

        $stmt->execute([
            $metadata['public_ip'] ?? '',
            $metadata['public_ip_mode'] ?? 'ecs_public_ip',
            $metadata['eip_allocation_id'] ?? '',
            $metadata['eip_address'] ?? '',
            !empty($metadata['eip_managed']) ? 1 : 0,
            (int) ($metadata['internet_max_bandwidth_out'] ?? 0),
            $id
        ]);
        $this->load();
    }

    private function updateGroupBaseSettings($groupKey, $group)
    {
        $stmt = $this->db->prepare("
            UPDATE accounts
            SET access_key_id = ?,
                access_key_secret = ?,
                region_id = ?,
                max_traffic = ?,
                schedule_enabled = ?,
                start_time = ?,
                stop_time = ?,
                site_type = ?,
                group_key = ?
            WHERE group_key = ?
        ");

        $stmt->execute([
            $group['AccessKeyId'],
            $this->encryptValue($group['AccessKeySecret']),
            $group['regionId'],
            $group['maxTraffic'],
            0,
            '',
            '',
            $group['siteType'],
            $groupKey,
            $groupKey
        ]);
    }

    public function deleteAccountById($id)
    {
        $stmt = $this->db->prepare("DELETE FROM accounts WHERE id = ?");
        $stmt->execute([$id]);
        $this->load();
    }

    public function markAccountAsDeleted($id)
    {
        $stmt = $this->db->prepare("UPDATE accounts SET is_deleted = 1 WHERE id = ?");
        $stmt->execute([$id]);
        $this->load();
    }

    public function getPendingReleaseAccounts()
    {
        $stmt = $this->db->query("SELECT * FROM accounts WHERE is_deleted = 1");
        $accounts = $stmt->fetchAll();
        foreach ($accounts as &$row) {
            if (!empty($row['access_key_secret']) && $this->isEncryptedValue($row['access_key_secret'])) {
                $row['access_key_secret'] = $this->decryptValue($row['access_key_secret']);
            }
        }
        return $accounts;
    }

    public function physicallyDeleteAccount($id)
    {
        // 软删除机制：标记为 2 (彻底销毁完毕状态)。
        // 目的：阻断刚执行完释放后，阿里云 API 缓存尚未更新，导致同步器误认为这是"新冒出来的机器"从而执行重新插入。
        // 此尸体记录会在下一次或下下一次定时同步时，由于彻底在阿里云失联，被同步器的 deleteStmt 收尸清理。
        $stmt = $this->db->prepare("UPDATE accounts SET is_deleted = 2, instance_status = 'Released' WHERE id = ?");
        $stmt->execute([$id]);
        // 不强制更新 cache，后台无声息处理。
    }

    public function updateAccountStatus($id, $traffic, $status, $updatedAt, $metadata = [])
    {
        $sql = "UPDATE accounts SET traffic_used = ?, traffic_billing_month = ?, instance_status = ?, updated_at = ?";
        $params = [$traffic, date('Y-m'), $status, $updatedAt];

        if (isset($metadata['health_status'])) {
            $sql .= ", health_status = ?";
            $params[] = $metadata['health_status'];
        }
        if (isset($metadata['stopped_mode'])) {
            $sql .= ", stopped_mode = ?";
            $params[] = $metadata['stopped_mode'];
        }

        $sql .= " WHERE id = ?";
        $params[] = $id;

        $stmt = $this->db->prepare($sql);
        return $stmt->execute($params);
    }

    public function updateLastKeepAlive($id, $time)
    {
        $stmt = $this->db->prepare("UPDATE accounts SET last_keep_alive_at = ? WHERE id = ?");
        return $stmt->execute([$time, $id]);
    }

    public function updateAutoStartBlocked($id, $blocked)
    {
        $stmt = $this->db->prepare("UPDATE accounts SET auto_start_blocked = ? WHERE id = ?");
        return $stmt->execute([$blocked ? 1 : 0, $id]);
    }

    public function blockCurrentlyStoppedInstances()
    {
        $stmt = $this->db->prepare("UPDATE accounts SET auto_start_blocked = 1 WHERE instance_status = 'Stopped'");
        $stmt->execute();
        $this->load();
    }
}
