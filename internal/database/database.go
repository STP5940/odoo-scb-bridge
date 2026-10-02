package database

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"odoo-scb-bridge/internal/models"

	_ "modernc.org/sqlite"
)

type DB struct {
	conn *sql.DB
	mu   sync.RWMutex
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

		d := &DB{conn: conn}
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
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_audit_logs_event_type ON audit_logs(event_type);
	CREATE INDEX IF NOT EXISTS idx_audit_logs_timestamp ON audit_logs(timestamp);
	`

	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.conn.Exec(schema)
	if err != nil {
		return fmt.Errorf("migration error: %w", err)
	}

	// Seed default inbound config if not present
	var count int
	err = d.conn.QueryRow("SELECT COUNT(*) FROM inbound_configs").Scan(&count)
	if err == nil && count == 0 {
		baseDir, _ := os.Getwd()
		targetDir := filepath.Join(baseDir, "data", "inbound")
		tempDir := filepath.Join(baseDir, "data", "temp")
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

	return nil
}

// LogAudit inserts a new event record
func (d *DB) LogAudit(log models.AuditLog) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.internalLogAudit(log)
}

func (d *DB) internalLogAudit(log models.AuditLog) error {
	query := `
		INSERT INTO audit_logs (event_type, protocol, username, client_ip, file_name, file_size, file_hash, status, details, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	ts := log.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	_, err := d.conn.Exec(query, log.EventType, log.Protocol, log.Username, log.ClientIP, log.FileName, log.FileSize, log.FileHash, log.Status, log.Details, ts)
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
		rows, err = d.conn.Query("SELECT id, event_type, protocol, username, client_ip, file_name, file_size, file_hash, status, details, timestamp FROM audit_logs WHERE event_type = ? ORDER BY id DESC LIMIT ? OFFSET ?", eventType, limit, offset)
	} else {
		rows, err = d.conn.Query("SELECT id, event_type, protocol, username, client_ip, file_name, file_size, file_hash, status, details, timestamp FROM audit_logs ORDER BY id DESC LIMIT ? OFFSET ?", limit, offset)
	}

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	logs := make([]models.AuditLog, 0)
	for rows.Next() {
		var l models.AuditLog
		if err := rows.Scan(&l.ID, &l.EventType, &l.Protocol, &l.Username, &l.ClientIP, &l.FileName, &l.FileSize, &l.FileHash, &l.Status, &l.Details, &l.Timestamp); err != nil {
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

	row := d.conn.QueryRow("SELECT id, username, password, root_dir, enabled, created_at, updated_at FROM users WHERE username = ?", username)
	var u models.User
	var enabled int
	err := row.Scan(&u.ID, &u.Username, &u.Password, &u.RootDir, &enabled, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	u.Enabled = enabled == 1
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
			INSERT INTO users (username, password, root_dir, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
			ON CONFLICT(username) DO UPDATE SET
				password=excluded.password,
				root_dir=excluded.root_dir,
				enabled=excluded.enabled,
				updated_at=CURRENT_TIMESTAMP
		`, u.Username, u.Password, u.RootDir, enabled)
	} else {
		_, err = d.conn.Exec(`
			INSERT INTO users (username, password, root_dir, enabled, created_at, updated_at)
			VALUES (?, '', ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
			ON CONFLICT(username) DO UPDATE SET
				root_dir=excluded.root_dir,
				enabled=excluded.enabled,
				updated_at=CURRENT_TIMESTAMP
		`, u.Username, u.RootDir, enabled)
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

	rows, err := d.conn.Query("SELECT id, username, root_dir, enabled, created_at, updated_at FROM users")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []models.User
	for rows.Next() {
		var u models.User
		var enabled int
		if err := rows.Scan(&u.ID, &u.Username, &u.RootDir, &enabled, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		u.Enabled = enabled == 1
		list = append(list, u)
	}
	return list, nil
}

// ListOutboundJobs gets all outbound push jobs
func (d *DB) ListOutboundJobs() ([]models.OutboundJob, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.Query("SELECT id, name, cron_expr, source_dir, file_pattern, protocol, remote_host, remote_port, remote_user, remote_password, remote_dir, post_action, archive_dir, enabled, last_run_at, last_status, last_error, created_at, updated_at FROM outbound_jobs")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []models.OutboundJob
	for rows.Next() {
		var j models.OutboundJob
		var enabled int
		if err := rows.Scan(&j.ID, &j.Name, &j.CronExpr, &j.SourceDir, &j.FilePattern, &j.Protocol, &j.RemoteHost, &j.RemotePort, &j.RemoteUser, &j.RemotePassword, &j.RemoteDir, &j.PostAction, &j.ArchiveDir, &enabled, &j.LastRunAt, &j.LastStatus, &j.LastError, &j.CreatedAt, &j.UpdatedAt); err != nil {
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
			INSERT INTO outbound_jobs (name, cron_expr, source_dir, file_pattern, protocol, remote_host, remote_port, remote_user, remote_password, remote_dir, post_action, archive_dir, enabled)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, j.Name, j.CronExpr, j.SourceDir, j.FilePattern, j.Protocol, j.RemoteHost, j.RemotePort, j.RemoteUser, j.RemotePassword, j.RemoteDir, j.PostAction, j.ArchiveDir, enabled)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		j.ID = id
		return nil
	}

	_, err := d.conn.Exec(`
		UPDATE outbound_jobs
		SET name=?, cron_expr=?, source_dir=?, file_pattern=?, protocol=?, remote_host=?, remote_port=?, remote_user=?, remote_password=?, remote_dir=?, post_action=?, archive_dir=?, enabled=?, updated_at=CURRENT_TIMESTAMP
		WHERE id=?
	`, j.Name, j.CronExpr, j.SourceDir, j.FilePattern, j.Protocol, j.RemoteHost, j.RemotePort, j.RemoteUser, j.RemotePassword, j.RemoteDir, j.PostAction, j.ArchiveDir, enabled, j.ID)
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
