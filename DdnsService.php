<?php

class DdnsService
{
    private $config;

    public function __construct(array $config)
    {
        $this->config = $config;
    }

    public function isEnabled()
    {
        return ($this->config['ddns_enabled'] ?? '0') === '1'
            && ($this->config['ddns_provider'] ?? 'cloudflare') === 'cloudflare'
            && !empty($this->config['ddns_cf_token'])
            && !empty($this->config['ddns_domain']);
    }

    public function buildRecordName(array $account, $sameGroupInstanceCount = 1)
    {
        $domain = $this->normalizeDomain($this->config['ddns_domain'] ?? '');
        if ($domain === '') {
            throw new Exception('请先填写 DDNS 根域名');
        }

        $accountSlug = $this->slug($account['account_remark'] ?? $account['remark'] ?? '');
        $instanceSlug = $this->slug($account['instance_name'] ?? '');
        $instanceId = trim((string) ($account['instance_id'] ?? ''));
        $shortId = $this->slug($instanceId !== '' ? preg_replace('/^i-/', '', $instanceId) : '');

        if ($accountSlug === '') {
            $accountSlug = $instanceSlug ?: $shortId;
        }
        if ($accountSlug === '') {
            throw new Exception('DDNS 记录名生成失败，请检查账号备注或实例名称');
        }

        $subdomain = $accountSlug;
        if ((int) $sameGroupInstanceCount > 1) {
            $suffix = $instanceSlug ?: $shortId;
            if ($suffix !== '' && $suffix !== $accountSlug) {
                $subdomain .= '-' . $suffix;
            }
        }

        $subdomain = trim($subdomain, '-');
        return $subdomain . '.' . $domain;
    }

    public function syncARecord($recordName, $ip)
    {
        if (!$this->isEnabled()) {
            return ['success' => true, 'skipped' => true, 'message' => 'DDNS 未启用'];
        }

        $recordName = strtolower(trim((string) $recordName));
        $ip = trim((string) $ip);
        if (!filter_var($ip, FILTER_VALIDATE_IP, FILTER_FLAG_IPV4 | FILTER_FLAG_NO_PRIV_RANGE | FILTER_FLAG_NO_RES_RANGE)) {
            return ['success' => false, 'message' => '公网 IP 为空或不是公网 IPv4'];
        }

        $existing = $this->findRecord($recordName);
        $payload = [
            'type' => 'A',
            'name' => $recordName,
            'content' => $ip,
            'ttl' => 1,
            'proxied' => ($this->config['ddns_cf_proxied'] ?? '0') === '1',
            'comment' => 'Managed by CDT Monitor'
        ];

        if ($existing) {
            $response = $this->request('PUT', '/dns_records/' . $existing['id'], $payload);
        } else {
            $response = $this->request('POST', '/dns_records', $payload);
        }

        if (empty($response['success'])) {
            return ['success' => false, 'message' => $this->formatErrors($response)];
        }

        return [
            'success' => true,
            'record' => $recordName,
            'ip' => $ip,
            'action' => $existing ? 'updated' : 'created'
        ];
    }

    public function deleteARecord($recordName)
    {
        if (!$this->isEnabled()) {
            return ['success' => true, 'skipped' => true, 'message' => 'DDNS 未启用'];
        }

        $recordName = strtolower(trim((string) $recordName));
        if ($recordName === '') {
            return ['success' => false, 'message' => 'DDNS 记录名为空'];
        }

        $existing = $this->findRecord($recordName);
        if (!$existing) {
            return ['success' => true, 'skipped' => true, 'record' => $recordName, 'message' => '记录不存在'];
        }

        $response = $this->request('DELETE', '/dns_records/' . $existing['id']);
        if (empty($response['success'])) {
            return ['success' => false, 'message' => $this->formatErrors($response)];
        }

        return [
            'success' => true,
            'record' => $recordName,
            'action' => 'deleted'
        ];
    }

    private function findRecord($recordName)
    {
        $response = $this->request('GET', '/dns_records?type=A&name=' . rawurlencode($recordName));
        if (empty($response['success'])) {
            throw new Exception('Cloudflare 查询记录失败: ' . $this->formatErrors($response));
        }

        $records = $response['result'] ?? [];
        return $records[0] ?? null;
    }

    private function request($method, $path, array $payload = null)
    {
        $zoneId = $this->resolveZoneId();
        $token = trim((string) ($this->config['ddns_cf_token'] ?? ''));
        $url = 'https://api.cloudflare.com/client/v4/zones/' . rawurlencode($zoneId) . $path;

        $ch = curl_init($url);
        curl_setopt($ch, CURLOPT_CUSTOMREQUEST, $method);
        curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
        curl_setopt($ch, CURLOPT_TIMEOUT, 15);
        curl_setopt($ch, CURLOPT_HTTPHEADER, [
            'Authorization: Bearer ' . $token,
            'Content-Type: application/json'
        ]);

        if ($payload !== null) {
            curl_setopt($ch, CURLOPT_POSTFIELDS, json_encode($payload, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE));
        }

        $body = curl_exec($ch);
        $error = curl_error($ch);
        $httpCode = curl_getinfo($ch, CURLINFO_HTTP_CODE);
        curl_close($ch);

        if ($error) {
            throw new Exception('Cloudflare 网络请求失败: ' . $error);
        }

        $decoded = json_decode((string) $body, true);
        if (!is_array($decoded)) {
            throw new Exception("Cloudflare 响应解析失败，状态码 {$httpCode}");
        }

        return $decoded;
    }

    private function resolveZoneId()
    {
        $zoneId = trim((string) ($this->config['ddns_cf_zone_id'] ?? ''));
        if ($zoneId !== '') {
            return $zoneId;
        }

        $domain = $this->normalizeDomain($this->config['ddns_domain'] ?? '');
        if ($domain === '') {
            throw new Exception('请先填写 DDNS 根域名');
        }

        $token = trim((string) ($this->config['ddns_cf_token'] ?? ''));
        $url = 'https://api.cloudflare.com/client/v4/zones?name=' . rawurlencode($domain) . '&status=active';
        $ch = curl_init($url);
        curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
        curl_setopt($ch, CURLOPT_TIMEOUT, 15);
        curl_setopt($ch, CURLOPT_HTTPHEADER, [
            'Authorization: Bearer ' . $token,
            'Content-Type: application/json'
        ]);
        $body = curl_exec($ch);
        $error = curl_error($ch);
        curl_close($ch);

        if ($error) {
            throw new Exception('Cloudflare Zone 查询失败: ' . $error);
        }

        $decoded = json_decode((string) $body, true);
        if (empty($decoded['success'])) {
            throw new Exception('Cloudflare Zone 查询失败: ' . $this->formatErrors(is_array($decoded) ? $decoded : []));
        }

        $zone = $decoded['result'][0] ?? null;
        if (empty($zone['id'])) {
            throw new Exception("Cloudflare 未找到域名 {$domain}，请确认 Token 有该域名的 DNS 编辑权限，或手动填写 Zone ID");
        }

        return $zone['id'];
    }

    private function formatErrors(array $response)
    {
        $errors = $response['errors'] ?? [];
        if (empty($errors)) {
            return '未知错误';
        }

        $messages = [];
        foreach ($errors as $error) {
            $messages[] = $error['message'] ?? json_encode($error, JSON_UNESCAPED_UNICODE);
        }
        return implode('；', $messages);
    }

    private function slug($value)
    {
        $original = trim((string) $value);
        $value = strtolower($original);
        if ($value === '') {
            return '';
        }

        if (function_exists('iconv')) {
            $converted = @iconv('UTF-8', 'ASCII//TRANSLIT//IGNORE', $value);
            if ($converted !== false) {
                $value = $converted;
            }
        }

        $value = preg_replace('/[^a-z0-9]+/', '-', $value);
        $value = trim($value, '-');
        if ($value === '') {
            $value = substr(sha1($original), 0, 8);
        }
        return substr($value, 0, 48);
    }

    private function normalizeDomain($domain)
    {
        $domain = strtolower(trim((string) $domain));
        $domain = preg_replace('#^https?://#', '', $domain);
        $domain = trim(explode('/', $domain)[0] ?? '', '.');
        return $domain;
    }
}
