package store

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/nacl/secretbox"
	_ "modernc.org/sqlite"
)

type Store struct {
	DB    *sql.DB
	Key   [32]byte
	keyMu sync.RWMutex
}

var ErrAccountNotObservable = errors.New("account cannot be marked missing")

type Job struct {
	ID          int64
	JobID       string
	Kind        string
	EntityKey   string
	Status      string
	Payload     string
	Attempts    int
	AvailableAt int64
}

type TrafficSample struct {
	TrafficBytes float64
	LastSampleMS int64
}

type TelegramActionToken struct {
	ID        int64
	Token     string
	UserID    string
	ChatID    string
	Action    string
	AccountID int64
	Payload   string
	ExpiresAt int64
}

func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	dbPath := filepath.Join(dataDir, "data.sqlite")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db}
	if err := s.loadKey(filepath.Join(dataDir, ".secret_encryption.key")); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) loadKey(path string) error {
	if raw, err := os.ReadFile(path); err == nil {
		if len(raw) != 32 {
			return fmt.Errorf("encryption key %s has invalid length", path)
		}
		copy(s.Key[:], raw)
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read encryption key: %w", err)
	}

	if _, err := rand.Read(s.Key[:]); err != nil {
		return fmt.Errorf("generate encryption key: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return s.loadKey(path)
		}
		return fmt.Errorf("create encryption key: %w", err)
	}
	if _, err := f.Write(s.Key[:]); err != nil {
		f.Close()
		return fmt.Errorf("write encryption key: %w", err)
	}
	return f.Close()
}

func (s *Store) Seal(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	s.keyMu.RLock()
	defer s.keyMu.RUnlock()
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	ciphertext := secretbox.Seal(nil, []byte(value), &nonce, &s.Key)
	raw := append(nonce[:], ciphertext...)
	return "ENC1" + base64.StdEncoding.EncodeToString(raw), nil
}

func (s *Store) OpenSecret(value string) (string, error) {
	if value == "" || !strings.HasPrefix(value, "ENC1") {
		return value, nil
	}
	s.keyMu.RLock()
	defer s.keyMu.RUnlock()
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, "ENC1"))
	if err != nil || len(raw) < 24 {
		return "", fmt.Errorf("invalid encrypted value")
	}
	var nonce [24]byte
	copy(nonce[:], raw[:24])
	plain, ok := secretbox.Open(nil, raw[24:], &nonce, &s.Key)
	if !ok {
		return "", fmt.Errorf("unable to decrypt value")
	}
	return string(plain), nil
}

// EncryptionKey returns a copy of the key used to protect stored credentials.
// Backups include it inside the encrypted archive so a restore remains portable.
func (s *Store) EncryptionKey() [32]byte {
	s.keyMu.RLock()
	defer s.keyMu.RUnlock()
	return s.Key
}

func (s *Store) migrate() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT)`,
		`CREATE TABLE IF NOT EXISTS accounts (
			id INTEGER PRIMARY KEY AUTOINCREMENT, access_key_id TEXT, access_key_secret TEXT,
			region_id TEXT, instance_id TEXT, max_traffic REAL DEFAULT 200,
			schedule_enabled INTEGER DEFAULT 0, schedule_start_enabled INTEGER DEFAULT 0,
			schedule_stop_enabled INTEGER DEFAULT 0, start_time TEXT DEFAULT '', stop_time TEXT DEFAULT '',
			traffic_used REAL DEFAULT 0, traffic_billing_month TEXT DEFAULT '', instance_status TEXT DEFAULT 'Unknown',
			updated_at INTEGER DEFAULT 0, last_keep_alive_at INTEGER DEFAULT 0, auto_start_blocked INTEGER DEFAULT 0,
			schedule_last_start_date TEXT DEFAULT '', schedule_last_stop_date TEXT DEFAULT '', schedule_stop_active INTEGER DEFAULT 0,
			schedule_blocked_by_traffic INTEGER DEFAULT 0, remark TEXT DEFAULT '', site_type TEXT DEFAULT 'international',
			group_key TEXT DEFAULT '', instance_name TEXT DEFAULT '', instance_type TEXT DEFAULT '',
			internet_max_bandwidth_out INTEGER DEFAULT 0, public_ip TEXT DEFAULT '', public_ip_mode TEXT DEFAULT 'ecs_public_ip',
			eip_allocation_id TEXT DEFAULT '', eip_address TEXT DEFAULT '', eip_managed INTEGER DEFAULT 0,
			private_ip TEXT DEFAULT '', cpu INTEGER DEFAULT 0, memory INTEGER DEFAULT 0, os_name TEXT DEFAULT '',
			stopped_mode TEXT DEFAULT '', health_status TEXT DEFAULT 'Unknown', traffic_api_status TEXT DEFAULT 'ok',
			traffic_api_message TEXT DEFAULT '', protection_suspended INTEGER DEFAULT 0,
			protection_suspend_reason TEXT DEFAULT '', protection_suspend_notified_at INTEGER DEFAULT 0,
			last_seen_at INTEGER DEFAULT 0, missing_count INTEGER DEFAULT 0, missing_since INTEGER DEFAULT 0,
			cloud_presence TEXT DEFAULT 'present', is_deleted INTEGER DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS logs (id INTEGER PRIMARY KEY AUTOINCREMENT, type TEXT, message TEXT, created_at INTEGER)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_type_id ON logs(type,id)`,
		`CREATE TABLE IF NOT EXISTS login_attempts (id INTEGER PRIMARY KEY AUTOINCREMENT, ip TEXT, attempt_time INTEGER)`,
		`CREATE TABLE IF NOT EXISTS sessions (id TEXT PRIMARY KEY, csrf_token TEXT NOT NULL, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS traffic_hourly (id INTEGER PRIMARY KEY AUTOINCREMENT, account_id INTEGER, traffic REAL, recorded_at INTEGER)`,
		`CREATE TABLE IF NOT EXISTS traffic_daily (id INTEGER PRIMARY KEY AUTOINCREMENT, account_id INTEGER, traffic REAL, recorded_at INTEGER)`,
		`CREATE TABLE IF NOT EXISTS billing_cache (id INTEGER PRIMARY KEY AUTOINCREMENT, account_id INTEGER NOT NULL, cache_type TEXT NOT NULL, billing_cycle TEXT DEFAULT '', data TEXT NOT NULL, updated_at INTEGER NOT NULL, UNIQUE(account_id, cache_type, billing_cycle))`,
		`CREATE TABLE IF NOT EXISTS instance_traffic_usage (id INTEGER PRIMARY KEY AUTOINCREMENT, account_id INTEGER NOT NULL, instance_id TEXT NOT NULL, billing_month TEXT NOT NULL, traffic_bytes REAL DEFAULT 0, last_sample_ms INTEGER DEFAULT 0, updated_at INTEGER NOT NULL, UNIQUE(account_id, instance_id, billing_month))`,
		`CREATE TABLE IF NOT EXISTS ecs_create_tasks (id INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT UNIQUE NOT NULL, preview_id TEXT DEFAULT '', account_group_key TEXT NOT NULL, region_id TEXT NOT NULL, zone_id TEXT DEFAULT '', instance_type TEXT NOT NULL, image_id TEXT DEFAULT '', os_label TEXT DEFAULT '', instance_name TEXT DEFAULT '', vpc_id TEXT DEFAULT '', vswitch_id TEXT DEFAULT '', security_group_id TEXT DEFAULT '', internet_max_bandwidth_out INTEGER DEFAULT 0, system_disk_category TEXT DEFAULT '', system_disk_size INTEGER DEFAULT 0, instance_id TEXT DEFAULT '', public_ip TEXT DEFAULT '', public_ip_mode TEXT DEFAULT 'ecs_public_ip', eip_allocation_id TEXT DEFAULT '', eip_address TEXT DEFAULT '', eip_managed INTEGER DEFAULT 0, login_user TEXT DEFAULT '', login_password TEXT DEFAULT '', status TEXT NOT NULL, step TEXT DEFAULT '', error_message TEXT DEFAULT '', payload TEXT DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS jobs (id INTEGER PRIMARY KEY AUTOINCREMENT, job_id TEXT UNIQUE NOT NULL, kind TEXT NOT NULL, entity_key TEXT NOT NULL, status TEXT NOT NULL, payload TEXT DEFAULT '', attempts INTEGER DEFAULT 0, available_at INTEGER NOT NULL, locked_at INTEGER DEFAULT 0, last_error TEXT DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_active ON jobs(kind, entity_key) WHERE status IN ('queued','running','retry')`,
		`CREATE TABLE IF NOT EXISTS telegram_bot_state (key TEXT PRIMARY KEY, value TEXT)`,
		`CREATE TABLE IF NOT EXISTS telegram_action_tokens (id INTEGER PRIMARY KEY AUTOINCREMENT, token TEXT UNIQUE NOT NULL, user_id TEXT NOT NULL, chat_id TEXT NOT NULL, action TEXT NOT NULL, account_id INTEGER NOT NULL, payload TEXT DEFAULT '', expires_at INTEGER NOT NULL, used_at INTEGER DEFAULT 0, created_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS passkey_credentials (id INTEGER PRIMARY KEY AUTOINCREMENT, credential_id TEXT UNIQUE NOT NULL, credential_data TEXT NOT NULL, created_at INTEGER NOT NULL, last_used_at INTEGER DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS passkey_challenges (id TEXT PRIMARY KEY, kind TEXT NOT NULL, session_id TEXT DEFAULT '', session_data TEXT NOT NULL, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL)`,
	}
	for _, statement := range statements {
		if _, err := s.DB.Exec(statement); err != nil {
			return fmt.Errorf("migrate schema: %w", err)
		}
	}
	// PHP versions before v1.6 used access_key_id in traffic tables. Add the
	// current column first; this keeps old databases openable during migration.
	for _, spec := range []struct{ table, column, definition string }{
		{"accounts", "traffic_billing_month", "TEXT DEFAULT ''"},
		{"accounts", "schedule_enabled", "INTEGER DEFAULT 0"},
		{"accounts", "schedule_start_enabled", "INTEGER DEFAULT 0"},
		{"accounts", "schedule_stop_enabled", "INTEGER DEFAULT 0"},
		{"accounts", "start_time", "TEXT DEFAULT ''"},
		{"accounts", "stop_time", "TEXT DEFAULT ''"},
		{"accounts", "traffic_used", "REAL DEFAULT 0"},
		{"traffic_hourly", "account_id", "INTEGER"},
		{"traffic_daily", "account_id", "INTEGER"},
		{"ecs_create_tasks", "public_ip_mode", "TEXT DEFAULT 'ecs_public_ip'"},
		{"ecs_create_tasks", "eip_allocation_id", "TEXT DEFAULT ''"},
		{"ecs_create_tasks", "eip_address", "TEXT DEFAULT ''"},
		{"ecs_create_tasks", "eip_managed", "INTEGER DEFAULT 0"},
	} {
		if spec.column == "account_key_placeholder" {
			continue
		}
		if err := ensureColumn(s.DB, spec.table, spec.column, spec.definition); err != nil {
			return err
		}
	}
	if err := s.migrateLegacyTrafficStats(); err != nil {
		return err
	}
	if _, err := s.DB.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_traffic_hourly_unique ON traffic_hourly(account_id, recorded_at)`); err != nil {
		return fmt.Errorf("migrate hourly index: %w", err)
	}
	if _, err := s.DB.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_traffic_daily_unique ON traffic_daily(account_id, recorded_at)`); err != nil {
		return fmt.Errorf("migrate daily index: %w", err)
	}
	for _, spec := range []struct{ table, column, definition string }{
		{"accounts", "auto_start_blocked", "INTEGER DEFAULT 0"}, {"accounts", "schedule_last_start_date", "TEXT DEFAULT ''"}, {"accounts", "schedule_last_stop_date", "TEXT DEFAULT ''"},
		{"accounts", "schedule_stop_active", "INTEGER DEFAULT 0"},
		{"accounts", "schedule_blocked_by_traffic", "INTEGER DEFAULT 0"}, {"accounts", "remark", "TEXT DEFAULT ''"}, {"accounts", "site_type", "TEXT DEFAULT 'international'"}, {"accounts", "group_key", "TEXT DEFAULT ''"}, {"accounts", "instance_name", "TEXT DEFAULT ''"}, {"accounts", "instance_type", "TEXT DEFAULT ''"}, {"accounts", "internet_max_bandwidth_out", "INTEGER DEFAULT 0"}, {"accounts", "public_ip", "TEXT DEFAULT ''"}, {"accounts", "public_ip_mode", "TEXT DEFAULT 'ecs_public_ip'"}, {"accounts", "eip_allocation_id", "TEXT DEFAULT ''"}, {"accounts", "eip_address", "TEXT DEFAULT ''"}, {"accounts", "eip_managed", "INTEGER DEFAULT 0"}, {"accounts", "private_ip", "TEXT DEFAULT ''"}, {"accounts", "cpu", "INTEGER DEFAULT 0"}, {"accounts", "memory", "INTEGER DEFAULT 0"}, {"accounts", "os_name", "TEXT DEFAULT ''"}, {"accounts", "stopped_mode", "TEXT DEFAULT ''"}, {"accounts", "health_status", "TEXT DEFAULT 'Unknown'"}, {"accounts", "traffic_api_status", "TEXT DEFAULT 'ok'"}, {"accounts", "traffic_api_message", "TEXT DEFAULT ''"}, {"accounts", "protection_suspended", "INTEGER DEFAULT 0"}, {"accounts", "protection_suspend_reason", "TEXT DEFAULT ''"}, {"accounts", "protection_suspend_notified_at", "INTEGER DEFAULT 0"}, {"accounts", "last_seen_at", "INTEGER DEFAULT 0"}, {"accounts", "missing_count", "INTEGER DEFAULT 0"}, {"accounts", "missing_since", "INTEGER DEFAULT 0"}, {"accounts", "cloud_presence", "TEXT DEFAULT 'present'"}, {"accounts", "is_deleted", "INTEGER DEFAULT 0"},
	} {
		if err := ensureColumn(s.DB, spec.table, spec.column, spec.definition); err != nil {
			return err
		}
	}
	if err := s.ensureActiveInstanceUniqueness(); err != nil {
		return err
	}
	if err := s.ResetMonthlyTraffic(); err != nil {
		return err
	}
	if err := s.migratePlaintextSecrets(); err != nil {
		return err
	}
	return nil
}

func (s *Store) ensureActiveInstanceUniqueness() error {
	// Older versions could insert the same cloud instance more than once during
	// overlapping syncs. Keep the oldest row so traffic history remains linked
	// to the visible account, then prevent the race from recurring.
	_, err := s.DB.Exec(`UPDATE accounts AS duplicate
		SET is_deleted=2,instance_status='Released',cloud_presence='retired_duplicate',access_key_secret=''
		WHERE duplicate.is_deleted=0 AND duplicate.group_key<>'' AND duplicate.instance_id<>''
		AND EXISTS (
			SELECT 1 FROM accounts AS original
			WHERE original.is_deleted=0
			AND original.group_key=duplicate.group_key
			AND original.instance_id=duplicate.instance_id
			AND original.id<duplicate.id
		)`)
	if err != nil {
		return fmt.Errorf("retire duplicate active instances: %w", err)
	}
	if _, err := s.DB.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_active_instance
		ON accounts(group_key,instance_id)
		WHERE is_deleted=0 AND group_key<>'' AND instance_id<>''`); err != nil {
		return fmt.Errorf("create active instance index: %w", err)
	}
	return nil
}

// migrateLegacyTrafficStats converts pre-account-id statistics created by the
// PHP version. The old rows are keyed by access_key_id, while the Go runtime
// needs the concrete account id so multiple instances sharing one AK remain
// independent.
func (s *Store) migrateLegacyTrafficStats() error {
	for _, table := range []string{"traffic_hourly", "traffic_daily"} {
		var legacy int
		if err := s.DB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name='access_key_id'`, table).Scan(&legacy); err != nil {
			return fmt.Errorf("inspect legacy %s schema: %w", table, err)
		}
		if legacy == 0 {
			continue
		}

		temporary := table + "_account_id_migration"
		tx, err := s.DB.Begin()
		if err != nil {
			return fmt.Errorf("begin %s migration: %w", table, err)
		}
		rollback := func(cause error) error {
			_ = tx.Rollback()
			return fmt.Errorf("migrate %s: %w", table, cause)
		}
		if _, err = tx.Exec(`DROP TABLE IF EXISTS ` + temporary); err != nil {
			return rollback(err)
		}
		if _, err = tx.Exec(`CREATE TABLE ` + temporary + ` (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id INTEGER,
			traffic REAL,
			recorded_at INTEGER
		)`); err != nil {
			return rollback(err)
		}
		query := `INSERT INTO ` + temporary + ` (account_id, traffic, recorded_at)
			SELECT a.id, MAX(t.traffic), t.recorded_at
			FROM ` + table + ` t
			JOIN accounts a ON t.access_key_id = a.access_key_id
			GROUP BY a.id, t.recorded_at`
		if _, err = tx.Exec(query); err != nil {
			return rollback(err)
		}
		if _, err = tx.Exec(`DROP TABLE ` + table); err != nil {
			return rollback(err)
		}
		if _, err = tx.Exec(`ALTER TABLE ` + temporary + ` RENAME TO ` + table); err != nil {
			return rollback(err)
		}
		if _, err = tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_` + table + `_unique ON ` + table + `(account_id, recorded_at)`); err != nil {
			return rollback(err)
		}
		if err = tx.Commit(); err != nil {
			return fmt.Errorf("commit %s migration: %w", table, err)
		}
	}
	return nil
}

func (s *Store) ResetMonthlyTraffic() error {
	month := time.Now().Format("2006-01")
	if _, err := s.DB.Exec(`UPDATE accounts SET traffic_billing_month=? WHERE traffic_billing_month IS NULL OR traffic_billing_month=''`, month); err != nil {
		return fmt.Errorf("initialize traffic billing month: %w", err)
	}
	if _, err := s.DB.Exec(`UPDATE accounts SET traffic_used=0,traffic_billing_month=?,updated_at=0,schedule_blocked_by_traffic=0 WHERE traffic_billing_month<>?`, month, month); err != nil {
		return fmt.Errorf("reset monthly traffic: %w", err)
	}
	return nil
}

func (s *Store) migratePlaintextSecrets() error {
	rows, err := s.DB.Query(`SELECT id,access_key_secret FROM accounts WHERE access_key_secret IS NOT NULL AND access_key_secret<>''`)
	if err != nil {
		return fmt.Errorf("inspect account secrets: %w", err)
	}
	type accountSecret struct {
		id     int64
		secret string
	}
	accounts := make([]accountSecret, 0)
	for rows.Next() {
		var item accountSecret
		if err := rows.Scan(&item.id, &item.secret); err != nil {
			rows.Close()
			return fmt.Errorf("read account secret: %w", err)
		}
		if !strings.HasPrefix(item.secret, "ENC1") {
			accounts = append(accounts, item)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("scan account secrets: %w", err)
	}
	rows.Close()
	for _, item := range accounts {
		sealed, err := s.Seal(item.secret)
		if err != nil {
			return err
		}
		if _, err := s.DB.Exec(`UPDATE accounts SET access_key_secret=? WHERE id=?`, sealed, item.id); err != nil {
			return fmt.Errorf("encrypt account secret: %w", err)
		}
	}

	for _, key := range []string{"notify_password", "notify_tg_token", "notify_tg_proxy_pass", "ddns_cf_token"} {
		var value string
		err := s.DB.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&value)
		if errors.Is(err, sql.ErrNoRows) || value == "" || strings.HasPrefix(value, "ENC1") {
			continue
		}
		if err != nil {
			return fmt.Errorf("read secret setting %s: %w", key, err)
		}
		sealed, sealErr := s.Seal(value)
		if sealErr != nil {
			return sealErr
		}
		if _, err := s.DB.Exec(`UPDATE settings SET value=? WHERE key=?`, sealed, key); err != nil {
			return fmt.Errorf("encrypt setting %s: %w", key, err)
		}
	}

	rows, err = s.DB.Query(`SELECT task_id,login_password,payload FROM ecs_create_tasks`)
	if err != nil {
		return fmt.Errorf("inspect ECS task secrets: %w", err)
	}
	type taskSecret struct {
		id       string
		password string
		payload  string
	}
	tasks := make([]taskSecret, 0)
	for rows.Next() {
		var item taskSecret
		if err := rows.Scan(&item.id, &item.password, &item.payload); err != nil {
			rows.Close()
			return fmt.Errorf("read ECS task secret: %w", err)
		}
		tasks = append(tasks, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("scan ECS task secrets: %w", err)
	}
	rows.Close()
	for _, item := range tasks {
		password := item.password
		if password == "" || strings.HasPrefix(password, "ENC1") {
			password = ""
		}
		var payloadMap map[string]any
		payloadChanged := false
		if item.payload != "" && json.Unmarshal([]byte(item.payload), &payloadMap) == nil {
			for _, key := range []string{"loginPassword", "login_password"} {
				if value, ok := payloadMap[key].(string); ok && value != "" {
					if password == "" {
						password = value
					}
					delete(payloadMap, key)
					payloadChanged = true
				}
			}
		}
		if password == "" && !payloadChanged {
			continue
		}
		sealed := item.password
		if password != "" {
			var sealErr error
			sealed, sealErr = s.Seal(password)
			if sealErr != nil {
				return sealErr
			}
		}
		if payloadChanged {
			raw, marshalErr := json.Marshal(payloadMap)
			if marshalErr != nil {
				return marshalErr
			}
			if _, err := s.DB.Exec(`UPDATE ecs_create_tasks SET login_password=?,payload=? WHERE task_id=?`, sealed, string(raw), item.id); err != nil {
				return fmt.Errorf("encrypt ECS task password: %w", err)
			}
		} else if password != "" {
			if _, err := s.DB.Exec(`UPDATE ecs_create_tasks SET login_password=? WHERE task_id=?`, sealed, item.id); err != nil {
				return fmt.Errorf("encrypt ECS task password: %w", err)
			}
		}
	}
	return nil
}

func ensureColumn(db *sql.DB, table, column, definition string) error {
	var found int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, table, column).Scan(&found)
	if err != nil {
		return fmt.Errorf("inspect %s.%s: %w", table, column, err)
	}
	if found != 0 {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

func (s *Store) GetSetting(key, fallback string) string {
	var value string
	if err := s.DB.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value); err != nil {
		return fallback
	}
	if value == "" || value == "<nil>" {
		return fallback
	}
	return value
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.DB.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) GetTelegramState(key, fallback string) string {
	var value string
	if err := s.DB.QueryRow(`SELECT value FROM telegram_bot_state WHERE key=?`, key).Scan(&value); err != nil {
		return fallback
	}
	return value
}

func (s *Store) SetTelegramState(key, value string) error {
	_, err := s.DB.Exec(`INSERT INTO telegram_bot_state(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) CreateTelegramActionToken(token, userID, chatID, action string, accountID int64, payload string, ttl time.Duration) error {
	now := time.Now().Unix()
	_, err := s.DB.Exec(`INSERT INTO telegram_action_tokens(token,user_id,chat_id,action,account_id,payload,expires_at,used_at,created_at) VALUES(?,?,?,?,?,?,?,0,?)`, token, userID, chatID, action, accountID, payload, time.Now().Add(ttl).Unix(), now)
	return err
}

func (s *Store) UseTelegramActionToken(token, userID, chatID string) (*TelegramActionToken, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var record TelegramActionToken
	now := time.Now().Unix()
	err = tx.QueryRow(`SELECT id,token,user_id,chat_id,action,account_id,payload,expires_at FROM telegram_action_tokens WHERE token=? AND user_id=? AND chat_id=? AND used_at=0 AND expires_at>=? LIMIT 1`, token, userID, chatID, now).Scan(&record.ID, &record.Token, &record.UserID, &record.ChatID, &record.Action, &record.AccountID, &record.Payload, &record.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE telegram_action_tokens SET used_at=? WHERE id=? AND used_at=0`, now, record.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &record, nil
}

func (s *Store) CleanupTelegramActionTokens() error {
	now := time.Now().Unix()
	_, err := s.DB.Exec(`DELETE FROM telegram_action_tokens WHERE expires_at<? OR (used_at>0 AND used_at<?)`, now-3600, now-86400)
	return err
}

func (s *Store) Settings() map[string]string {
	rows, err := s.DB.Query(`SELECT key,value FROM settings`)
	if err != nil {
		return map[string]string{}
	}
	defer rows.Close()
	result := map[string]string{}
	for rows.Next() {
		var key, value string
		if rows.Scan(&key, &value) == nil {
			result[key] = value
		}
	}
	return result
}

func (s *Store) AddLog(kind, message string) {
	_, _ = s.DB.Exec(`INSERT INTO logs(type,message,created_at) VALUES(?,?,?)`, kind, message, time.Now().Unix())
}

func (s *Store) Logs(tab string, limit int) []map[string]any {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	query := `SELECT id,type,message,created_at FROM logs WHERE type IN ('info','warning','error') ORDER BY id DESC LIMIT ?`
	args := []any{limit}
	if tab == "heartbeat" {
		query = `SELECT id,type,message,created_at FROM logs WHERE type = 'heartbeat' ORDER BY id DESC LIMIT ?`
	}
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return []map[string]any{}
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		var id, created int64
		var kind, message string
		if rows.Scan(&id, &kind, &message, &created) == nil {
			result = append(result, map[string]any{"id": id, "type": kind, "message": message, "created_at": created, "time_str": time.Unix(created, 0).Format("2006-01-02 15:04:05")})
		}
	}
	return result
}

func (s *Store) ClearLogs(tab string) error {
	if tab != "action" && tab != "heartbeat" {
		return fmt.Errorf("%w: 无效的日志类别", ErrInvalidLogQuery)
	}
	if tab == "heartbeat" {
		_, err := s.DB.Exec(`DELETE FROM logs WHERE type = 'heartbeat'`)
		return err
	}
	_, err := s.DB.Exec(`DELETE FROM logs WHERE type IN ('info','warning','error')`)
	return err
}

// PruneMaintenance removes high-frequency history without rewriting primary
// keys. Reordering SQLite ids is unsafe when other tables reference them.
func (s *Store) PruneMaintenance(now time.Time) error {
	_, err := s.DB.Exec(`DELETE FROM logs WHERE (type='heartbeat' AND created_at<?) OR (type<>'heartbeat' AND created_at<?)`, now.Add(-3*24*time.Hour).Unix(), now.Add(-30*24*time.Hour).Unix())
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`DELETE FROM traffic_hourly WHERE recorded_at<?`, now.Add(-24*time.Hour).Unix())
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`DELETE FROM traffic_daily WHERE recorded_at<?`, now.Add(-30*24*time.Hour).Unix())
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`DELETE FROM billing_cache WHERE updated_at<?`, now.Add(-90*24*time.Hour).Unix())
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`DELETE FROM instance_traffic_usage WHERE billing_month<?`, now.AddDate(0, -4, 0).Format("2006-01"))
	if err != nil {
		return err
	}
	if _, err = s.DB.Exec(`DELETE FROM sessions WHERE expires_at<?`, now.Unix()); err != nil {
		return err
	}
	if _, err = s.DB.Exec(`DELETE FROM login_attempts WHERE attempt_time<?`, now.Add(-24*time.Hour).Unix()); err != nil {
		return err
	}
	if _, err = s.DB.Exec(`DELETE FROM telegram_action_tokens WHERE expires_at<? OR (used_at>0 AND used_at<?)`, now.Unix(), now.Add(-24*time.Hour).Unix()); err != nil {
		return err
	}
	if _, err = s.DB.Exec(`DELETE FROM passkey_challenges WHERE expires_at<?`, now.Unix()); err != nil {
		return err
	}
	if _, err = s.DB.Exec(`DELETE FROM jobs WHERE status IN ('done','failed') AND updated_at<?`, now.Add(-30*24*time.Hour).Unix()); err != nil {
		return err
	}
	// Keep task status long enough for the UI, but never retain a successful
	// task's credential indefinitely if the browser never consumes it.
	if _, err = s.DB.Exec(`UPDATE ecs_create_tasks SET login_password='' WHERE status IN ('success','failed') AND updated_at<?`, now.Add(-24*time.Hour).Unix()); err != nil {
		return err
	}
	_, err = s.DB.Exec(`DELETE FROM ecs_create_tasks WHERE status IN ('success','failed') AND updated_at<?`, now.Add(-30*24*time.Hour).Unix())
	return err
}

func (s *Store) Vacuum() error {
	_, err := s.DB.Exec(`VACUUM`)
	return err
}

func (s *Store) RecentLoginFailures(ip string, window time.Duration) int {
	var count int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM login_attempts WHERE ip=? AND attempt_time>?`, ip, time.Now().Add(-window).Unix()).Scan(&count)
	return count
}

func (s *Store) RecordLoginFailure(ip string) {
	_, _ = s.DB.Exec(`INSERT INTO login_attempts(ip,attempt_time) VALUES(?,?)`, ip, time.Now().Unix())
}
func (s *Store) ClearLoginFailures(ip string) {
	_, _ = s.DB.Exec(`DELETE FROM login_attempts WHERE ip=?`, ip)
}

func (s *Store) IsInitialized() bool { return s.GetSetting("admin_password", "") != "" }

func (s *Store) SetAdminPassword(password string) error {
	if len(password) < 6 {
		return fmt.Errorf("管理员密码至少需要 6 个字符")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return s.SetSetting("admin_password", string(hash))
}

func (s *Store) CheckAdminPassword(password string) bool {
	encoded := s.GetSetting("admin_password", "")
	if encoded == "" {
		return false
	}
	if strings.HasPrefix(encoded, "$2") {
		candidate := encoded
		// PHP's bcrypt uses the $2y$ marker; Go's bcrypt verifier accepts the
		// equivalent $2a$ representation across supported versions.
		if strings.HasPrefix(candidate, "$2y$") {
			candidate = "$2a$" + strings.TrimPrefix(candidate, "$2y$")
		}
		return bcrypt.CompareHashAndPassword([]byte(candidate), []byte(password)) == nil
	}
	if strings.HasPrefix(encoded, "$argon2id$") || strings.HasPrefix(encoded, "$argon2i$") {
		if verifyPHPArgon2(encoded, password) {
			_ = s.SetAdminPassword(password)
			return true
		}
		return false
	}
	// Very old PHP deployments stored the administrator password verbatim.
	// Upgrade it immediately after a successful login.
	if subtle.ConstantTimeCompare([]byte(encoded), []byte(password)) == 1 {
		_ = s.SetAdminPassword(password)
		return true
	}
	return false
}

func verifyPHPArgon2(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || (parts[1] != "argon2id" && parts[1] != "argon2i") || parts[2] != "v=19" {
		return false
	}
	params := map[string]uint32{}
	for _, item := range strings.Split(parts[3], ",") {
		pair := strings.SplitN(item, "=", 2)
		if len(pair) != 2 {
			return false
		}
		value, err := strconv.ParseUint(pair[1], 10, 32)
		if err != nil {
			return false
		}
		params[pair[0]] = uint32(value)
	}
	if params["m"] == 0 || params["t"] == 0 || params["p"] == 0 || params["m"] > 1<<30 || params["t"] > 100 || params["p"] > 64 {
		return false
	}
	salt, ok := decodePHPCryptoBase64(parts[4])
	if !ok || len(salt) < 8 {
		return false
	}
	expected, ok := decodePHPCryptoBase64(parts[5])
	if !ok || len(expected) == 0 {
		return false
	}
	var actual []byte
	if parts[1] == "argon2id" {
		actual = argon2.IDKey([]byte(password), salt, params["t"], params["m"], uint8(params["p"]), uint32(len(expected)))
	} else {
		actual = argon2.Key([]byte(password), salt, params["t"], params["m"], uint8(params["p"]), uint32(len(expected)))
	}
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func decodePHPCryptoBase64(value string) ([]byte, bool) {
	if value == "" {
		return nil, false
	}
	padded := value + strings.Repeat("=", (4-len(value)%4)%4)
	decoded, err := base64.StdEncoding.DecodeString(padded)
	if err != nil {
		return nil, false
	}
	return decoded, true
}

func (s *Store) CreateSession(id, csrf string, ttl time.Duration) error {
	now := time.Now().Unix()
	_, err := s.DB.Exec(`INSERT INTO sessions(id,csrf_token,created_at,expires_at) VALUES(?,?,?,?)`, id, csrf, now, time.Now().Add(ttl).Unix())
	return err
}

func (s *Store) Session(id string) (csrf string, ok bool) {
	var expires int64
	err := s.DB.QueryRow(`SELECT csrf_token,expires_at FROM sessions WHERE id=?`, id).Scan(&csrf, &expires)
	if err != nil || expires < time.Now().Unix() {
		if err == nil {
			_, _ = s.DB.Exec(`DELETE FROM sessions WHERE id=?`, id)
		}
		return "", false
	}
	return csrf, true
}

func (s *Store) DeleteSession(id string) { _, _ = s.DB.Exec(`DELETE FROM sessions WHERE id=?`, id) }

func (s *Store) EnqueueJob(jobID, kind, entityKey string, payload any) error {
	if kind == "create_ecs" {
		if payloadMap, ok := payload.(map[string]any); ok {
			clone := map[string]any{}
			for k, v := range payloadMap {
				if k != "loginPassword" {
					clone[k] = v
				}
			}
			payload = clone
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	_, err = s.DB.Exec(`INSERT INTO jobs(job_id,kind,entity_key,status,payload,attempts,available_at,created_at,updated_at) VALUES(?,?,?,?,?,0,?,?,?)`, jobID, kind, entityKey, "queued", string(raw), now, now, now)
	return err
}

func (s *Store) ClaimJob(lease time.Duration) (*Job, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	var j Job
	err = tx.QueryRow(`SELECT id,job_id,kind,entity_key,status,payload,attempts,available_at FROM jobs WHERE (status IN ('queued','retry') AND available_at<=?) OR (status='running' AND locked_at<?) ORDER BY id LIMIT 1`, now, now-int64(lease.Seconds())).Scan(&j.ID, &j.JobID, &j.Kind, &j.EntityKey, &j.Status, &j.Payload, &j.Attempts, &j.AvailableAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`UPDATE jobs SET status='running',locked_at=?,attempts=attempts+1,updated_at=? WHERE id=?`, now, now, j.ID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	j.Status, j.Attempts = "running", j.Attempts+1
	return &j, nil
}

func (s *Store) FinishJob(jobID string) error {
	_, err := s.DB.Exec(`UPDATE jobs SET status='done',locked_at=0,updated_at=? WHERE job_id=?`, time.Now().Unix(), jobID)
	return err
}

func (s *Store) RetryJob(jobID string, delay time.Duration, message string) error {
	_, err := s.DB.Exec(`UPDATE jobs SET status='retry',locked_at=0,available_at=?,last_error=?,updated_at=? WHERE job_id=?`, time.Now().Add(delay).Unix(), message, time.Now().Unix(), jobID)
	return err
}

func (s *Store) FailJob(jobID, message string) error {
	_, err := s.DB.Exec(`UPDATE jobs SET status='failed',locked_at=0,last_error=?,updated_at=? WHERE job_id=?`, message, time.Now().Unix(), jobID)
	return err
}

func (s *Store) LoadAccounts(includeDeleted bool) ([]app.Account, error) {
	where := "WHERE is_deleted = 0 AND COALESCE(cloud_presence,'present') <> 'missing'"
	if includeDeleted {
		where = ""
	}
	rows, err := s.DB.Query(`SELECT id,
		COALESCE(access_key_id,''),COALESCE(access_key_secret,''),COALESCE(region_id,''),COALESCE(instance_id,''),COALESCE(max_traffic,0),
		COALESCE(schedule_enabled,0),COALESCE(schedule_start_enabled,0),COALESCE(schedule_stop_enabled,0),COALESCE(start_time,''),COALESCE(stop_time,''),
		COALESCE(traffic_used,0),COALESCE(traffic_billing_month,''),COALESCE(instance_status,'Unknown'),COALESCE(updated_at,0),COALESCE(last_keep_alive_at,0),
		COALESCE(auto_start_blocked,0),COALESCE(schedule_last_start_date,''),COALESCE(schedule_last_stop_date,''),COALESCE(schedule_stop_active,0),COALESCE(schedule_blocked_by_traffic,0),
		COALESCE(remark,''),COALESCE(site_type,'international'),COALESCE(group_key,''),COALESCE(instance_name,''),COALESCE(instance_type,''),
		COALESCE(internet_max_bandwidth_out,0),COALESCE(public_ip,''),COALESCE(public_ip_mode,'ecs_public_ip'),COALESCE(eip_allocation_id,''),COALESCE(eip_address,''),COALESCE(eip_managed,0),
		COALESCE(private_ip,''),COALESCE(cpu,0),COALESCE(memory,0),COALESCE(os_name,''),COALESCE(stopped_mode,''),COALESCE(health_status,'Unknown'),
		COALESCE(traffic_api_status,'ok'),COALESCE(traffic_api_message,''),COALESCE(protection_suspended,0),COALESCE(protection_suspend_reason,''),COALESCE(protection_suspend_notified_at,0),
		COALESCE(last_seen_at,0),COALESCE(missing_count,0),COALESCE(missing_since,0),COALESCE(cloud_presence,'present'),COALESCE(is_deleted,0)
		FROM accounts ` + where + ` ORDER BY COALESCE(region_id,''),COALESCE(remark,''),id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []app.Account{}
	for rows.Next() {
		var a app.Account
		var flags [8]int
		err := rows.Scan(&a.ID, &a.AccessKeyID, &a.AccessKeySecret, &a.RegionID, &a.InstanceID, &a.MaxTraffic, &flags[0], &flags[1], &flags[2], &a.StartTime, &a.StopTime, &a.TrafficUsed, &a.TrafficBillingMonth, &a.InstanceStatus, &a.UpdatedAt, &a.LastKeepAliveAt, &flags[3], &a.ScheduleLastStartDate, &a.ScheduleLastStopDate, &flags[4], &flags[5], &a.Remark, &a.SiteType, &a.GroupKey, &a.InstanceName, &a.InstanceType, &a.InternetBandwidth, &a.PublicIP, &a.PublicIPMode, &a.EIPAllocationID, &a.EIPAddress, &flags[6], &a.PrivateIP, &a.CPU, &a.Memory, &a.OSName, &a.StoppedMode, &a.HealthStatus, &a.TrafficAPIStatus, &a.TrafficAPIMessage, &flags[7], &a.ProtectionSuspendReason, &a.ProtectionNotifiedAt, &a.LastSeenAt, &a.MissingCount, &a.MissingSince, &a.CloudPresence, &a.IsDeleted)
		if err != nil {
			return nil, err
		}
		secret, err := s.OpenSecret(a.AccessKeySecret)
		if err != nil {
			return nil, err
		}
		a.AccessKeySecret = secret
		a.ScheduleEnabled, a.ScheduleStartEnabled, a.ScheduleStopEnabled = flags[0] != 0, flags[1] != 0, flags[2] != 0
		a.AutoStartBlocked, a.ScheduleStopActive, a.ScheduleBlockedByTraffic, a.EIPManaged, a.ProtectionSuspended = flags[3] != 0, flags[4] != 0, flags[5] != 0, flags[6] != 0, flags[7] != 0
		result = append(result, a)
	}
	return result, rows.Err()
}

func (s *Store) Account(id int64, includeDeleted bool) (*app.Account, error) {
	accounts, err := s.LoadAccounts(includeDeleted)
	if err != nil {
		return nil, err
	}
	for i := range accounts {
		if accounts[i].ID == id {
			return &accounts[i], nil
		}
	}
	return nil, sql.ErrNoRows
}

func (s *Store) UpsertAccount(a app.Account) error {
	if a.CloudPresence == "" {
		a.CloudPresence = "present"
	}
	if a.ID == 0 && a.GroupKey != "" && a.InstanceID != "" {
		var existingID int64
		err := s.DB.QueryRow(`SELECT id FROM accounts WHERE is_deleted=0 AND group_key=? AND instance_id=? LIMIT 1`, a.GroupKey, a.InstanceID).Scan(&existingID)
		if err == nil {
			a.ID = existingID
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	secret, err := s.Seal(a.AccessKeySecret)
	if err != nil {
		return err
	}
	idValue := any(a.ID)
	if a.ID == 0 {
		idValue = nil
	}
	args := []any{idValue, a.AccessKeyID, secret, a.RegionID, a.InstanceID, a.MaxTraffic, boolInt(a.ScheduleEnabled), boolInt(a.ScheduleStartEnabled), boolInt(a.ScheduleStopEnabled), a.StartTime, a.StopTime, a.TrafficUsed, a.TrafficBillingMonth, a.InstanceStatus, a.UpdatedAt, a.LastKeepAliveAt, boolInt(a.AutoStartBlocked), a.ScheduleLastStartDate, a.ScheduleLastStopDate, boolInt(a.ScheduleStopActive), boolInt(a.ScheduleBlockedByTraffic), a.Remark, a.SiteType, a.GroupKey, a.InstanceName, a.InstanceType, a.InternetBandwidth, a.PublicIP, a.PublicIPMode, a.EIPAllocationID, a.EIPAddress, boolInt(a.EIPManaged), a.PrivateIP, a.CPU, a.Memory, a.OSName, a.StoppedMode, a.HealthStatus, a.TrafficAPIStatus, a.TrafficAPIMessage, boolInt(a.ProtectionSuspended), a.ProtectionSuspendReason, a.ProtectionNotifiedAt, a.LastSeenAt, a.MissingCount, a.MissingSince, a.CloudPresence, a.IsDeleted}
	columns := "id,access_key_id,access_key_secret,region_id,instance_id,max_traffic,schedule_enabled,schedule_start_enabled,schedule_stop_enabled,start_time,stop_time,traffic_used,traffic_billing_month,instance_status,updated_at,last_keep_alive_at,auto_start_blocked,schedule_last_start_date,schedule_last_stop_date,schedule_stop_active,schedule_blocked_by_traffic,remark,site_type,group_key,instance_name,instance_type,internet_max_bandwidth_out,public_ip,public_ip_mode,eip_allocation_id,eip_address,eip_managed,private_ip,cpu,memory,os_name,stopped_mode,health_status,traffic_api_status,traffic_api_message,protection_suspended,protection_suspend_reason,protection_suspend_notified_at,last_seen_at,missing_count,missing_since,cloud_presence,is_deleted"
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	_, err = s.DB.Exec(`INSERT INTO accounts(`+columns+`) VALUES(`+placeholders+`) ON CONFLICT(id) DO UPDATE SET access_key_id=excluded.access_key_id,access_key_secret=excluded.access_key_secret,region_id=excluded.region_id,instance_id=excluded.instance_id,max_traffic=excluded.max_traffic,schedule_enabled=excluded.schedule_enabled,schedule_start_enabled=excluded.schedule_start_enabled,schedule_stop_enabled=excluded.schedule_stop_enabled,start_time=excluded.start_time,stop_time=excluded.stop_time,traffic_used=excluded.traffic_used,traffic_billing_month=excluded.traffic_billing_month,instance_status=excluded.instance_status,updated_at=excluded.updated_at,last_keep_alive_at=excluded.last_keep_alive_at,auto_start_blocked=excluded.auto_start_blocked,schedule_last_start_date=excluded.schedule_last_start_date,schedule_stop_active=excluded.schedule_stop_active,schedule_last_stop_date=excluded.schedule_last_stop_date,schedule_blocked_by_traffic=excluded.schedule_blocked_by_traffic,remark=excluded.remark,site_type=excluded.site_type,group_key=excluded.group_key,instance_name=excluded.instance_name,instance_type=excluded.instance_type,internet_max_bandwidth_out=excluded.internet_max_bandwidth_out,public_ip=excluded.public_ip,public_ip_mode=excluded.public_ip_mode,eip_allocation_id=excluded.eip_allocation_id,eip_address=excluded.eip_address,eip_managed=excluded.eip_managed,private_ip=excluded.private_ip,cpu=excluded.cpu,memory=excluded.memory,os_name=excluded.os_name,stopped_mode=excluded.stopped_mode,health_status=excluded.health_status,traffic_api_status=excluded.traffic_api_status,traffic_api_message=excluded.traffic_api_message,protection_suspended=excluded.protection_suspended,protection_suspend_reason=excluded.protection_suspend_reason,protection_suspend_notified_at=excluded.protection_suspend_notified_at,last_seen_at=excluded.last_seen_at,missing_count=excluded.missing_count,missing_since=excluded.missing_since,cloud_presence=excluded.cloud_presence,is_deleted=excluded.is_deleted`, args...)
	return err
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *Store) MarkDeleted(id int64) error {
	_, err := s.DB.Exec(`UPDATE accounts SET is_deleted=1,cloud_presence='retired_local',access_key_secret='' WHERE id=?`, id)
	return err
}
func (s *Store) MarkReleasing(id int64) error {
	_, err := s.DB.Exec(`UPDATE accounts SET instance_status='Releasing',updated_at=? WHERE id=? AND is_deleted=0`, time.Now().Unix(), id)
	return err
}
func (s *Store) PhysicallyDelete(id int64) error {
	return s.DeleteInstanceData(id)
}

// DeleteInstanceData permanently removes one local ECS instance and every
// instance-scoped record. Account-group settings live in settings and are not
// touched, so the next inventory pass can discover a newly created instance.
func (s *Store) DeleteInstanceData(id int64) error {
	var instanceID string
	err := s.DB.QueryRow(`SELECT COALESCE(instance_id,'') FROM accounts WHERE id=?`, id).Scan(&instanceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}

	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	rollback := func(cause error) error {
		_ = tx.Rollback()
		return cause
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`DELETE FROM traffic_hourly WHERE account_id=?`, []any{id}},
		{`DELETE FROM traffic_daily WHERE account_id=?`, []any{id}},
		{`DELETE FROM billing_cache WHERE account_id=?`, []any{id}},
		{`DELETE FROM instance_traffic_usage WHERE account_id=?`, []any{id}},
		{`DELETE FROM telegram_action_tokens WHERE account_id=?`, []any{id}},
		{`DELETE FROM jobs WHERE entity_key=?`, []any{strconv.FormatInt(id, 10)}},
		// Logs have no structured entity column in older databases. Remove
		// messages containing this exact instance ID to avoid retaining its
		// operational history after the instance is purged.
		{`DELETE FROM logs WHERE ?<>'' AND instr(message,?)>0`, []any{instanceID, instanceID}},
	} {
		if _, err := tx.Exec(statement.query, statement.args...); err != nil {
			return rollback(err)
		}
	}
	if instanceID != "" {
		if _, err := tx.Exec(`DELETE FROM ecs_create_tasks WHERE instance_id=?`, instanceID); err != nil {
			return rollback(err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM accounts WHERE id=?`, id); err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// PhysicallyDeleteReleaseFailed removes only a terminal failed-release row.
// The status guard prevents a concurrent retry from being hidden while a
// cloud reconciliation is cleaning up an orphaned local record.
func (s *Store) PhysicallyDeleteReleaseFailed(id int64) (bool, error) {
	var instanceID string
	err := s.DB.QueryRow(`SELECT COALESCE(instance_id,'') FROM accounts WHERE id=? AND is_deleted=0 AND instance_status='ReleaseFailed'`, id).Scan(&instanceID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := s.DeleteInstanceData(id); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ObserveAccountMissing(id int64, observedAt time.Time) (int, int64, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, 0, err
	}
	rollback := func(cause error) (int, int64, error) {
		_ = tx.Rollback()
		return 0, 0, cause
	}
	at := observedAt.Unix()
	result, err := tx.Exec(`UPDATE accounts SET
		missing_count=missing_count+1,
		missing_since=CASE WHEN missing_since=0 THEN ? ELSE missing_since END,
		cloud_presence='missing',instance_status='Missing',health_status='warning',
		traffic_api_status='unknown',traffic_api_message='云端实例不存在，等待再次确认',
		protection_suspended=1,protection_suspend_reason='instance_missing',updated_at=?
		WHERE id=? AND is_deleted=0 AND instance_status NOT IN ('Releasing','ReleaseFailed')`, at, at, id)
	if err != nil {
		return rollback(err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return rollback(err)
	}
	if changed == 0 {
		return rollback(ErrAccountNotObservable)
	}
	var count int
	var since int64
	if err := tx.QueryRow(`SELECT missing_count,missing_since FROM accounts WHERE id=?`, id).Scan(&count, &since); err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return count, since, nil
}

func (s *Store) RetireMissingAccount(id int64, missingBefore time.Time) (bool, error) {
	var matched int64
	err := s.DB.QueryRow(`SELECT id FROM accounts WHERE id=? AND is_deleted=0 AND cloud_presence='missing'
		AND missing_count>=2 AND missing_since>0 AND missing_since<=?`, id, missingBefore.Unix()).Scan(&matched)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := s.DeleteInstanceData(matched); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) UpdateAccountStatus(id int64, traffic float64, status string, updatedAt int64, metadata map[string]any) error {
	query := `UPDATE accounts SET traffic_used=?,traffic_billing_month=?,instance_status=?,updated_at=?`
	args := []any{traffic, time.Now().Format("2006-01"), status, updatedAt}
	for key, column := range map[string]string{"health_status": "health_status", "traffic_api_status": "traffic_api_status", "traffic_api_message": "traffic_api_message", "protection_suspended": "protection_suspended", "protection_suspend_reason": "protection_suspend_reason", "protection_suspend_notified_at": "protection_suspend_notified_at"} {
		if value, ok := metadata[key]; ok {
			query += "," + column + "=?"
			args = append(args, value)
		}
	}
	query += " WHERE id=?"
	args = append(args, id)
	_, err := s.DB.Exec(query, args...)
	return err
}
func (s *Store) SetInstanceStatus(id int64, status string) error {
	_, err := s.DB.Exec(`UPDATE accounts SET instance_status=?,updated_at=? WHERE id=?`, status, time.Now().Unix(), id)
	return err
}

func (s *Store) SetAutoStartBlocked(id int64, blocked bool) error {
	_, err := s.DB.Exec(`UPDATE accounts SET auto_start_blocked=? WHERE id=?`, boolInt(blocked), id)
	return err
}

func (s *Store) SetScheduleStopActive(id int64, active bool) error {
	_, err := s.DB.Exec(`UPDATE accounts SET schedule_stop_active=? WHERE id=?`, boolInt(active), id)
	return err
}

// BlockCurrentlyStoppedInstances prevents a new ECS creation from making the
// keep-alive or monthly-start automation wake up instances that were already
// intentionally stopped by the operator.
func (s *Store) BlockCurrentlyStoppedInstances() error {
	_, err := s.DB.Exec(`UPDATE accounts SET auto_start_blocked=1 WHERE instance_status='Stopped' AND is_deleted=0`)
	return err
}

func (s *Store) UpdateNetwork(id int64, fields map[string]any) error {
	allowed := map[string]bool{"public_ip": true, "public_ip_mode": true, "eip_allocation_id": true, "eip_address": true, "eip_managed": true, "internet_max_bandwidth_out": true}
	set, args := []string{}, []any{}
	for key, value := range fields {
		if allowed[key] {
			set = append(set, key+"=?")
			args = append(args, value)
		}
	}
	if len(set) == 0 {
		return nil
	}
	args = append(args, id)
	_, err := s.DB.Exec(`UPDATE accounts SET `+strings.Join(set, ",")+` WHERE id=?`, args...)
	return err
}

func (s *Store) UpdateScheduleExecutionState(id int64, action, date string) error {
	column := "schedule_last_start_date"
	if action == "stop" {
		column = "schedule_last_stop_date"
	}
	if action != "start" && action != "stop" {
		return fmt.Errorf("invalid schedule action")
	}
	_, err := s.DB.Exec(`UPDATE accounts SET `+column+`=? WHERE id=?`, date, id)
	return err
}

func (s *Store) SetGroupScheduleBlocked(groupKey string, blocked bool) error {
	_, err := s.DB.Exec(`UPDATE accounts SET schedule_blocked_by_traffic=? WHERE group_key=?`, boolInt(blocked), groupKey)
	return err
}

func (s *Store) ApplyGroupSettings(group app.AccountGroup) error {
	args := []any{group.AccessKeyID, group.RegionID, group.MaxTraffic, boolInt(group.ScheduleEnabled), boolInt(group.ScheduleStartEnabled), boolInt(group.ScheduleStopEnabled), group.StartTime, group.StopTime, group.Remark, group.SiteType, group.GroupKey, group.GroupKey, group.AccessKeyID, group.RegionID}
	query := `UPDATE accounts SET access_key_id=?,region_id=?,max_traffic=?,schedule_enabled=?,schedule_start_enabled=?,schedule_stop_enabled=?,start_time=?,stop_time=?,remark=?,site_type=?,group_key=? WHERE group_key=? OR (group_key='' AND access_key_id=? AND region_id=?)`
	if strings.TrimSpace(group.AccessKeySecret) != "" && group.AccessKeySecret != "********" {
		sealed, err := s.Seal(group.AccessKeySecret)
		if err != nil {
			return err
		}
		query = `UPDATE accounts SET access_key_id=?,access_key_secret=?,region_id=?,max_traffic=?,schedule_enabled=?,schedule_start_enabled=?,schedule_stop_enabled=?,start_time=?,stop_time=?,remark=?,site_type=?,group_key=? WHERE group_key=? OR (group_key='' AND access_key_id=? AND region_id=?)`
		args = append([]any{group.AccessKeyID, sealed}, args[1:]...)
	}
	_, err := s.DB.Exec(query, args...)
	if err != nil {
		return err
	}
	if !group.ScheduleEnabled || !group.ScheduleStopEnabled {
		_, err = s.DB.Exec(`UPDATE accounts SET schedule_stop_active=0 WHERE group_key=? OR (group_key='' AND access_key_id=? AND region_id=?)`, group.GroupKey, group.AccessKeyID, group.RegionID)
	}
	return err
}

// RemoveAccountsOutsideGroups hides local records whose account group was
// removed from configuration without touching the corresponding cloud ECS.
// Cloud resources are user-owned and must not be deleted as a side effect of
// editing the controller's monitoring configuration.
func (s *Store) RemoveAccountsOutsideGroups(groups []app.AccountGroup) ([]app.Account, error) {
	accounts, err := s.LoadAccounts(false)
	if err != nil {
		return nil, err
	}
	keep := make(map[string]bool, len(groups))
	keepComposite := make(map[string]bool, len(groups))
	for _, group := range groups {
		key := group.GroupKey
		if key == "" {
			key = derivedGroupKey(group.AccessKeyID, group.RegionID)
		}
		keep[key] = true
		keepComposite[group.AccessKeyID+"|"+group.RegionID] = true
	}
	removed := make([]app.Account, 0)
	for _, account := range accounts {
		var unfinished int
		if err := s.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind='delete_instance' AND entity_key=? AND status<>'done'`, strconv.FormatInt(account.ID, 10)).Scan(&unfinished); err != nil {
			return removed, err
		}
		if account.InstanceStatus == "Releasing" || unfinished > 0 {
			continue
		}
		key := account.GroupKey
		if key == "" {
			key = derivedGroupKey(account.AccessKeyID, account.RegionID)
		}
		if keep[key] || keepComposite[account.AccessKeyID+"|"+account.RegionID] {
			continue
		}
		if err := s.PhysicallyDelete(account.ID); err != nil {
			return removed, err
		}
		removed = append(removed, account)
	}
	return removed, nil
}

func mustSeal(s *Store, value string) string {
	sealed, err := s.Seal(value)
	if err != nil {
		return value
	}
	return sealed
}

func (s *Store) UpdateLastKeepAlive(id int64, at int64) error {
	_, err := s.DB.Exec(`UPDATE accounts SET last_keep_alive_at=? WHERE id=?`, at, id)
	return err
}

func (s *Store) SaveGroups(groups []app.AccountGroup) error {
	copyGroups := make([]app.AccountGroup, len(groups))
	copy(copyGroups, groups)
	seenAccountRegions := make(map[string]struct{}, len(copyGroups))
	for _, group := range copyGroups {
		accessKeyID := strings.TrimSpace(group.AccessKeyID)
		regionID := strings.ToLower(strings.TrimSpace(group.RegionID))
		if accessKeyID == "" || regionID == "" {
			continue
		}
		key := accessKeyID + "|" + regionID
		if _, exists := seenAccountRegions[key]; exists {
			return fmt.Errorf("账号与区域组合重复：同一账号不能重复添加相同区域")
		}
		seenAccountRegions[key] = struct{}{}
	}
	secrets := map[string]string{}
	var existingSecrets map[string]string
	_ = json.Unmarshal([]byte(s.GetSetting("account_group_secrets", "{}")), &existingSecrets)
	for i := range copyGroups {
		key := copyGroups[i].GroupKey
		if key == "" {
			key = derivedGroupKey(copyGroups[i].AccessKeyID, copyGroups[i].RegionID)
			copyGroups[i].GroupKey = key
		}
		if copyGroups[i].AccessKeySecret != "" && copyGroups[i].AccessKeySecret != "********" {
			sealed, err := s.Seal(copyGroups[i].AccessKeySecret)
			if err != nil {
				return err
			}
			secrets[key] = sealed
		} else if existingSecrets != nil && existingSecrets[key] != "" {
			secrets[key] = existingSecrets[key]
		}
		copyGroups[i].AccessKeySecret = ""
	}
	raw, err := json.Marshal(copyGroups)
	if err != nil {
		return err
	}
	secretRaw, err := json.Marshal(secrets)
	if err != nil {
		return err
	}
	if err := s.SetSetting("account_groups", string(raw)); err != nil {
		return err
	}
	return s.SetSetting("account_group_secrets", string(secretRaw))
}

func (s *Store) LoadGroups() ([]app.AccountGroup, error) {
	raw := s.GetSetting("account_groups", "[]")
	var groups []app.AccountGroup
	if err := json.Unmarshal([]byte(raw), &groups); err != nil {
		groups = nil
	}
	var secrets map[string]string
	_ = json.Unmarshal([]byte(s.GetSetting("account_group_secrets", "{}")), &secrets)
	accounts, err := s.LoadAccounts(false)
	if err != nil {
		return nil, err
	}
	blockedGroups := map[string]bool{}
	for _, account := range accounts {
		if account.ScheduleBlockedByTraffic {
			blockedGroups[account.GroupKey] = true
		}
	}
	legacyPlaintext := false
	for i := range groups {
		if groups[i].GroupKey == "" {
			groups[i].GroupKey = derivedGroupKey(groups[i].AccessKeyID, groups[i].RegionID)
		}
		if sealed := secrets[groups[i].GroupKey]; sealed != "" {
			if secret, decryptErr := s.OpenSecret(sealed); decryptErr == nil {
				groups[i].AccessKeySecret = secret
			}
		}
		if groups[i].AccessKeySecret == "" || groups[i].AccessKeySecret == "********" {
			for _, a := range accounts {
				if (groups[i].GroupKey != "" && a.GroupKey == groups[i].GroupKey) || (a.AccessKeyID == groups[i].AccessKeyID && a.RegionID == groups[i].RegionID) {
					groups[i].AccessKeySecret = a.AccessKeySecret
					break
				}
			}
		} else if !strings.HasPrefix(groups[i].AccessKeySecret, "ENC1") {
			legacyPlaintext = true
		}
		if groups[i].GroupKey == "" {
			groups[i].GroupKey = groups[i].AccessKeyID + "|" + groups[i].RegionID
		}
		if groups[i].SiteType == "" {
			groups[i].SiteType = "international"
		}
		if groups[i].MaxTraffic <= 0 {
			groups[i].MaxTraffic = 200
		}
		groups[i].ScheduleBlockedByTraffic = groups[i].ScheduleBlockedByTraffic || blockedGroups[groups[i].GroupKey]
	}
	derived := false
	if len(groups) == 0 && len(accounts) > 0 {
		groups = deriveGroupsFromAccounts(accounts)
		derived = len(groups) > 0
	}
	if legacyPlaintext || derived {
		_ = s.SaveGroups(groups)
	}
	return groups, nil
}

func derivedGroupKey(accessKeyID, regionID string) string {
	sum := sha1.Sum([]byte(strings.TrimSpace(accessKeyID) + "|" + strings.TrimSpace(regionID)))
	return fmt.Sprintf("%x", sum[:])[:16]
}

func deriveGroupsFromAccounts(accounts []app.Account) []app.AccountGroup {
	groups := make([]app.AccountGroup, 0)
	seen := map[string]bool{}
	for _, account := range accounts {
		if account.AccessKeyID == "" || account.RegionID == "" {
			continue
		}
		key := account.GroupKey
		if key == "" {
			key = derivedGroupKey(account.AccessKeyID, account.RegionID)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		siteType := account.SiteType
		if siteType == "" {
			if strings.HasPrefix(strings.ToLower(account.RegionID), "cn-") && strings.ToLower(account.RegionID) != "cn-hongkong" {
				siteType = "china"
			} else {
				siteType = "international"
			}
		}
		maxTraffic := account.MaxTraffic
		if maxTraffic <= 0 {
			maxTraffic = 200
		}
		groups = append(groups, app.AccountGroup{GroupKey: key, AccessKeyID: account.AccessKeyID, AccessKeySecret: account.AccessKeySecret, RegionID: account.RegionID, SiteType: siteType, MaxTraffic: maxTraffic, Remark: account.Remark, ScheduleEnabled: account.ScheduleEnabled, ScheduleStartEnabled: account.ScheduleStartEnabled, ScheduleStopEnabled: account.ScheduleStopEnabled, StartTime: account.StartTime, StopTime: account.StopTime, ScheduleBlockedByTraffic: account.ScheduleBlockedByTraffic})
	}
	return groups
}

func (s *Store) CreateTask(taskID, previewID, groupKey, region, instanceType string, payload any) error {
	payloadMap, _ := payload.(map[string]any)
	password := ""
	if payloadMap != nil {
		password, _ = payloadMap["loginPassword"].(string)
		if password != "" {
			clone := map[string]any{}
			for k, v := range payloadMap {
				if k != "loginPassword" {
					clone[k] = v
				}
			}
			payload = clone
		}
	}
	raw, _ := json.Marshal(payload)
	now := time.Now().Unix()
	_, err := s.DB.Exec(`INSERT INTO ecs_create_tasks(task_id,preview_id,account_group_key,region_id,instance_type,status,step,payload,login_password,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, taskID, previewID, groupKey, region, instanceType, "queued", "等待后台 worker", string(raw), mustSeal(s, password), now, now)
	return err
}

func (s *Store) UpdateTask(taskID string, fields map[string]any) error {
	allowed := map[string]bool{"status": true, "step": true, "error_message": true, "instance_id": true, "public_ip": true, "login_user": true, "login_password": true, "payload": true, "zone_id": true, "image_id": true, "os_label": true, "instance_name": true, "vpc_id": true, "vswitch_id": true, "security_group_id": true, "internet_max_bandwidth_out": true, "system_disk_category": true, "system_disk_size": true, "public_ip_mode": true, "eip_allocation_id": true, "eip_address": true, "eip_managed": true}
	set, args := []string{}, []any{}
	for key, value := range fields {
		if allowed[key] {
			if key == "login_password" {
				if plain, ok := value.(string); ok {
					value = mustSeal(s, plain)
				}
			}
			if raw, ok := value.(map[string]any); ok {
				value, _ = json.Marshal(raw)
			}
			set = append(set, key+"=?")
			args = append(args, value)
		}
	}
	if len(set) == 0 {
		return nil
	}
	set = append(set, "updated_at=?")
	args = append(args, time.Now().Unix(), taskID)
	_, err := s.DB.Exec(`UPDATE ecs_create_tasks SET `+strings.Join(set, ",")+` WHERE task_id=?`, args...)
	return err
}

// GetTaskForWorker returns the decrypted task credential only to the internal
// create worker. HTTP callers must use GetTask/ConsumeTaskPassword instead.
func (s *Store) GetTaskForWorker(taskID string) (*app.EcsTask, error) {
	var t app.EcsTask
	var created, updated int64
	var raw string
	err := s.DB.QueryRow(`SELECT task_id,preview_id,account_group_key,region_id,instance_type,status,step,error_message,instance_id,public_ip,login_user,login_password,payload,created_at,updated_at FROM ecs_create_tasks WHERE task_id=?`, taskID).Scan(&t.TaskID, &t.PreviewID, &t.GroupKey, &t.RegionID, &t.InstanceType, &t.Status, &t.Step, &t.ErrorMessage, &t.InstanceID, &t.PublicIP, &t.LoginUser, &t.LoginPassword, &raw, &created, &updated)
	if err != nil {
		return nil, err
	}
	legacyPassword := t.LoginPassword
	if t.LoginPassword, err = s.OpenSecret(t.LoginPassword); err != nil {
		return nil, err
	}
	if legacyPassword != "" && !strings.HasPrefix(legacyPassword, "ENC1") {
		_ = s.UpdateTask(taskID, map[string]any{"login_password": t.LoginPassword})
	}
	_ = json.Unmarshal([]byte(raw), &t.Payload)
	t.CreatedAt, t.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return &t, nil
}

// GetTask deliberately never exposes the ECS login password. A successful
// client poll must call ConsumeTaskPassword to receive it once.
func (s *Store) GetTask(taskID string) (*app.EcsTask, error) {
	task, err := s.GetTaskForWorker(taskID)
	if err != nil {
		return nil, err
	}
	task.LoginPassword = ""
	return task, nil
}

// ConsumeTaskPassword atomically clears and returns the password for a
// successful create task. A second browser/tab receives an empty password.
func (s *Store) ConsumeTaskPassword(taskID string) (*app.EcsTask, error) {
	task, err := s.GetTaskForWorker(taskID)
	if err != nil {
		return nil, err
	}
	if task.Status != "success" || task.LoginPassword == "" {
		task.LoginPassword = ""
		return task, nil
	}
	var sealed string
	if err := s.DB.QueryRow(`SELECT login_password FROM ecs_create_tasks WHERE task_id=? AND status='success'`, taskID).Scan(&sealed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			task.LoginPassword = ""
			return task, nil
		}
		return nil, err
	}
	result, err := s.DB.Exec(`UPDATE ecs_create_tasks SET login_password='',updated_at=? WHERE task_id=? AND status='success' AND login_password=?`, time.Now().Unix(), taskID, sealed)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed == 0 {
		task.LoginPassword = ""
	}
	return task, nil
}

func (s *Store) LastRun() int64 {
	value, _ := strconv.ParseInt(s.GetSetting("last_monitor_run", "0"), 10, 64)
	return value
}
func (s *Store) SetLastRun() {
	_ = s.SetSetting("last_monitor_run", strconv.FormatInt(time.Now().Unix(), 10))
}

func (s *Store) InstanceTrafficUsage(accountID int64, instanceID, month string) (TrafficSample, error) {
	var sample TrafficSample
	err := s.DB.QueryRow(`SELECT traffic_bytes,last_sample_ms FROM instance_traffic_usage WHERE account_id=? AND instance_id=? AND billing_month=?`, accountID, instanceID, month).Scan(&sample.TrafficBytes, &sample.LastSampleMS)
	if errors.Is(err, sql.ErrNoRows) {
		return TrafficSample{}, nil
	}
	return sample, err
}

func (s *Store) AddInstanceTraffic(accountID int64, instanceID, month string, deltaBytes float64, lastSampleMS int64) (TrafficSample, error) {
	now := time.Now().Unix()
	_, err := s.DB.Exec(`INSERT INTO instance_traffic_usage(account_id,instance_id,billing_month,traffic_bytes,last_sample_ms,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(account_id,instance_id,billing_month) DO UPDATE SET traffic_bytes=instance_traffic_usage.traffic_bytes+excluded.traffic_bytes,last_sample_ms=MAX(instance_traffic_usage.last_sample_ms,excluded.last_sample_ms),updated_at=excluded.updated_at`, accountID, instanceID, month, deltaBytes, lastSampleMS, now)
	if err != nil {
		return TrafficSample{}, err
	}
	return s.InstanceTrafficUsage(accountID, instanceID, month)
}

// SetInstanceTraffic stores an absolute monthly CMS total. This lets the
// monitor recover after an instance was stopped or the process was offline
// without losing the traffic accumulated between two polling windows.
func (s *Store) SetInstanceTraffic(accountID int64, instanceID, month string, trafficBytes float64, lastSampleMS int64) (TrafficSample, error) {
	now := time.Now().Unix()
	_, err := s.DB.Exec(`INSERT INTO instance_traffic_usage(account_id,instance_id,billing_month,traffic_bytes,last_sample_ms,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(account_id,instance_id,billing_month) DO UPDATE SET traffic_bytes=excluded.traffic_bytes,last_sample_ms=MAX(instance_traffic_usage.last_sample_ms,excluded.last_sample_ms),updated_at=excluded.updated_at`, accountID, instanceID, month, trafficBytes, lastSampleMS, now)
	if err != nil {
		return TrafficSample{}, err
	}
	return s.InstanceTrafficUsage(accountID, instanceID, month)
}

func (s *Store) AddTrafficHistory(accountID int64, traffic float64, at time.Time) error {
	hour := at.Truncate(time.Hour).Unix()
	day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location()).Unix()
	if _, err := s.DB.Exec(`INSERT INTO traffic_hourly(account_id,traffic,recorded_at) VALUES(?,?,?) ON CONFLICT(account_id,recorded_at) DO UPDATE SET traffic=excluded.traffic`, accountID, traffic, hour); err != nil {
		return err
	}
	_, err := s.DB.Exec(`INSERT INTO traffic_daily(account_id,traffic,recorded_at) VALUES(?,?,?) ON CONFLICT(account_id,recorded_at) DO UPDATE SET traffic=excluded.traffic`, accountID, traffic, day)
	return err
}

// DailyTrafficDelta returns the change in the cumulative CMS traffic value
// between the last samples on the requested day and the preceding day.
// Missing samples and counter resets are reported as incomplete instead of
// being mistaken for zero traffic.
func (s *Store) DailyTrafficDelta(accountID int64, day time.Time) (float64, bool, error) {
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location())
	end := start.AddDate(0, 0, 1)
	var current, previous float64
	if err := s.DB.QueryRow(`SELECT traffic FROM traffic_daily WHERE account_id=? AND recorded_at>=? AND recorded_at<? ORDER BY recorded_at DESC LIMIT 1`, accountID, start.Unix(), end.Unix()).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if err := s.DB.QueryRow(`SELECT traffic FROM traffic_daily WHERE account_id=? AND recorded_at<? ORDER BY recorded_at DESC LIMIT 1`, accountID, start.Unix()).Scan(&previous); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	delta := current - previous
	if delta < 0 {
		return 0, false, nil
	}
	return delta, true, nil
}

func (s *Store) AccountHistory(accountID int64) (map[string]any, error) {
	result := map[string]any{"history_24h": []map[string]any{}, "history_30d": []map[string]any{}}
	rows, err := s.DB.Query(`SELECT traffic,recorded_at FROM traffic_hourly WHERE account_id=? AND recorded_at>=? ORDER BY recorded_at`, accountID, time.Now().Add(-24*time.Hour).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hourly := []map[string]any{}
	for rows.Next() {
		var traffic float64
		var recorded int64
		if err := rows.Scan(&traffic, &recorded); err != nil {
			return nil, err
		}
		t := time.Unix(recorded, 0)
		hourly = append(hourly, map[string]any{"time": t.Format("15:04"), "full_time": t.Format("2006-01-02 15:04"), "value": traffic})
	}
	result["history_24h"] = hourly
	rows, err = s.DB.Query(`SELECT traffic,recorded_at FROM traffic_daily WHERE account_id=? AND recorded_at>=? ORDER BY recorded_at`, accountID, time.Now().Add(-30*24*time.Hour).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	daily := []map[string]any{}
	for rows.Next() {
		var traffic float64
		var recorded int64
		if err := rows.Scan(&traffic, &recorded); err != nil {
			return nil, err
		}
		daily = append(daily, map[string]any{"date": time.Unix(recorded, 0).Format("2006-01-02"), "value": traffic})
	}
	result["history_30d"] = daily
	return result, rows.Err()
}

func (s *Store) GetBillingCache(accountID int64, cacheType, cycle string, maxAge time.Duration) (map[string]any, bool) {
	var raw string
	var updated int64
	err := s.DB.QueryRow(`SELECT data,updated_at FROM billing_cache WHERE account_id=? AND cache_type=? AND billing_cycle=?`, accountID, cacheType, cycle).Scan(&raw, &updated)
	if err != nil || time.Now().Unix()-updated > int64(maxAge.Seconds()) {
		return nil, false
	}
	var data map[string]any
	if json.Unmarshal([]byte(raw), &data) != nil {
		return nil, false
	}
	if errorMessage, ok := data["error"].(string); ok && strings.TrimSpace(errorMessage) != "" {
		// Error entries should be retried after a code/configuration fix instead
		// of hiding the failure for the full cache TTL.
		return nil, false
	}
	return data, true
}

func (s *Store) SetBillingCache(accountID int64, cacheType, cycle string, data map[string]any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`INSERT INTO billing_cache(account_id,cache_type,billing_cycle,data,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(account_id,cache_type,billing_cycle) DO UPDATE SET data=excluded.data,updated_at=excluded.updated_at`, accountID, cacheType, cycle, string(raw), time.Now().Unix())
	return err
}
