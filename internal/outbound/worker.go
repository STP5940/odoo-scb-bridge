package outbound

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"odoo-scb-bridge/internal/database"
	"odoo-scb-bridge/internal/models"
	"odoo-scb-bridge/internal/utils"

	"github.com/jlaffaye/ftp"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type Worker struct {
	db *database.DB
}

func NewWorker(db *database.DB) *Worker {
	return &Worker{db: db}
}

// ExecuteJob runs a single outbound job scan and push
func (w *Worker) ExecuteJob(job *models.OutboundJob) error {
	// 1. Scan source directory
	files, err := filepath.Glob(filepath.Join(job.SourceDir, job.FilePattern))
	if err != nil {
		w.db.UpdateJobStatus(job.ID, "FAILED", fmt.Sprintf("Glob error: %v", err))
		return err
	}

	if len(files) == 0 {
		w.db.UpdateJobStatus(job.ID, "SUCCESS", "No files to dispatch")
		return nil
	}

	var lastErr error
	for _, filePath := range files {
		info, err := os.Stat(filePath)
		if err != nil || info.IsDir() {
			continue
		}

		fileName := info.Name()
		hash, size, err := utils.CalculateFileSHA256(filePath)
		if err != nil {
			lastErr = err
			continue
		}

		// Dispatch via configured protocol
		destinationIP := ""
		switch job.Protocol {
		case "sftp":
			destinationIP, err = w.sendViaSFTP(job, filePath, fileName)
		case "ftp", "ftps":
			destinationIP, err = w.sendViaFTP(job, filePath, fileName)
		default:
			err = fmt.Errorf("unsupported protocol: %s", job.Protocol)
		}

		status := "SUCCESS"
		details := fmt.Sprintf("Pushed to %s:%d/%s", job.RemoteHost, job.RemotePort, job.RemoteDir)
		if err != nil {
			status = "FAILED"
			details = fmt.Sprintf("Push error: %v", err)
			lastErr = err
		} else {
			// Perform Post-Action
			w.handlePostAction(job, filePath, fileName)
		}

		_ = w.db.LogAudit(models.AuditLog{
			EventType: "OUTBOUND_FILE",
			Protocol:  job.Protocol,
			Username:  job.RemoteUser,
			ClientIP:  destinationIP,
			FileName:  fileName,
			FileSize:  size,
			FileHash:  hash,
			Status:    status,
			Details:   details,
			Timestamp: time.Now(),
		})
	}

	if lastErr != nil {
		w.db.UpdateJobStatus(job.ID, "FAILED", lastErr.Error())
		return lastErr
	}

	w.db.UpdateJobStatus(job.ID, "SUCCESS", fmt.Sprintf("Dispatched %d files", len(files)))
	return nil
}

func (w *Worker) sendViaSFTP(job *models.OutboundJob, localPath, fileName string) (string, error) {
	config := &ssh.ClientConfig{
		User: job.RemoteUser,
		Auth: []ssh.AuthMethod{
			ssh.Password(job.RemotePassword),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	addr := net.JoinHostPort(job.RemoteHost, fmt.Sprintf("%d", job.RemotePort))
	sshClient, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return "", fmt.Errorf("ssh dial error: %w", err)
	}
	defer sshClient.Close()
	destinationIP := hostFromAddr(sshClient.RemoteAddr())

	client, err := sftp.NewClient(sshClient)
	if err != nil {
		return destinationIP, fmt.Errorf("sftp client error: %w", err)
	}
	defer client.Close()

	localFile, err := os.Open(localPath)
	if err != nil {
		return destinationIP, err
	}
	defer localFile.Close()

	remotePath := filepath.ToSlash(filepath.Join(job.RemoteDir, fileName))
	remoteFile, err := client.Create(remotePath)
	if err != nil {
		return destinationIP, fmt.Errorf("remote create error: %w", err)
	}
	defer remoteFile.Close()

	_, err = io.Copy(remoteFile, localFile)
	return destinationIP, err
}

func (w *Worker) sendViaFTP(job *models.OutboundJob, localPath, fileName string) (string, error) {
	addr := net.JoinHostPort(job.RemoteHost, fmt.Sprintf("%d", job.RemotePort))
	opts := ftpDialOptions(job)
	destinationIP := ""
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	opts = append(opts, ftp.DialWithDialFunc(func(network, address string) (net.Conn, error) {
		conn, err := dialer.Dial(network, address)
		if err == nil && destinationIP == "" {
			destinationIP = hostFromAddr(conn.RemoteAddr())
		}
		return conn, err
	}))
	c, err := ftp.Dial(addr, opts...)
	if err != nil {
		return destinationIP, fmt.Errorf("ftp dial error: %w", err)
	}
	defer c.Quit()

	err = c.Login(job.RemoteUser, job.RemotePassword)
	if err != nil {
		return destinationIP, fmt.Errorf("ftp login error: %w", err)
	}

	localFile, err := os.Open(localPath)
	if err != nil {
		return destinationIP, err
	}
	defer localFile.Close()

	remotePath := filepath.ToSlash(filepath.Join(job.RemoteDir, fileName))
	return destinationIP, c.Stor(remotePath, localFile)
}

func hostFromAddr(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err == nil {
		return host
	}
	return addr.String()
}

// TestConnection checks network reachability, authentication, and remote folder access without transferring files.
func TestConnection(job *models.OutboundJob) error {
	if job == nil || job.RemoteHost == "" || job.RemotePort < 1 || job.RemotePort > 65535 || job.RemoteUser == "" || job.RemotePassword == "" {
		return fmt.Errorf("host, valid port, username, and password are required")
	}
	if job.RemoteDir == "" {
		job.RemoteDir = "/"
	}
	addr := net.JoinHostPort(job.RemoteHost, fmt.Sprintf("%d", job.RemotePort))
	switch job.Protocol {
	case "sftp":
		config := &ssh.ClientConfig{
			User:            job.RemoteUser,
			Auth:            []ssh.AuthMethod{ssh.Password(job.RemotePassword)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			Timeout:         10 * time.Second,
		}
		sshClient, err := ssh.Dial("tcp", addr, config)
		if err != nil {
			return fmt.Errorf("SFTP connection or authentication failed: %w", err)
		}
		defer sshClient.Close()
		client, err := sftp.NewClient(sshClient)
		if err != nil {
			return fmt.Errorf("SFTP session failed: %w", err)
		}
		defer client.Close()
		info, err := client.Stat(job.RemoteDir)
		if err != nil {
			return fmt.Errorf("cannot access remote folder %q: %w", job.RemoteDir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("remote path %q is not a directory", job.RemoteDir)
		}
		return nil
	case "ftp", "ftps":
		c, err := ftp.Dial(addr, ftpDialOptions(job)...)
		if err != nil {
			return fmt.Errorf("FTP connection failed: %w", err)
		}
		defer c.Quit()
		if err := c.Login(job.RemoteUser, job.RemotePassword); err != nil {
			return fmt.Errorf("FTP authentication failed: %w", err)
		}
		if err := c.ChangeDir(job.RemoteDir); err != nil {
			return fmt.Errorf("cannot access remote folder %q: %w", job.RemoteDir, err)
		}
		if err := c.NoOp(); err != nil {
			return fmt.Errorf("FTP session check failed: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported protocol: %s", job.Protocol)
	}
}

func ftpDialOptions(job *models.OutboundJob) []ftp.DialOption {
	opts := []ftp.DialOption{ftp.DialWithTimeout(10 * time.Second)}
	if job.Protocol == "ftps" {
		opts = append(opts, ftp.DialWithExplicitTLS(&tls.Config{
			ServerName: job.RemoteHost,
			MinVersion: tls.VersionTLS12,
		}))
	}
	return opts
}

func (w *Worker) handlePostAction(job *models.OutboundJob, localPath, fileName string) {
	switch job.PostAction {
	case "delete":
		_ = os.Remove(localPath)
	case "archive":
		if job.ArchiveDir != "" {
			_ = os.MkdirAll(job.ArchiveDir, 0755)
			dest := filepath.Join(job.ArchiveDir, fileName)
			_ = utils.MoveFile(localPath, dest)
		}
	}
}
