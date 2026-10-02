package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
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
	TempDir   string `json:"temp_dir"`
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
	relativeTarget, err := filepath.Rel(filepath.Clean(config.TempDir), filepath.Clean(config.TargetDir))
	if err != nil {
		fail("Cannot calculate the inbound path relative to the SFTP upload folder: %v", err)
	}
	if filepath.IsAbs(relativeTarget) || strings.HasPrefix(relativeTarget, "..\\..\\..\\..\\..") {
		fail("Configured inbound folder is outside the expected SFTP data directory")
	}

	name := "codex_sftp_probe_" + strings.ReplaceAll(uuid.NewString(), "-", "") + ".txt"
	payload := []byte("Odoo SCB Bridge SFTP upload test\nProbe: " + name + "\n")
	targetPath := filepath.ToSlash(filepath.Join(relativeTarget, name))

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
		if err := verifyAndKeep(*host, *port, *username, password, targetPath, payload); err == nil {
			verified = true
			break
		} else {
			verifyErr = err
		}
	}
	if !verified {
		cleanupErr := removeProbe(*host, *port, *username, password, targetPath)
		if cleanupErr != nil {
			fail("Could not verify the uploaded file (%v); automatic cleanup also failed (%v). Probe: %s", verifyErr, cleanupErr, name)
		}
		fail("Could not verify the uploaded file: %v. The probe was removed.", verifyErr)
	}

	fmt.Printf("PASS: inbound file content verified at %s\n", filepath.Join(config.TargetDir, name))
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
	if config.TargetDir == "" || config.TempDir == "" {
		return nil, errors.New("target_dir or temp_dir is empty")
	}
	return &config, nil
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

func verifyAndKeep(host string, port int, username, password, path string, expected []byte) error {
	conn, client, err := connectSFTP(host, port, username, password)
	if err != nil {
		return fmt.Errorf("connect for verification: %w", err)
	}
	file, err := client.Open(path)
	if err != nil {
		closeSFTPSession(conn, client)
		return fmt.Errorf("inbound file not available yet: %w", err)
	}
	actual, readErr := io.ReadAll(file)
	closeFileErr := file.Close()
	if readErr != nil {
		closeSFTPSession(conn, client)
		return readErr
	}
	if closeFileErr != nil {
		closeSFTPSession(conn, client)
		return closeFileErr
	}
	if !bytes.Equal(actual, expected) {
		closeSFTPSession(conn, client)
		return errors.New("inbound file content does not match the uploaded probe")
	}
	closeSFTPSession(conn, client)
	return nil
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
