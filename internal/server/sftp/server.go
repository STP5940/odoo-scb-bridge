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
	"strings"
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
	mu        sync.Mutex
	running   bool
}

func NewServer(port int, targetDir, tempDir string, db *database.DB) *Server {
	return &Server{
		port:      port,
		targetDir: targetDir,
		tempDir:   tempDir,
		db:        db,
		running:   false,
	}
}

// Start launches the SFTP server
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return nil
	}

	_ = os.MkdirAll(s.targetDir, 0755)
	_ = os.MkdirAll(s.tempDir, 0755)

	sshConfig := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			clientIP, _, splitErr := net.SplitHostPort(c.RemoteAddr().String())
			if zone := strings.LastIndexByte(clientIP, '%'); zone >= 0 {
				clientIP = clientIP[:zone]
			}
			parsedIP := net.ParseIP(clientIP)
			if splitErr != nil || parsedIP == nil {
				_ = s.logSFTPLogin(c.User(), clientIP, "FAILED", "Could not determine client IP")
				return nil, fmt.Errorf("authentication rejected")
			}
			clientIP = parsedIP.String()
			allowed, policyErr := s.checkClientIP(clientIP)
			if policyErr != nil || !allowed {
				if policyErr != nil {
					log.Printf("[SFTP] Authentication rejected for %s because the IP security policy could not be checked", clientIP)
				}
				return nil, fmt.Errorf("authentication rejected")
			}
			user, err := s.db.GetUserByUsername(c.User())
			if err != nil || !user.Enabled || user.Password != string(pass) {
				_ = s.logSFTPLogin(c.User(), clientIP, "FAILED", "Invalid username or password")
				blocked, newlyBlocked, remaining, recordErr := s.db.RecordSFTPAuthFailure(clientIP, c.User(), time.Now())
				if recordErr != nil {
					log.Printf("[SFTP] Failed to update authentication throttling for %s: %v", clientIP, recordErr)
				}
				if newlyBlocked {
					durationDesc := fmt.Sprintf("%d minutes", remaining/60)
					if remaining >= 86400 {
						durationDesc = fmt.Sprintf("%d days", remaining/86400)
					} else if remaining >= 3600 {
						durationDesc = fmt.Sprintf("%d hours", remaining/3600)
					}
					settings, _, _ := s.db.GetSFTPAccessControl()
					_ = s.db.LogAudit(models.AuditLog{
						EventType: "SFTP_SECURITY", Protocol: "SFTP", Username: c.User(), ClientIP: clientIP,
						Status: "BLOCKED", Details: fmt.Sprintf("IP temporarily blocked after %d failed logins; block expires in %s", settings.MaxFailedAttempts, durationDesc),
					})
				} else if blocked {
					log.Printf("[SFTP] Authentication from %s rejected while blocked (%d seconds remain)", clientIP, remaining)
				}
				return nil, fmt.Errorf("authentication rejected")
			}
			_ = s.db.ResetSFTPAuthFailures(clientIP)
			_ = s.logSFTPLogin(c.User(), clientIP, "SUCCESS", "Login accepted")
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
	s.quit = make(chan struct{})
	s.running = true
	log.Printf("[SFTP] Server listening on %s (Drop folder: %s)", addr, s.targetDir)

	s.wg.Add(1)
	go s.acceptLoop(sshConfig)

	return nil
}

func (s *Server) logSFTPLogin(username, clientIP, status, details string) error {
	return s.db.LogAudit(models.AuditLog{
		EventType: "LOGIN",
		Protocol:  "SFTP",
		Username:  username,
		ClientIP:  clientIP,
		Status:    status,
		Details:   details,
	})
}

func (s *Server) checkClientIP(clientIP string) (bool, error) {
	blockedUntil, err := s.db.GetSFTPIPBlockedUntil(clientIP)
	if err != nil {
		return false, err
	}
	if blockedUntil.After(time.Now()) {
		return false, nil
	}
	settings, rules, err := s.db.GetSFTPAccessControl()
	if err != nil {
		return false, err
	}
	allowedByRule := false
	for _, rule := range rules {
		if !ipMatchesRule(clientIP, rule.CIDR) {
			continue
		}
		if rule.Action == "block" {
			return false, nil
		}
		if rule.Action == "allow" {
			allowedByRule = true
		}
	}
	if settings.IPMode == "allow_list" && !allowedByRule {
		return false, nil
	}
	if settings.IPMode != "allow_list" && settings.IPMode != "allow_all" {
		return false, fmt.Errorf("invalid SFTP IP access mode")
	}
	return true, nil
}

func ipMatchesRule(clientIP, cidr string) bool {
	ip := net.ParseIP(clientIP)
	if ip == nil {
		return false
	}
	if ruleIP := net.ParseIP(cidr); ruleIP != nil {
		return ruleIP.Equal(ip)
	}
	_, network, err := net.ParseCIDR(cidr)
	return err == nil && network.Contains(ip)
}

func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return
	}

	s.running = false
	if s.quit != nil {
		close(s.quit)
	}
	if s.listener != nil {
		_ = s.listener.Close()
		s.listener = nil
	}
	s.wg.Wait()
	log.Printf("[SFTP] Server stopped")
}

func (s *Server) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
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

		// Give each authenticated account its own filesystem root and enforce its
		// configured operations in the request handlers.
		clientIP, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
		user, err := s.db.GetUserByUsername(sshConn.User())
		if err != nil {
			return
		}
		userRootPath := filepath.Join(s.targetDir, filepath.FromSlash(user.RootDir))
		if err := os.MkdirAll(userRootPath, 0750); err != nil {
			log.Printf("[SFTP] Could not create home directory for %q: %v", user.Username, err)
			return
		}
		userRoot, err := os.OpenRoot(userRootPath)
		if err != nil {
			log.Printf("[SFTP] Could not open home directory for %q: %v", user.Username, err)
			return
		}
		fs := newUserFilesystem(userRoot, *user)
		server := sftp.NewRequestServer(channel, fs.handlers(), sftp.WithStartDirectory("/"))
		serveErr := server.Serve()
		_ = server.Close()
		_ = userRoot.Close()

		if serveErr != nil && serveErr != io.EOF {
			log.Printf("[SFTP] Session for %q ended: %v", user.Username, serveErr)
		}
		// Audit only files changed in this authenticated account's inbound home.
		s.processUploadedFiles(user.Username, clientIP, userRootPath, fs.modifiedFiles())
	}
}

func (s *Server) processUploadedFiles(username, clientIP, userRoot string, files []string) {
	for _, fileName := range files {
		fileName = filepath.Clean(filepath.FromSlash(fileName))
		if fileName == "." || filepath.IsAbs(fileName) || fileName == ".." || strings.HasPrefix(fileName, ".."+string(filepath.Separator)) {
			continue
		}
		inboundFilePath := filepath.Join(userRoot, fileName)
		info, err := os.Lstat(inboundFilePath)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		hash, size, err := utils.CalculateFileSHA256(inboundFilePath)
		if err != nil {
			continue
		}
		_ = s.db.LogAudit(models.AuditLog{
			EventType: "INBOUND_FILE", Protocol: "SFTP", Username: username, ClientIP: clientIP,
			FileName: filepath.ToSlash(fileName), FileSize: size, FileHash: hash, Status: "SUCCESS",
			Details: fmt.Sprintf("Received in private inbound folder %s", inboundFilePath), Timestamp: time.Now(),
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
