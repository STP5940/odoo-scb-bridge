package database

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"odoo-scb-bridge/internal/models"

	_ "modernc.org/sqlite"
)

type DB struct {
	conn    *sql.DB
	dataDir string
	mu      sync.RWMutex
}

var (
	instance *DB
	once     sync.Once
)

// Init initializes the SQLite database at dbPath and runs migrations
func Init(dbPath string) (*DB, error) {
	var err error
	once.Do(func() {
		// Ensure parent directory exists
		dbPath, err = filepath.Abs(dbPath)
		if err != nil {
			return
		}
		dir := filepath.Dir(dbPath)
		if err = os.MkdirAll(dir, 0755); err != nil {
			return
		}

		// Connect to SQLite with WAL mode enabled for concurrent read/write
		dsn := fmt.Sprintf("%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)", dbPath)
		var conn *sql.DB
		conn, err = sql.Open("sqlite", dsn)
		if err != nil {
			return
		}

		conn.SetMaxOpenConns(1) // SQLite works best with 1 open connection for writes

		d := &DB{conn: conn, dataDir: dir}
		if err = d.migrate(); err != nil {
			return
		}

		instance = d
	})

	if err != nil {
		return nil, fmt.Errorf("failed to init database: %w", err)
	}
	return instance, nil
}

// Get returns the singleton DB instance
func Get() *DB {
	return instance
}

// migrate creates tables if they don't exist and seeds defaults
func (d *DB) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT UNIQUE NOT NULL,
		password TEXT NOT NULL,
		root_dir TEXT NOT NULL,
		enabled INTEGER DEFAULT 1,
		can_list INTEGER NOT NULL DEFAULT 1,
		can_read INTEGER NOT NULL DEFAULT 1,
		can_write INTEGER NOT NULL DEFAULT 1,
		can_delete INTEGER NOT NULL DEFAULT 1,
		can_mkdir INTEGER NOT NULL DEFAULT 1,
		can_rename INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS inbound_configs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		protocol TEXT DEFAULT 'sftp',
		sftp_port INTEGER DEFAULT 2222,
		ftp_port INTEGER DEFAULT 2121,
		target_dir TEXT NOT NULL,
		temp_dir TEXT NOT NULL,
		host_key_path TEXT DEFAULT '',
		tls_cert_path TEXT DEFAULT '',
		tls_key_path TEXT DEFAULT '',
		auto_move_enabled INTEGER DEFAULT 1,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS outbound_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		cron_expr TEXT NOT NULL,
		source_user_id INTEGER NOT NULL DEFAULT 0,
		source_dir TEXT NOT NULL,
		file_pattern TEXT DEFAULT '*.*',
		protocol TEXT DEFAULT 'sftp',
		remote_host TEXT NOT NULL,
		remote_port INTEGER DEFAULT 22,
		remote_user TEXT NOT NULL,
		remote_password TEXT NOT NULL,
		remote_dir TEXT NOT NULL,
		post_action TEXT DEFAULT 'archive',
		archive_dir TEXT DEFAULT '',
		enabled INTEGER DEFAULT 1,
		last_run_at DATETIME,
		last_status TEXT DEFAULT 'IDLE',
		last_error TEXT DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS audit_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		event_type TEXT NOT NULL,
		protocol TEXT NOT NULL,
		username TEXT DEFAULT '',
		client_ip TEXT DEFAULT '',
		file_name TEXT DEFAULT '',
		file_size INTEGER DEFAULT 0,
		file_hash TEXT DEFAULT '',
		status TEXT NOT NULL,
		details TEXT DEFAULT '',
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
		job_id INTEGER NOT NULL DEFAULT 0
	);

	CREATE TABLE IF NOT EXISTS app_metadata (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS sftp_security_settings (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		ip_mode TEXT NOT NULL DEFAULT 'allow_all',
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	INSERT OR IGNORE INTO sftp_security_settings (id, ip_mode) VALUES (1, 'allow_all');

	CREATE TABLE IF NOT EXISTS sftp_ip_rules (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		cidr TEXT NOT NULL,
		action TEXT NOT NULL CHECK (action IN ('allow', 'block')),
		expires_at DATETIME,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(cidr, action)
	);

	CREATE TABLE IF NOT EXISTS sftp_ip_failures (
		ip TEXT PRIMARY KEY,
		username TEXT NOT NULL DEFAULT '',
		failed_attempts INTEGER NOT NULL DEFAULT 0,
		window_started_at DATETIME,
		blocked_until DATETIME,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_audit_logs_event_type ON audit_logs(event_type);
	CREATE INDEX IF NOT EXISTS idx_audit_logs_timestamp ON audit_logs(timestamp);
	CREATE INDEX IF NOT EXISTS idx_audit_logs_username ON audit_logs(username);
	CREATE INDEX IF NOT EXISTS idx_sftp_ip_rules_cidr ON sftp_ip_rules(cidr);
	CREATE INDEX IF NOT EXISTS idx_sftp_ip_failures_blocked_until ON sftp_ip_failures(blocked_until);
	`

	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.conn.Exec(schema)
	if err != nil {
		return fmt.Errorf("migration error: %w", err)
	}
	if err := ensureOutboundSourceUserColumn(d.conn); err != nil {
		return fmt.Errorf("migrate outbound source user: %w", err)
	}
	if err := ensureAuditLogJobIDColumn(d.conn); err != nil {
		return fmt.Errorf("migrate audit log job id: %w", err)
	}
	if err := backfillOutboundLogJobIDs(d.conn); err != nil {
		return fmt.Errorf("associate existing outbound logs with jobs: %w", err)
	}
	// Existing accounts retain their previous unrestricted behavior until an
	// administrator reviews their individual permissions in the UI.
	for _, column := range []string{"can_list", "can_read", "can_write", "can_delete", "can_mkdir", "can_rename"} {
		if err := ensureUserPermissionColumn(d.conn, column); err != nil {
			return fmt.Errorf("migrate user permission %s: %w", column, err)
		}
	}
	if _, err := d.conn.Exec(`UPDATE users SET root_dir='users/' || id`); err != nil {
		return fmt.Errorf("migrate user home directories: %w", err)
	}

	// Seed default inbound config if not present
	var count int
	err = d.conn.QueryRow("SELECT COUNT(*) FROM inbound_configs").Scan(&count)
	if err == nil && count == 0 {
		targetDir := filepath.Join(d.dataDir, "inbound")
		tempDir := filepath.Join(d.dataDir, "temp")
		os.MkdirAll(targetDir, 0755)
		os.MkdirAll(tempDir, 0755)

		_, _ = d.conn.Exec(`
			INSERT INTO inbound_configs (protocol, sftp_port, ftp_port, target_dir, temp_dir, auto_move_enabled)
			VALUES ('sftp', 2222, 2121, ?, ?, 1)
		`, targetDir, tempDir)

		// Seed initial audit log event
		_, _ = d.conn.Exec(`
			INSERT INTO audit_logs (event_type, protocol, username, client_ip, file_name, status, details, timestamp)
			VALUES ('SYSTEM', 'INIT', 'admin', '127.0.0.1', 'system_init.db', 'SUCCESS', 'Database and service initialized', CURRENT_TIMESTAMP)
		`)
	}

	return d.repairWorkingDirectoryInboundPaths()
}

// repairWorkingDirectoryInboundPaths fixes defaults created by older versions
// while running as a Windows service, whose working directory is System32.
// Customized paths are preserved unless they exactly match that old default.
func (d *DB) repairWorkingDirectoryInboundPaths() error {
	workingDir, err := os.Getwd()
	if err != nil {
		return err
	}
	legacyTarget := filepath.Join(workingDir, "data", "inbound")
	legacyTemp := filepath.Join(workingDir, "data", "temp")
	correctTarget := filepath.Join(d.dataDir, "inbound")
	correctTemp := filepath.Join(d.dataDir, "temp")

	var id int64
	var targetDir, tempDir string
	err = d.conn.QueryRow("SELECT id, target_dir, temp_dir FROM inbound_configs LIMIT 1").Scan(&id, &targetDir, &tempDir)
	if err != nil {
		return err
	}
	changed := false
	if sameFilesystemPath(targetDir, legacyTarget) && !sameFilesystemPath(targetDir, correctTarget) {
		targetDir = correctTarget
		if err := os.MkdirAll(targetDir, 0755); err != nil {
			return fmt.Errorf("create inbound target folder: %w", err)
		}
		changed = true
	}
	if sameFilesystemPath(tempDir, legacyTemp) && !sameFilesystemPath(tempDir, correctTemp) {
		tempDir = correctTemp
		if err := os.MkdirAll(tempDir, 0755); err != nil {
			return fmt.Errorf("create inbound temp folder: %w", err)
		}
		changed = true
	}
	if !changed {
		return d.ensureUserHomeDirectories(targetDir)
	}
	if _, err = d.conn.Exec("UPDATE inbound_configs SET target_dir=?, temp_dir=? WHERE id=?", targetDir, tempDir, id); err != nil {
		return err
	}
	return d.ensureUserHomeDirectories(targetDir)
}

func (d *DB) ensureUserHomeDirectories(targetDir string) error {
	rows, err := d.conn.Query("SELECT root_dir FROM users")
	if err != nil {
		return err
	}
	var directories []string
	for rows.Next() {
		var rootDir string
		if err := rows.Scan(&rootDir); err != nil {
			rows.Close()
			return err
		}
		relative := filepath.Clean(filepath.FromSlash(rootDir))
		if relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			rows.Close()
			return fmt.Errorf("invalid SFTP user home path %q", rootDir)
		}
		directories = append(directories, filepath.Join(targetDir, relative))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, directory := range directories {
		if err := os.MkdirAll(directory, 0750); err != nil {
			return fmt.Errorf("create SFTP user home folder: %w", err)
		}
	}
	return nil
}

func sameFilesystemPath(first, second string) bool {
	first = filepath.Clean(first)
	second = filepath.Clean(second)
	if os.PathSeparator == '\\' {
		return strings.EqualFold(first, second)
	}
	return first == second
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func ensureUserPermissionColumn(conn *sql.DB, column string) error {
	rows, err := conn.Query("PRAGMA table_info(users)")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, columnType string
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &columnType, &notnull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = conn.Exec("ALTER TABLE users ADD COLUMN " + column + " INTEGER NOT NULL DEFAULT 1")
	return err
}

func ensureOutboundSourceUserColumn(conn *sql.DB) error {
	rows, err := conn.Query("PRAGMA table_info(outbound_jobs)")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, columnType string
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &columnType, &notnull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == "source_user_id" {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = conn.Exec("ALTER TABLE outbound_jobs ADD COLUMN source_user_id INTEGER NOT NULL DEFAULT 0")
	return err
}

func ensureAuditLogJobIDColumn(conn *sql.DB) error {
	rows, err := conn.Query("PRAGMA table_info(audit_logs)")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, columnType string
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &columnType, &notnull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == "job_id" {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = conn.Exec("ALTER TABLE audit_logs ADD COLUMN job_id INTEGER NOT NULL DEFAULT 0")
	return err
}

func backfillOutboundLogJobIDs(conn *sql.DB) error {
	rows, err := conn.Query(`SELECT id, protocol, remote_user, remote_host, remote_port, remote_dir FROM outbound_jobs`)
	if err != nil {
		return err
	}
	type outboundLogKey struct {
		protocol, username, host, remoteDir string
		port                                int
	}
	type outboundLogJob struct {
		id  int64
		key outboundLogKey
	}
	var jobs []outboundLogJob
	counts := make(map[outboundLogKey]int)
	for rows.Next() {
		var job outboundLogJob
		if err := rows.Scan(&job.id, &job.key.protocol, &job.key.username, &job.key.host, &job.key.port, &job.key.remoteDir); err != nil {
			rows.Close()
			return err
		}
		job.key.protocol = strings.ToLower(job.key.protocol)
		jobs = append(jobs, job)
		counts[job.key]++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, job := range jobs {
		if counts[job.key] != 1 {
			continue
		}
		prefix := fmt.Sprintf("Pushed to %s:%d/%s", job.key.host, job.key.port, job.key.remoteDir)
		if _, err := conn.Exec(`UPDATE audit_logs SET job_id=? WHERE job_id=0 AND event_type='OUTBOUND_FILE' AND LOWER(protocol)=LOWER(?) AND username=? AND substr(details, 1, length(?))=?`, job.id, job.key.protocol, job.key.username, prefix, prefix); err != nil {
			return err
		}
	}
	return nil
}

// LogAudit inserts a new event record
func (d *DB) LogAudit(log models.AuditLog) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.internalLogAudit(log)
}

// RecordAppVersion logs the initial version or a version change once per version.
func (d *DB) RecordAppVersion(version string) error {
	return d.RecordAppVersionAt(version, time.Time{})
}

// RecordAppVersionAt uses installedAt for the audit timestamp when it is available.
func (d *DB) RecordAppVersionAt(version string, installedAt time.Time) error {
	if version == "" {
		return fmt.Errorf("application version is empty")
	}
	if installedAt.IsZero() {
		installedAt = time.Now()
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var previous string
	err = tx.QueryRow(`SELECT value FROM app_metadata WHERE key='app_version'`).Scan(&previous)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil && previous == version {
		return tx.Commit()
	}

	details := "Current application version: " + version
	if previous != "" {
		details = "Application version changed from " + previous + " to " + version
	}
	now := time.Now()
	if _, err := tx.Exec(`INSERT INTO app_metadata (key, value, updated_at) VALUES ('app_version', ?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, version, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO audit_logs (event_type, protocol, username, client_ip, file_name, file_size, file_hash, status, details, timestamp)
		VALUES ('APP_VERSION', 'SYSTEM', 'system', '', '', 0, '', 'SUCCESS', ?, ?)`, details, installedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) internalLogAudit(log models.AuditLog) error {
	query := `
		INSERT INTO audit_logs (event_type, protocol, username, client_ip, file_name, file_size, file_hash, status, details, timestamp, job_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	ts := log.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	_, err := d.conn.Exec(query, log.EventType, log.Protocol, log.Username, log.ClientIP, log.FileName, log.FileSize, log.FileHash, log.Status, log.Details, ts, log.JobID)
	return err
}

// GetRecentLogs retrieves a page of audit logs with an optional event filter.
func (d *DB) GetRecentLogs(limit int, eventType string, offsets ...int) ([]models.AuditLog, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	offset := 0
	if len(offsets) > 0 && offsets[0] > 0 {
		offset = offsets[0]
	}

	var rows *sql.Rows
	var err error

	if eventType != "" {
		rows, err = d.conn.Query("SELECT id, event_type, protocol, username, client_ip, file_name, file_size, file_hash, status, details, timestamp, job_id FROM audit_logs WHERE event_type = ? ORDER BY id DESC LIMIT ? OFFSET ?", eventType, limit, offset)
	} else {
		rows, err = d.conn.Query("SELECT id, event_type, protocol, username, client_ip, file_name, file_size, file_hash, status, details, timestamp, job_id FROM audit_logs ORDER BY id DESC LIMIT ? OFFSET ?", limit, offset)
	}

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	logs := make([]models.AuditLog, 0)
	for rows.Next() {
		var l models.AuditLog
		if err := rows.Scan(&l.ID, &l.EventType, &l.Protocol, &l.Username, &l.ClientIP, &l.FileName, &l.FileSize, &l.FileHash, &l.Status, &l.Details, &l.Timestamp, &l.JobID); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, nil
}

// CountLogs returns the number of audit logs matching an optional event filter.
func (d *DB) CountLogs(eventType string) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var count int
	var err error
	if eventType != "" {
		err = d.conn.QueryRow("SELECT COUNT(*) FROM audit_logs WHERE event_type = ?", eventType).Scan(&count)
	} else {
		err = d.conn.QueryRow("SELECT COUNT(*) FROM audit_logs").Scan(&count)
	}
	return count, err
}

// GetRecentLogsFiltered retrieves audit logs filtered by event type and exact username.
func (d *DB) GetRecentLogsFiltered(limit int, eventType, username string, offset int) ([]models.AuditLog, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	query := `SELECT id, event_type, protocol, username, client_ip, file_name, file_size, file_hash, status, details, timestamp, job_id FROM audit_logs WHERE 1=1`
	args := make([]interface{}, 0, 4)
	if eventType != "" {
		query += ` AND event_type = ?`
		args = append(args, eventType)
	}
	if username != "" {
		query += ` AND username = ?`
		args = append(args, username)
	}
	query += ` ORDER BY id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := d.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	logs := make([]models.AuditLog, 0)
	for rows.Next() {
		var item models.AuditLog
		if err := rows.Scan(&item.ID, &item.EventType, &item.Protocol, &item.Username, &item.ClientIP, &item.FileName, &item.FileSize, &item.FileHash, &item.Status, &item.Details, &item.Timestamp, &item.JobID); err != nil {
			return nil, err
		}
		logs = append(logs, item)
	}
	return logs, rows.Err()
}

func (d *DB) CountLogsFiltered(eventType, username string) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	query := `SELECT COUNT(*) FROM audit_logs WHERE 1=1`
	args := make([]interface{}, 0, 3)
	if eventType != "" {
		query += ` AND event_type = ?`
		args = append(args, eventType)
	}
	if username != "" {
		query += ` AND username = ?`
		args = append(args, username)
	}
	var count int
	err := d.conn.QueryRow(query, args...).Scan(&count)
	return count, err
}

// GetOutboundJobLogs returns transfer logs linked to one outbound job only.
func (d *DB) GetOutboundJobLogs(jobID int64, limit, offset int) ([]models.AuditLog, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	rows, err := d.conn.Query(`SELECT id, event_type, protocol, username, client_ip, file_name, file_size, file_hash, status, details, timestamp, job_id FROM audit_logs WHERE job_id = ? AND event_type = 'OUTBOUND_FILE' ORDER BY id DESC LIMIT ? OFFSET ?`, jobID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	logs := make([]models.AuditLog, 0)
	for rows.Next() {
		var item models.AuditLog
		if err := rows.Scan(&item.ID, &item.EventType, &item.Protocol, &item.Username, &item.ClientIP, &item.FileName, &item.FileSize, &item.FileHash, &item.Status, &item.Details, &item.Timestamp, &item.JobID); err != nil {
			return nil, err
		}
		logs = append(logs, item)
	}
	return logs, rows.Err()
}

func (d *DB) CountOutboundJobLogs(jobID int64) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var count int
	err := d.conn.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE job_id = ? AND event_type = 'OUTBOUND_FILE'`, jobID).Scan(&count)
	return count, err
}

func (d *DB) GetSFTPAccessControl() (models.SFTPSecuritySettings, []models.SFTPIPRule, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	settings := models.SFTPSecuritySettings{}
	if err := d.conn.QueryRow(`SELECT ip_mode FROM sftp_security_settings WHERE id=1`).Scan(&settings.IPMode); err != nil {
		return settings, nil, err
	}
	rows, err := d.conn.Query(`SELECT id, cidr, action, expires_at, created_at FROM sftp_ip_rules WHERE expires_at IS NULL OR expires_at > ? ORDER BY id DESC`, time.Now())
	if err != nil {
		return settings, nil, err
	}
	defer rows.Close()
	rules := make([]models.SFTPIPRule, 0)
	for rows.Next() {
		var rule models.SFTPIPRule
		var expires sql.NullTime
		if err := rows.Scan(&rule.ID, &rule.CIDR, &rule.Action, &expires, &rule.CreatedAt); err != nil {
			return settings, nil, err
		}
		rule.Permanent = !expires.Valid
		if expires.Valid {
			t := expires.Time
			rule.ExpiresAt = &t
		}
		rules = append(rules, rule)
	}
	return settings, rules, rows.Err()
}

func (d *DB) SetSFTPIPMode(mode string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.conn.Exec(`UPDATE sftp_security_settings SET ip_mode=?, updated_at=CURRENT_TIMESTAMP WHERE id=1`, mode)
	return err
}

func (d *DB) AddSFTPIPRule(cidr, action string, expiresAt *time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.conn.Exec(`INSERT INTO sftp_ip_rules (cidr, action, expires_at) VALUES (?, ?, ?)`, cidr, action, expiresAt)
	return err
}

func (d *DB) DeleteSFTPIPRule(id int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	result, err := d.conn.Exec(`DELETE FROM sftp_ip_rules WHERE id=?`, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		return sql.ErrNoRows
	}
	return err
}

func (d *DB) GetSFTPIPBlockedUntil(ip string) (time.Time, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var blockedUntil sql.NullTime
	err := d.conn.QueryRow(`SELECT blocked_until FROM sftp_ip_failures WHERE ip=?`, ip).Scan(&blockedUntil)
	if err == sql.ErrNoRows || (err == nil && !blockedUntil.Valid) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return blockedUntil.Time, err
}

// RecordSFTPAuthFailure applies a per-IP threshold: five failures in fifteen minutes block the IP for fifteen minutes.
func (d *DB) RecordSFTPAuthFailure(ip, username string, now time.Time) (bool, bool, int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var failures int
	var windowStarted, blockedUntil sql.NullTime
	err := d.conn.QueryRow(`SELECT failed_attempts, window_started_at, blocked_until FROM sftp_ip_failures WHERE ip=?`, ip).Scan(&failures, &windowStarted, &blockedUntil)
	if err != nil && err != sql.ErrNoRows {
		return false, false, 0, err
	}
	if blockedUntil.Valid && blockedUntil.Time.After(now) {
		return true, false, int(blockedUntil.Time.Sub(now).Seconds() + 0.999), nil
	}
	if !windowStarted.Valid || now.Sub(windowStarted.Time) > 15*time.Minute || blockedUntil.Valid {
		failures = 0
		windowStarted = sql.NullTime{Time: now, Valid: true}
	}
	failures++
	var nextBlock interface{}
	blocked := failures >= 5
	if blocked {
		until := now.Add(15 * time.Minute)
		nextBlock = until
	}
	_, err = d.conn.Exec(`INSERT INTO sftp_ip_failures (ip, username, failed_attempts, window_started_at, blocked_until, updated_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(ip) DO UPDATE SET username=excluded.username, failed_attempts=excluded.failed_attempts,
		window_started_at=excluded.window_started_at, blocked_until=excluded.blocked_until, updated_at=excluded.updated_at`,
		ip, username, failures, windowStarted.Time, nextBlock, now)
	if err != nil {
		return false, false, 0, err
	}
	if blocked {
		return true, true, 15 * 60, nil
	}
	return false, false, 5 - failures, nil
}

func (d *DB) ResetSFTPAuthFailures(ip string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.conn.Exec(`DELETE FROM sftp_ip_failures WHERE ip=?`, ip)
	return err
}

func (d *DB) ListSFTPBlockedIPs() ([]models.SFTPBlockedIP, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	rows, err := d.conn.Query(`SELECT ip, username, failed_attempts, blocked_until FROM sftp_ip_failures WHERE blocked_until > ? ORDER BY blocked_until`, time.Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]models.SFTPBlockedIP, 0)
	for rows.Next() {
		var item models.SFTPBlockedIP
		if err := rows.Scan(&item.IP, &item.Username, &item.FailedAttempts, &item.BlockedUntil); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (d *DB) UnblockSFTPIP(ip string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.conn.Exec(`DELETE FROM sftp_ip_failures WHERE ip=?`, ip)
	return err
}

// GetInboundConfig retrieves the active inbound configuration
func (d *DB) GetInboundConfig() (*models.InboundConfig, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	row := d.conn.QueryRow("SELECT id, protocol, sftp_port, ftp_port, target_dir, temp_dir, host_key_path, tls_cert_path, tls_key_path, auto_move_enabled, updated_at FROM inbound_configs LIMIT 1")
	var cfg models.InboundConfig
	var autoMove int
	err := row.Scan(&cfg.ID, &cfg.Protocol, &cfg.SFTPPort, &cfg.FTPPort, &cfg.TargetDir, &cfg.TempDir, &cfg.HostKeyPath, &cfg.TLSCertPath, &cfg.TLSKeyPath, &autoMove, &cfg.UpdatedAt)
	if err != nil {
		return nil, err
	}
	cfg.AutoMoveEnabled = autoMove == 1
	return &cfg, nil
}

// UpdateInboundConfig updates inbound listener settings
func (d *DB) UpdateInboundConfig(cfg *models.InboundConfig) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	autoMove := 0
	if cfg.AutoMoveEnabled {
		autoMove = 1
	}

	_, err := d.conn.Exec(`
		UPDATE inbound_configs 
		SET protocol=?, sftp_port=?, ftp_port=?, target_dir=?, temp_dir=?, host_key_path=?, tls_cert_path=?, tls_key_path=?, auto_move_enabled=?, updated_at=CURRENT_TIMESTAMP
		WHERE id=?
	`, cfg.Protocol, cfg.SFTPPort, cfg.FTPPort, cfg.TargetDir, cfg.TempDir, cfg.HostKeyPath, cfg.TLSCertPath, cfg.TLSKeyPath, autoMove, cfg.ID)
	return err
}

// GetUserByUsername checks credentials
func (d *DB) GetUserByUsername(username string) (*models.User, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	row := d.conn.QueryRow(`SELECT id, username, password, root_dir, enabled, can_list, can_read, can_write, can_delete, can_mkdir, can_rename, created_at, updated_at FROM users WHERE username = ?`, username)
	var u models.User
	var enabled, canList, canRead, canWrite, canDelete, canMkdir, canRename int
	err := row.Scan(&u.ID, &u.Username, &u.Password, &u.RootDir, &enabled, &canList, &canRead, &canWrite, &canDelete, &canMkdir, &canRename, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	u.Enabled = enabled == 1
	u.CanList, u.CanRead, u.CanWrite = canList == 1, canRead == 1, canWrite == 1
	u.CanDelete, u.CanMkdir, u.CanRename = canDelete == 1, canMkdir == 1, canRename == 1
	return &u, nil
}

// SaveUser creates or updates a user
func (d *DB) SaveUser(u *models.User) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	enabled := 0
	if u.Enabled {
		enabled = 1
	}

	var err error
	if u.Password != "" {
		_, err = d.conn.Exec(`
			INSERT INTO users (username, password, root_dir, enabled, can_list, can_read, can_write, can_delete, can_mkdir, can_rename, created_at, updated_at)
			VALUES (?, ?, '', ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
			ON CONFLICT(username) DO UPDATE SET
				password=excluded.password,
				enabled=excluded.enabled,
				can_list=excluded.can_list, can_read=excluded.can_read, can_write=excluded.can_write,
				can_delete=excluded.can_delete, can_mkdir=excluded.can_mkdir, can_rename=excluded.can_rename,
				updated_at=CURRENT_TIMESTAMP
		`, u.Username, u.Password, enabled, boolInt(u.CanList), boolInt(u.CanRead), boolInt(u.CanWrite), boolInt(u.CanDelete), boolInt(u.CanMkdir), boolInt(u.CanRename))
	} else {
		_, err = d.conn.Exec(`
			INSERT INTO users (username, password, root_dir, enabled, can_list, can_read, can_write, can_delete, can_mkdir, can_rename, created_at, updated_at)
			VALUES (?, '', '', ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
			ON CONFLICT(username) DO UPDATE SET
				enabled=excluded.enabled,
				can_list=excluded.can_list, can_read=excluded.can_read, can_write=excluded.can_write,
				can_delete=excluded.can_delete, can_mkdir=excluded.can_mkdir, can_rename=excluded.can_rename,
				updated_at=CURRENT_TIMESTAMP
		`, u.Username, enabled, boolInt(u.CanList), boolInt(u.CanRead), boolInt(u.CanWrite), boolInt(u.CanDelete), boolInt(u.CanMkdir), boolInt(u.CanRename))
	}
	if err == nil {
		var id int64
		err = d.conn.QueryRow("SELECT id FROM users WHERE username=?", u.Username).Scan(&id)
		if err == nil {
			u.ID = id
			u.RootDir = fmt.Sprintf("users/%d", id)
			_, err = d.conn.Exec("UPDATE users SET root_dir=?, updated_at=CURRENT_TIMESTAMP WHERE id=?", u.RootDir, id)
		}
	}
	if err == nil {
		_ = d.internalLogAudit(models.AuditLog{
			EventType: "USER_MGMT",
			Protocol:  "ADMIN",
			Username:  u.Username,
			Status:    "SUCCESS",
			Details:   fmt.Sprintf("User account %q created/updated", u.Username),
			Timestamp: time.Now(),
		})
	}
	return err
}

// DeleteUser removes a user by ID
func (d *DB) DeleteUser(id int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	var username string
	_ = d.conn.QueryRow("SELECT username FROM users WHERE id = ?", id).Scan(&username)

	_, err := d.conn.Exec("DELETE FROM users WHERE id = ?", id)
	if err == nil && username != "" {
		_ = d.internalLogAudit(models.AuditLog{
			EventType: "USER_MGMT",
			Protocol:  "ADMIN",
			Username:  username,
			Status:    "SUCCESS",
			Details:   fmt.Sprintf("User account %q deleted", username),
			Timestamp: time.Now(),
		})
	}
	return err
}

// ListUsers retrieves all users
func (d *DB) ListUsers() ([]models.User, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.Query(`SELECT id, username, root_dir, enabled, can_list, can_read, can_write, can_delete, can_mkdir, can_rename, created_at, updated_at FROM users`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []models.User
	for rows.Next() {
		var u models.User
		var enabled, canList, canRead, canWrite, canDelete, canMkdir, canRename int
		if err := rows.Scan(&u.ID, &u.Username, &u.RootDir, &enabled, &canList, &canRead, &canWrite, &canDelete, &canMkdir, &canRename, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		u.Enabled = enabled == 1
		u.CanList, u.CanRead, u.CanWrite = canList == 1, canRead == 1, canWrite == 1
		u.CanDelete, u.CanMkdir, u.CanRename = canDelete == 1, canMkdir == 1, canRename == 1
		list = append(list, u)
	}
	return list, nil
}

// ListOutboundJobs gets all outbound push jobs
func (d *DB) ListOutboundJobs() ([]models.OutboundJob, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.Query("SELECT id, name, cron_expr, source_user_id, source_dir, file_pattern, protocol, remote_host, remote_port, remote_user, remote_password, remote_dir, post_action, archive_dir, enabled, last_run_at, last_status, last_error, created_at, updated_at FROM outbound_jobs")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []models.OutboundJob
	for rows.Next() {
		var j models.OutboundJob
		var enabled int
		if err := rows.Scan(&j.ID, &j.Name, &j.CronExpr, &j.SourceUserID, &j.SourceDir, &j.FilePattern, &j.Protocol, &j.RemoteHost, &j.RemotePort, &j.RemoteUser, &j.RemotePassword, &j.RemoteDir, &j.PostAction, &j.ArchiveDir, &enabled, &j.LastRunAt, &j.LastStatus, &j.LastError, &j.CreatedAt, &j.UpdatedAt); err != nil {
			return nil, err
		}
		j.Enabled = enabled == 1
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// SaveOutboundJob inserts or updates an outbound job
func (d *DB) SaveOutboundJob(j *models.OutboundJob) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	enabled := 0
	if j.Enabled {
		enabled = 1
	}

	if j.ID == 0 {
		res, err := d.conn.Exec(`
			INSERT INTO outbound_jobs (name, cron_expr, source_user_id, source_dir, file_pattern, protocol, remote_host, remote_port, remote_user, remote_password, remote_dir, post_action, archive_dir, enabled)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, j.Name, j.CronExpr, j.SourceUserID, j.SourceDir, j.FilePattern, j.Protocol, j.RemoteHost, j.RemotePort, j.RemoteUser, j.RemotePassword, j.RemoteDir, j.PostAction, j.ArchiveDir, enabled)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		j.ID = id
		return nil
	}

	_, err := d.conn.Exec(`
		UPDATE outbound_jobs
		SET name=?, cron_expr=?, source_user_id=?, source_dir=?, file_pattern=?, protocol=?, remote_host=?, remote_port=?, remote_user=?, remote_password=?, remote_dir=?, post_action=?, archive_dir=?, enabled=?, updated_at=CURRENT_TIMESTAMP
		WHERE id=?
	`, j.Name, j.CronExpr, j.SourceUserID, j.SourceDir, j.FilePattern, j.Protocol, j.RemoteHost, j.RemotePort, j.RemoteUser, j.RemotePassword, j.RemoteDir, j.PostAction, j.ArchiveDir, enabled, j.ID)
	return err
}

// UpdateJobStatus updates the status and timestamp of a job execution
func (d *DB) UpdateJobStatus(id int64, status, lastError string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.conn.Exec(`
		UPDATE outbound_jobs
		SET last_run_at=CURRENT_TIMESTAMP, last_status=?, last_error=?, updated_at=CURRENT_TIMESTAMP
		WHERE id=?
	`, status, lastError, id)
	return err
}

// DeleteOutboundJob removes an outbound job
func (d *DB) DeleteOutboundJob(id int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.conn.Exec("DELETE FROM outbound_jobs WHERE id = ?", id)
	return err
}
