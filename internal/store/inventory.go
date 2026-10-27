package store

import (
	"fmt"
	"strconv"
)

// PurgeCloudMissing atomically protects every duplicate and its unfinished
// release work. Hidden rows remain available to the release worker, including
// expired leases and failed jobs awaiting an explicit retry.
func (s *Store) PurgeCloudMissing(identity, region, instance string) (bool, error) {
	if identity == "" || region == "" || instance == "" {
		return false, fmt.Errorf("实例清理身份不完整")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id FROM accounts WHERE access_key_id=? AND region_id=? AND instance_id=?`, identity, region, instance)
	if err != nil {
		return false, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	if len(ids) == 0 {
		return false, tx.Commit()
	}
	protected := false
	for _, id := range ids {
		var count int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind='delete_instance' AND entity_key=? AND status<>'done'`, strconv.FormatInt(id, 10)).Scan(&count); err != nil {
			return false, err
		}
		if count > 0 {
			protected = true
		}
	}
	if protected {
		// Do not overwrite Releasing/ReleaseFailed: retries must retain their state.
		_, err = tx.Exec(`UPDATE accounts SET cloud_presence='missing',protection_suspended=1,protection_suspend_reason='instance_missing' WHERE access_key_id=? AND region_id=? AND instance_id=?`, identity, region, instance)
		if err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	for _, id := range ids {
		for _, table := range []string{"traffic_hourly", "traffic_daily", "billing_cache", "instance_traffic_usage", "telegram_action_tokens"} {
			if _, err = tx.Exec(`DELETE FROM `+table+` WHERE account_id=?`, id); err != nil {
				return false, err
			}
		}
		if _, err = tx.Exec(`DELETE FROM jobs WHERE entity_key=?`, strconv.FormatInt(id, 10)); err != nil {
			return false, err
		}
	}
	if _, err = tx.Exec(`DELETE FROM logs WHERE instr(message,?)>0`, instance); err != nil {
		return false, err
	}
	if _, err = tx.Exec(`DELETE FROM ecs_create_tasks WHERE instance_id=?`, instance); err != nil {
		return false, err
	}
	if _, err = tx.Exec(`DELETE FROM accounts WHERE access_key_id=? AND region_id=? AND instance_id=?`, identity, region, instance); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
