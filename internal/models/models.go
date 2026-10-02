package models

import "time"

// User represents an FTP/SFTP account
type User struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	Password  string    `json:"-"`
	RootDir   string    `json:"root_dir"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// InboundConfig represents configuration for incoming file listeners
type InboundConfig struct {
	ID              int64     `json:"id"`
	Protocol        string    `json:"protocol"`         // "sftp", "ftps", "both"
	SFTPPort        int       `json:"sftp_port"`        // default 2222
	FTPPort         int       `json:"ftp_port"`         // default 2121
	TargetDir       string    `json:"target_dir"`       // Destination folder where files will be stored
	TempDir         string    `json:"temp_dir"`         // Temp upload staging area
	HostKeyPath     string    `json:"host_key_path"`    // SSH Private key path for SFTP
	TLSCertPath     string    `json:"tls_cert_path"`    // TLS Cert for FTPS
	TLSKeyPath      string    `json:"tls_key_path"`     // TLS Key for FTPS
	AutoMoveEnabled bool      `json:"auto_move_enabled"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// OutboundJob represents a scheduled push task to remote destinations
type OutboundJob struct {
	ID             int64      `json:"id"`
	Name           string     `json:"name"`
	CronExpr       string     `json:"cron_expr"`        // e.g. "*/5 * * * *" or "@every 10m"
	SourceDir      string     `json:"source_dir"`       // Folder to scan for files to push
	FilePattern    string     `json:"file_pattern"`     // e.g. "*.txt", "*.*"
	Protocol       string     `json:"protocol"`         // "sftp", "ftps", "ftp"
	RemoteHost     string     `json:"remote_host"`
	RemotePort     int        `json:"remote_port"`
	RemoteUser     string     `json:"remote_user"`
	RemotePassword string     `json:"remote_password"`
	RemoteDir      string     `json:"remote_dir"`       // Target path on remote host
	PostAction     string     `json:"post_action"`      // "delete", "archive", "none"
	ArchiveDir     string     `json:"archive_dir"`      // If post_action is "archive"
	Enabled        bool       `json:"enabled"`
	LastRunAt      *time.Time `json:"last_run_at"`
	LastStatus     string     `json:"last_status"`
	LastError      string     `json:"last_error"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// AuditLog represents historical events: logins, file transfers, job executions
type AuditLog struct {
	ID          int64     `json:"id"`
	EventType   string    `json:"event_type"`   // "LOGIN", "INBOUND_FILE", "OUTBOUND_FILE", "JOB_RUN"
	Protocol    string    `json:"protocol"`     // "SFTP", "FTPS", "FTP", "SYSTEM"
	Username    string    `json:"username"`
	ClientIP    string    `json:"client_ip"`
	FileName    string    `json:"file_name"`
	FileSize    int64     `json:"file_size"`
	FileHash    string    `json:"file_hash"`    // SHA-256 Checksum
	Status      string    `json:"status"`       // "SUCCESS", "FAILED"
	Details     string    `json:"details"`
	Timestamp   time.Time `json:"timestamp"`
}
