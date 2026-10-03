package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type inboundConfig struct {
	TargetDir string `json:"target_dir"`
}

type sftpUser struct {
	Username string `json:"username"`
	RootDir  string `json:"root_dir"`
}

type auditLog struct {
	FileName string `json:"file_name"`
	FileSize int64  `json:"file_size"`
	FileHash string `json:"file_hash"`
	Status   string `json:"status"`
}

func main() {
	host := flag.String("host", "127.0.0.1", "SFTP server hostname or IP")
	port := flag.Int("port", 2222, "SFTP server port")
	username := flag.String("user", "", "SFTP username")
	apiURL := flag.String("api", "http://127.0.0.1:9527/api/inbound", "Bridge inbound configuration API")
	flag.Parse()

	if *username == "" {
		fail("SFTP username is required")
	}
	if !isLoopback(*host) {
		fail("This test helper skips host-key verification and only supports a local SFTP server")
	}
	password := os.Getenv("SFTP_PROBE_PASSWORD")
	if password == "" {
		fail("Password was not provided by the secure prompt")
	}

	config, err := loadInboundConfig(*apiURL)
	if err != nil {
		fail("Cannot read the bridge inbound configuration: %v", err)
	}
	userRoot, err := loadUserRootDir(*apiURL, *username)
	if err != nil {
		fail("Cannot read the SFTP user's private folder: %v", err)
	}
	name := "codex_sftp_probe_" + strings.ReplaceAll(uuid.NewString(), "-", "") + ".txt"
	payload := []byte("Odoo SCB Bridge SFTP upload test\nProbe: " + name + "\n")

	if err := upload(*host, *port, *username, password, name, payload); err != nil {
		fail("Upload failed: %v", err)
	}
	fmt.Printf("Uploaded probe: %s\n", name)

	verified := false
	var verifyErr error
	for attempt := 0; attempt < 12; attempt++ {
		if attempt > 0 {
			time.Sleep(250 * time.Millisecond)
		}
		if err := verifyInboundAudit(*apiURL, *username, name, payload); err == nil {
			verified = true
			break
		} else {
			verifyErr = err
		}
	}
	if !verified {
		cleanupErr := removeProbe(*host, *port, *username, password, name)
		if cleanupErr != nil {
			fail("Could not verify the uploaded file (%v); automatic cleanup also failed (%v). Probe: %s", verifyErr, cleanupErr, name)
		}
		fail("Could not verify the uploaded file: %v. The probe was removed.", verifyErr)
	}

	fmt.Printf("PASS: inbound file content verified at %s\n", filepath.Join(config.TargetDir, filepath.FromSlash(userRoot), name))
	fmt.Println("Note: a scheduled outbound job may move this file to its archive folder after verification.")
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func loadInboundConfig(apiURL string) (*inboundConfig, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("configuration API returned %s", resp.Status)
	}
	var config inboundConfig
	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		return nil, err
	}
	if config.TargetDir == "" {
		return nil, errors.New("target_dir is empty")
	}
	return &config, nil
}

func loadUserRootDir(apiURL, username string) (string, error) {
	parsedURL, err := url.Parse(apiURL)
	if err != nil {
		return "", fmt.Errorf("invalid API URL: %w", err)
	}
	parsedURL.Path = strings.TrimSuffix(parsedURL.Path, "/inbound") + "/users"
	response, err := (&http.Client{Timeout: 5 * time.Second}).Get(parsedURL.String())
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("users API returned %s", response.Status)
	}
	var users []sftpUser
	if err := json.NewDecoder(response.Body).Decode(&users); err != nil {
		return "", err
	}
	for _, user := range users {
		if user.Username != username {
			continue
		}
		root := filepath.Clean(filepath.FromSlash(user.RootDir))
		if root == "." || filepath.IsAbs(root) || root == ".." || strings.HasPrefix(root, ".."+string(filepath.Separator)) {
			return "", errors.New("user home path is not a safe relative path")
		}
		return user.RootDir, nil
	}
	return "", fmt.Errorf("SFTP user %q was not found", username)
}

func sshConfig(username, password string) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:            username,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // The test app creates its host key dynamically.
		Timeout:         8 * time.Second,
	}
}

func connectSFTP(host string, port int, username, password string) (*ssh.Client, *sftp.Client, error) {
	conn, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", host, port), sshConfig(username, password))
	if err != nil {
		return nil, nil, err
	}
	client, err := sftp.NewClient(conn)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, client, nil
}

func upload(host string, port int, username, password, name string, payload []byte) error {
	conn, client, err := connectSFTP(host, port, username, password)
	if err != nil {
		return fmt.Errorf("connect/authenticate: %w", err)
	}
	file, err := client.Create(name)
	if err != nil {
		closeSFTPSession(conn, client)
		return fmt.Errorf("create remote file: %w", err)
	}
	_, writeErr := file.Write(payload)
	closeFileErr := file.Close()
	closeSFTPSession(conn, client)
	if writeErr != nil {
		return writeErr
	}
	if closeFileErr != nil {
		return closeFileErr
	}
	return nil
}

func verifyInboundAudit(apiURL, username, filename string, expected []byte) error {
	parsedURL, err := url.Parse(apiURL)
	if err != nil {
		return fmt.Errorf("invalid API URL: %w", err)
	}
	parsedURL.Path = strings.TrimSuffix(parsedURL.Path, "/inbound") + "/logs"
	query := parsedURL.Query()
	query.Set("username", username)
	query.Set("page", "1")
	query.Set("page_size", "100")
	parsedURL.RawQuery = query.Encode()

	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(parsedURL.String())
	if err != nil {
		return fmt.Errorf("read inbound transfer log: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("inbound log API returned %s", response.Status)
	}
	var logs []auditLog
	if err := json.NewDecoder(response.Body).Decode(&logs); err != nil {
		return fmt.Errorf("decode inbound transfer log: %w", err)
	}
	wantHash := sha256.Sum256(expected)
	wantHashText := hex.EncodeToString(wantHash[:])
	for _, item := range logs {
		if item.FileName != filename || item.Status != "SUCCESS" {
			continue
		}
		if item.FileSize != int64(len(expected)) || item.FileHash != wantHashText {
			return errors.New("inbound file content does not match the uploaded probe")
		}
		return nil
	}
	return errors.New("the inbound server has not recorded this probe yet")
}

func removeProbe(host string, port int, username, password, path string) error {
	conn, client, err := connectSFTP(host, port, username, password)
	if err != nil {
		return err
	}
	defer closeSFTPSession(conn, client)
	for _, candidate := range []string{path, filepath.Base(path)} {
		if _, err := client.Stat(candidate); err == nil {
			if err := client.Remove(candidate); err != nil {
				return err
			}
		}
	}
	return nil
}

func closeSFTPSession(conn *ssh.Client, client *sftp.Client) {
	// Closing SSH first also closes the SFTP channel, which avoids waiting
	// indefinitely for a channel-close reply from older bridge builds.
	_ = conn.Close()
	_ = client.Close()
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[FAIL] "+format+"\n", args...)
	os.Exit(1)
}
