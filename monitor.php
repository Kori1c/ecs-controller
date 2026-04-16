<?php
// 此文件用于 Cron 任务
// 输出简洁的文本日志

require_once 'AliyunTrafficCheck.php';

header('Content-Type: text/plain; charset=utf-8');

$app = new AliyunTrafficCheck();

// CLI 模式直接运行，Web 模式使用 Bearer Token 鉴权
$isCli = (PHP_SAPI === 'cli');

if (!$isCli) {
    $authHeader = $_SERVER['HTTP_AUTHORIZATION'] ?? $_SERVER['HTTP_X_MONITOR_TOKEN'] ?? '';
    if (preg_match('/^Bearer\s+(.+)$/i', $authHeader, $matches)) {
        $authHeader = $matches[1];
    }

    $monitorKey = $app->getMonitorKey();
    if (empty($monitorKey) || !hash_equals($monitorKey, $authHeader)) {
        http_response_code(403);
        echo "访问被拒绝，请使用有效的监控密钥。";
        exit;
    }
}

// 输出简洁日志
echo "--- ECS 服务器管理 开始检测: " . date('Y-m-d H:i:s') . " ---\n";
echo $app->monitor();
echo "\n--- 检测结束 ---\n";
