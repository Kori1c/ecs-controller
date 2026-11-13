package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
	"github.com/Kori1c/ecs-controller/internal/notify"
)

func protectionErrorReason(err error) string {
	if cloud.IsCredentialError(err) {
		return "credential_invalid"
	}
	if cloud.IsPermissionError(err) {
		return "permission_denied"
	}
	return "traffic_api_error"
}

func (w *Worker) notifyProtectionError(ctx context.Context, account *app.Account, err error, now time.Time) {
	if !account.ProtectionSuspended || account.ProtectionNotifiedAt != 0 {
		return
	}
	title, advice := "", ""
	switch account.ProtectionSuspendReason {
	case "credential_invalid":
		title = "阿里云凭据异常"
		advice = "请检查 AccessKey、签名配置或安全令牌，接口恢复后系统将自动重试。"
	case "permission_denied":
		title = "阿里云接口访问受限"
		advice = "请检查 RAM 权限、访问策略及账号限制，不要仅凭此错误更换 AccessKey。"
	default:
		return
	}
	w.dispatchEvent(ctx, notify.Event{Title: title, Summary: "已暂停自动停机保护", AccountID: accountLabel(*account), Text: fmt.Sprintf("【ECS 控制台】%s\n账号/实例: %s\n实例 ID: %s\n区域: %s\n错误: %s\n系统已暂停自动停机保护，%s", title, accountLabel(*account), account.InstanceID, account.RegionID, err.Error(), advice), Fields: map[string]string{"instance_id": account.InstanceID, "reason": account.ProtectionSuspendReason}})
	account.ProtectionNotifiedAt = now.Unix()
}

func (w *Worker) handleTrafficError(ctx context.Context, client cloud.Client, account *app.Account, now time.Time, threshold float64, thresholdAction, shutdownMode, message string, err error, keepAlive, monthlyAutoStart bool) {
	previousReason, previousNotifiedAt := account.ProtectionSuspendReason, account.ProtectionNotifiedAt
	reason := protectionErrorReason(err)
	account.TrafficAPIStatus, account.TrafficAPIMessage = "error", message
	account.ProtectionSuspended, account.ProtectionSuspendReason = true, reason
	account.UpdatedAt, account.TrafficBillingMonth = now.Unix(), now.Format("2006-01")
	// A previous failure notification must not suppress a CDT threshold alert.
	// Keep the threshold alert timestamp when the previous cycle was healthy.
	if previousReason != "" {
		account.ProtectionNotifiedAt = 0
	}
	w.applyTrafficProtection(ctx, client, account, now, threshold, thresholdAction, shutdownMode, account.TrafficUsed, false, keepAlive, monthlyAutoStart)
	if account.ProtectionSuspended {
		account.ProtectionNotifiedAt = 0
		if previousReason == reason {
			account.ProtectionNotifiedAt = previousNotifiedAt
		}
	}
	w.notifyProtectionError(ctx, account, err, now)
}
