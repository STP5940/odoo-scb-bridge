package sftp

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"odoo-scb-bridge/internal/database"
	"odoo-scb-bridge/internal/models"
	"odoo-scb-bridge/internal/utils"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type Server struct {
	port      int
	targetDir string
	tempDir   string
	listener  net.Listener
	db        *database.DB
	quit      chan struct{}
	wg        sync.WaitGroup
}

func NewServer(port int, targetDir, tempDir string, db *database.DB) *Server {
	return &Server{
		port:      port,
		targetDir: targetDir,
		tempDir:   tempDir,
		db:        db,
		quit:      make(chan struct{}),
	}
}

// Start launches the SFTP server
func (s *Server) Start() error {
	_ = os.MkdirAll(s.targetDir, 0755)
	_ = os.MkdirAll(s.tempDir, 0755)

	sshConfig := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			clientIP, _, _ := net.SplitHostPort(c.RemoteAddr().String())
			user, err := s.db.GetUserByUsername(c.User())
			if err != nil || !user.Enabled || user.Password != string(pass) {
				_ = s.db.LogAudit(models.AuditLog{
					EventType: "LOGIN",
					Protocol:  "SFTP",
					Username:  c.User(),
					ClientIP:  clientIP,
					Status:    "FAILED",
					Details:   "Invalid username or password",
				})
				return nil, fmt.Errorf("password rejected for %q", c.User())
			}

			_ = s.db.LogAudit(models.AuditLog{
				EventType: "LOGIN",
				Protocol:  "SFTP",
				Username:  c.User(),
				ClientIP:  clientIP,
				Status:    "SUCCESS",
				Details:   "Login accepted",
			})
			return nil, nil
		},
	}

	keyPath := filepath.Join(filepath.Dir(s.targetDir), "host_rsa.key")
	hostKey, err := generateOrLoadHostKey(keyPath)
	if err != nil {
		return fmt.Errorf("failed to load/generate host key: %w", err)
	}
	sshConfig.AddHostKey(hostKey)

	addr := fmt.Sprintf("0.0.0.0:%d", s.port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	s.listener = listener
	log.Printf("[SFTP] Server listening on %s (Drop folder: %s)", addr, s.targetDir)

	s.wg.Add(1)
	go s.acceptLoop(sshConfig)

	return nil
}

func (s *Server) Stop() {
	close(s.quit)
	if s.listener != nil {
		s.listener.Close()
	}
	s.wg.Wait()
	log.Printf("[SFTP] Server stopped")
}

func (s *Server) acceptLoop(sshConfig *ssh.ServerConfig) {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return
			default:
				log.Printf("[SFTP] Accept error: %v", err)
				continue
			}
		}

		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			s.handleConn(c, sshConfig)
		}(conn)
	}
}

func (s *Server) handleConn(conn net.Conn, sshConfig *ssh.ServerConfig) {
	defer conn.Close()

	sshConn, chans, reqs, err := ssh.NewServerConn(conn, sshConfig)
	if err != nil {
		return
	}
	defer sshConn.Close()

	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}

		channel, requests, err := newChan.Accept()
		if err != nil {
			return
		}

		go func(in <-chan *ssh.Request) {
			for req := range in {
				ok := false
				if req.Type == "subsystem" && len(req.Payload) >= 4 && string(req.Payload[4:]) == "sftp" {
					ok = true
				}
				req.Reply(ok, nil)
			}
		}(requests)

		// Serve SFTP filesystem in tempDir
		clientIP, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
		server, err := sftp.NewServer(
			channel,
			sftp.WithDebug(io.Discard),
		)
		if err != nil {
			return
		}

		if err := server.Serve(); err == io.EOF {
			server.Close()
		}

		// Check for any uploaded files in tempDir and move to targetDir
		s.processUploadedFiles(sshConn.User(), clientIP)
	}
}

func (s *Server) processUploadedFiles(username, clientIP string) {
	entries, err := os.ReadDir(s.tempDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fileName := entry.Name()
		tempFilePath := filepath.Join(s.tempDir, fileName)
		targetFilePath := filepath.Join(s.targetDir, fileName)

		hash, size, err := utils.CalculateFileSHA256(tempFilePath)
		if err != nil {
			continue
		}

		err = utils.MoveFile(tempFilePath, targetFilePath)
		status := "SUCCESS"
		details := fmt.Sprintf("Stored in %s", targetFilePath)
		if err != nil {
			status = "FAILED"
			details = fmt.Sprintf("Move error: %v", err)
		}

		_ = s.db.LogAudit(models.AuditLog{
			EventType: "INBOUND_FILE",
			Protocol:  "SFTP",
			Username:  username,
			ClientIP:  clientIP,
			FileName:  fileName,
			FileSize:  size,
			FileHash:  hash,
			Status:    status,
			Details:   details,
			Timestamp: time.Now(),
		})
	}
}

func generateOrLoadHostKey(keyPath string) (ssh.Signer, error) {
	if data, err := os.ReadFile(keyPath); err == nil {
		return ssh.ParsePrivateKey(data)
	}

	// Generate 2048-bit RSA key
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	keyBytes := x509.MarshalPKCS1PrivateKey(privateKey)
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: keyBytes,
	})

	_ = os.WriteFile(keyPath, keyPEM, 0600)
	return ssh.ParsePrivateKey(keyPEM)
}
