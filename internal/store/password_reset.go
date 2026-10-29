package store

import (
	"fmt"
	"golang.org/x/crypto/bcrypt"
)

// ResetAdminPassword is only exposed by the local CLI, never by an HTTP route.
// Credentials and runtime authorization state change in one transaction.
func (s *Store) ResetAdminPassword(password string) error {
	if len(password) < 6 || len(password) > 72 {
		return fmt.Errorf("管理员密码需要 6 到 72 字节")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current string
	if err = tx.QueryRow(`SELECT value FROM settings WHERE key='admin_password'`).Scan(&current); err != nil || current == "" {
		return fmt.Errorf("系统尚未初始化，请先通过网页完成初始化")
	}
	for key, value := range map[string]string{"admin_password": string(hash), "password_login_enabled": "1"} {
		if _, err = tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
			return err
		}
	}
	for _, table := range []string{"sessions", "login_attempts", "passkey_challenges", "telegram_action_tokens"} {
		if _, err = tx.Exec(`DELETE FROM ` + table); err != nil {
			return err
		}
	}
	return tx.Commit()
}
