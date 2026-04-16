<?php
ini_set('session.cookie_httponly', 1);
ini_set('session.cookie_secure', isset($_SERVER['HTTPS']) ? 1 : 0);
ini_set('session.cookie_samesite', 'Lax');
ini_set('session.use_strict_mode', 1);
ini_set('session.use_only_cookies', 1);
session_start();

error_reporting(E_ALL & ~E_NOTICE & ~E_WARNING & ~E_DEPRECATED);
ini_set('display_errors', 0);

require_once 'AliyunTrafficCheck.php';

header("Cache-Control: no-store, no-cache, must-revalidate, max-age=0");

$app = new AliyunTrafficCheck();
$action = $_GET['action'] ?? 'view';

// ---------------- 公开接口 ----------------

if ($action === 'check_init') {
    header('Content-Type: application/json');
    $initError = $app->getInitError();
    if ($initError) {
        echo json_encode(['initialized' => false, 'error' => $initError]);
    } else {
        echo json_encode(['initialized' => $app->isInitialized()]);
    }
    exit;
}

if ($action === 'setup') {
    header('Content-Type: application/json');
    if ($app->isInitialized()) {
        http_response_code(403);
        echo json_encode(['success' => false, 'message' => '系统已完成初始化']);
        exit;
    }
    
    $data = json_decode(file_get_contents('php://input'), true);
    try {
        if ($app->setup($data)) {
            $_SESSION['is_admin'] = true;
            echo json_encode(['success' => true]);
        } else {
            echo json_encode(['success' => false, 'message' => '初始化失败']);
        }
    } catch (Exception $e) {
        echo json_encode(['success' => false, 'message' => $e->getMessage()]);
    }
    exit;
}

if ($action === 'login') {
    $data = json_decode(file_get_contents('php://input'), true);
    try {
        if ($app->login($data['password'] ?? '')) {
            $_SESSION['is_admin'] = true;
            echo json_encode(['success' => true]);
        } else {
            echo json_encode(['success' => false, 'message' => '密码错误']);
        }
    } catch (Exception $e) {
        echo json_encode(['success' => false, 'message' => $e->getMessage()]);
    }
    exit;
}

if ($action === 'check_login') {
    echo json_encode(['logged_in' => isset($_SESSION['is_admin']) && $_SESSION['is_admin'] === true]);
    exit;
}

if ($action === 'get_status') {
    header('Content-Type: application/json; charset=utf-8');
    $initError = $app->getInitError();
    if ($initError) {
        echo json_encode(['error' => $initError]);
    } else {
        $isAdmin = isset($_SESSION['is_admin']) && $_SESSION['is_admin'] === true;
        if (!$isAdmin) {
            http_response_code(403);
            echo json_encode(['error' => '请先登录后再操作']);
        } else {
            echo json_encode($app->getStatusForFrontend(true));
        }
    }
    exit;
}

// ---------------- 需鉴权接口 ----------------

if ($action !== 'view' && !isset($_SESSION['is_admin'])) {
    http_response_code(403);
    echo json_encode(['error' => '请先登录后再操作']);
    exit;
}

if ($action === 'get_config') {
    echo json_encode($app->getConfigForFrontend());
    exit;
}

if ($action === 'save_config') {
    $data = json_decode(file_get_contents('php://input'), true);
    if ($app->updateConfig($data)) {
        echo json_encode(['success' => true]);
    } else {
        echo json_encode(['success' => false, 'message' => '保存失败']);
    }
    exit;
}

if ($action === 'send_test_email') {
    $data = json_decode(file_get_contents('php://input'), true);
    $result = $app->sendTestEmail($data['email'] ?? '');
    echo json_encode(['success' => $result === true, 'message' => $result]);
    exit;
}

if ($action === 'send_test_telegram') {
    $data = json_decode(file_get_contents('php://input'), true);
    $result = $app->sendTestTelegram($data['telegram'] ?? []);
    echo json_encode(['success' => $result === true, 'message' => $result]);
    exit;
}

if ($action === 'send_test_webhook') {
    $data = json_decode(file_get_contents('php://input'), true);
    $result = $app->sendTestWebhook($data['webhook'] ?? []);
    echo json_encode(['success' => $result === true, 'message' => $result]);
    exit;
}

if ($action === 'refresh_account') {
    $data = json_decode(file_get_contents('php://input'), true);
    $id = $data['id'] ?? 0;
    $result = $app->refreshAccount($id);
    if ($result === false) {
        echo json_encode(['success' => false, 'message' => '刷新失败']);
    } elseif (is_array($result)) {
        // 流量/状态刷新成功，但账单获取失败
        echo json_encode($result);
    } else {
        echo json_encode(['success' => true]);
    }
    exit;
}

if ($action === 'fetch_instances') {
    header('Content-Type: application/json; charset=utf-8');
    $data = json_decode(file_get_contents('php://input'), true);

    try {
        $instances = $app->fetchInstances($data['accessKeyId'] ?? '', $data['accessKeySecret'] ?? '', $data['regionId'] ?? '');
        echo json_encode(['success' => true, 'data' => $instances]);
    } catch (Exception $e) {
        http_response_code(400);
        echo json_encode(['success' => false, 'message' => $e->getMessage()]);
    }
    exit;
}

if ($action === 'test_account') {
    header('Content-Type: application/json; charset=utf-8');
    $data = json_decode(file_get_contents('php://input'), true);

    try {
        $result = $app->testAccountCredentials($data['account'] ?? []);
        echo json_encode($result);
    } catch (Exception $e) {
        http_response_code(400);
        echo json_encode([
            'success' => false,
            'message' => $e->getMessage()
        ]);
    }
    exit;
}

if ($action === 'sync_account_group') {
    header('Content-Type: application/json; charset=utf-8');
    $data = json_decode(file_get_contents('php://input'), true) ?: [];

    try {
        echo json_encode($app->syncAccountGroup($data['groupKey'] ?? ''));
    } catch (Exception $e) {
        http_response_code(400);
        echo json_encode([
            'success' => false,
            'message' => $e->getMessage()
        ]);
    }
    exit;
}

if ($action === 'preview_ecs_create') {
    header('Content-Type: application/json; charset=utf-8');
    $data = json_decode(file_get_contents('php://input'), true) ?: [];

    try {
        $result = $app->previewEcsCreate($data);
        $_SESSION['ecs_create_previews'] = $_SESSION['ecs_create_previews'] ?? [];
        $_SESSION['ecs_create_previews'][$result['previewId']] = [
            'summary' => $result['summary'],
            'created_at' => time()
        ];
        echo json_encode($result);
    } catch (Exception $e) {
        http_response_code(400);
        echo json_encode(['success' => false, 'message' => $e->getMessage()]);
    }
    exit;
}

if ($action === 'create_ecs') {
    header('Content-Type: application/json; charset=utf-8');
    $data = json_decode(file_get_contents('php://input'), true) ?: [];
    $previewId = $data['previewId'] ?? '';
    $confirmed = !empty($data['confirmed']);

    try {
        if (!$confirmed) {
            throw new Exception('请先确认配置清单和费用提示');
        }
        $previewStore = $_SESSION['ecs_create_previews'][$previewId] ?? null;
        if (!$previewStore || (time() - ($previewStore['created_at'] ?? 0)) > 900) {
            throw new Exception('配置清单已过期，请重新预检');
        }

        $result = $app->createEcsFromPreview($previewId, $previewStore['summary']);
        unset($_SESSION['ecs_create_previews'][$previewId]);
        echo json_encode($result);
    } catch (Exception $e) {
        http_response_code(400);
        echo json_encode(['success' => false, 'message' => $e->getMessage()]);
    }
    exit;
}

if ($action === 'get_ecs_create_task') {
    header('Content-Type: application/json; charset=utf-8');
    $taskId = $_GET['taskId'] ?? '';
    $task = $app->getEcsCreateTask($taskId);
    if (!$task) {
        http_response_code(404);
        echo json_encode(['success' => false, 'message' => '任务不存在']);
    } else {
        unset($task['login_password']);
        echo json_encode(['success' => true, 'data' => $task]);
    }
    exit;
}

// 修改：获取系统日志，支持 Tab
if ($action === 'get_logs') {
    header('Content-Type: application/json; charset=utf-8');
    $tab = $_GET['tab'] ?? 'action'; // 默认是动作日志
    echo json_encode(['data' => $app->getSystemLogs($tab)]);
    exit;
}

// 新增：清空日志
if ($action === 'clear_logs') {
    $data = json_decode(file_get_contents('php://input'), true);
    $tab = $data['tab'] ?? 'action';
    if ($app->clearSystemLogs($tab)) {
        echo json_encode(['success' => true]);
    } else {
        echo json_encode(['success' => false, 'message' => '清空失败']);
    }
    exit;
}

if ($action === 'get_history') {
    header('Content-Type: application/json; charset=utf-8');
    $id = $_GET['id'] ?? 0;
    echo json_encode(['data' => $app->getAccountHistory($id)]);
    exit;
}

if ($action === 'logout') {
    session_destroy();
    echo json_encode(['success' => true]);
    exit;
}

if ($action === 'get_all_instances') {
    header('Content-Type: application/json; charset=utf-8');
    $sync = ($_GET['sync'] ?? '0') === '1';
    echo json_encode(['data' => $app->getAllManagedInstances($sync)]);
    exit;
}

if ($action === 'control_instance') {
    $data = json_decode(file_get_contents('php://input'), true);
    $accountId = $data['accountId'] ?? 0;
    $actionType = $data['action'] ?? '';
    $shutdownMode = $data['shutdownMode'] ?? 'KeepCharging';

    if (!in_array($actionType, ['start', 'stop'])) {
        http_response_code(400);
        echo json_encode(['success' => false, 'message' => '无效的操作类型']);
        exit;
    }

    $result = $app->controlInstanceAction($accountId, $actionType, $shutdownMode);
    echo json_encode(['success' => $result]);
    exit;
}

if ($action === 'delete_instance') {
    $data = json_decode(file_get_contents('php://input'), true);
    $accountId = $data['accountId'] ?? 0;
    $forceStop = $data['forceStop'] ?? false;

    $result = $app->deleteInstanceAction($accountId, $forceStop);
    echo json_encode(['success' => $result]);
    exit;
}

echo $app->renderTemplate();
