package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"odoo-scb-bridge/internal/database"
	"odoo-scb-bridge/internal/models"
)

func TestHandleLogsReturnsRequestedPage(t *testing.T) {
	t.Chdir(t.TempDir())

	db, err := database.Init(":memory:")
	if err != nil {
		t.Fatalf("initialize test database: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := db.LogAudit(models.AuditLog{
			EventType: "PAGE_TEST",
			Protocol:  "TEST",
			Status:    "SUCCESS",
			Details:   string(rune('0' + i)),
		}); err != nil {
			t.Fatalf("insert test log %d: %v", i, err)
		}
	}

	server := &Server{db: db}
	request := httptest.NewRequest("GET", "/api/logs?page=2&page_size=2&event_type=PAGE_TEST", nil)
	response := httptest.NewRecorder()
	server.handleLogs(response, request)

	if response.Code != 200 {
		t.Fatalf("expected HTTP 200, got %d: %s", response.Code, response.Body.String())
	}
	for header, expected := range map[string]string{
		"X-Total-Count": "5",
		"X-Page":        "2",
		"X-Page-Size":   "2",
		"X-Total-Pages": "3",
	} {
		if actual := response.Header().Get(header); actual != expected {
			t.Errorf("%s = %q, want %q", header, actual, expected)
		}
	}

	var logs []models.AuditLog
	if err := json.Unmarshal(response.Body.Bytes(), &logs); err != nil {
		t.Fatalf("decode logs response: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("page contains %d logs, want 2", len(logs))
	}
	if logs[0].Details != "2" || logs[1].Details != "1" {
		t.Errorf("page details = [%q, %q], want [2, 1]", logs[0].Details, logs[1].Details)
	}
}

func TestHandleLogsSearch(t *testing.T) {
	t.Chdir(t.TempDir())

	db, err := database.Init(":memory:")
	if err != nil {
		t.Fatalf("initialize test database: %v", err)
	}
	_ = db.LogAudit(models.AuditLog{
		EventType: "LOGIN", Protocol: "SFTP", Username: "user1", ClientIP: "192.168.1.100",
		Status: "SUCCESS", Details: "Login accepted",
	})
	_ = db.LogAudit(models.AuditLog{
		EventType: "INBOUND_FILE", Protocol: "SFTP", Username: "user2", ClientIP: "10.0.0.5",
		FileName: "invoice_12345.pdf", Status: "SUCCESS", Details: "Uploaded file",
	})
	_ = db.LogAudit(models.AuditLog{
		EventType: "LOGIN", Protocol: "SFTP", Username: "user1", ClientIP: "192.168.1.100",
		Status: "FAILED", Details: "Invalid password",
	})

	server := &Server{db: db}

	// 1. Search for "12345" -> matches invoice_12345.pdf
	req := httptest.NewRequest("GET", "/api/logs?search=12345", nil)
	rec := httptest.NewRecorder()
	server.handleLogs(rec, req)
	if rec.Code != 200 {
		t.Fatalf("search status = %d: %s", rec.Code, rec.Body.String())
	}
	var logs []models.AuditLog
	if err := json.Unmarshal(rec.Body.Bytes(), &logs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(logs) != 1 || logs[0].FileName != "invoice_12345.pdf" {
		t.Fatalf("expected 1 log with invoice_12345.pdf, got %d", len(logs))
	}

	// 2. Search for "192.168" -> matches 2 logs
	req = httptest.NewRequest("GET", "/api/logs?search=192.168", nil)
	rec = httptest.NewRecorder()
	server.handleLogs(rec, req)
	if rec.Code != 200 {
		t.Fatalf("search status = %d", rec.Code)
	}
	logs = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &logs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("expected 2 logs for 192.168, got %d", len(logs))
	}

	// 3. Search for "Invalid" -> matches 1 log
	req = httptest.NewRequest("GET", "/api/logs?search=Invalid", nil)
	rec = httptest.NewRecorder()
	server.handleLogs(rec, req)
	logs = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &logs)
	if len(logs) != 1 || logs[0].Status != "FAILED" {
		t.Fatalf("expected 1 failed log, got %d", len(logs))
	}

	// 4. Search specific column "file_details" with "invoice"
	req = httptest.NewRequest("GET", "/api/logs?search=invoice&search_column=file_details", nil)
	rec = httptest.NewRecorder()
	server.handleLogs(rec, req)
	logs = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &logs)
	if len(logs) != 1 || logs[0].FileName != "invoice_12345.pdf" {
		t.Fatalf("expected 1 log for file_details column search, got %d", len(logs))
	}

	// 5. Search specific column "event_type" with "invoice" -> should return 0
	req = httptest.NewRequest("GET", "/api/logs?search=invoice&search_column=event_type", nil)
	rec = httptest.NewRecorder()
	server.handleLogs(rec, req)
	logs = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &logs)
	if len(logs) != 0 {
		t.Fatalf("expected 0 logs when searching event_type for invoice, got %d", len(logs))
	}

	// 6. Search Thai status "ล้มเหลว" -> should match FAILED
	req = httptest.NewRequest("GET", "/api/logs?search=ล้มเหลว&search_column=status", nil)
	rec = httptest.NewRecorder()
	server.handleLogs(rec, req)
	logs = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &logs)
	if len(logs) != 1 || logs[0].Status != "FAILED" {
		t.Fatalf("expected 1 log matching Thai status ล้มเหลว, got %d", len(logs))
	}
}

func TestHandleSFTPSecuritySettings(t *testing.T) {
	t.Chdir(t.TempDir())

	db, err := database.Init(":memory:")
	if err != nil {
		t.Fatalf("initialize test database: %v", err)
	}
	server := &Server{db: db}

	// 1. GET default settings
	req := httptest.NewRequest("GET", "/api/security/sftp", nil)
	rec := httptest.NewRecorder()
	server.handleSFTPSecurity(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET /api/security/sftp status = %d: %s", rec.Code, rec.Body.String())
	}
	var getResp struct {
		Settings models.SFTPSecuritySettings `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if getResp.Settings.MaxFailedAttempts != 5 {
		t.Errorf("expected default max_failed_attempts=5, got %d", getResp.Settings.MaxFailedAttempts)
	}
	if getResp.Settings.LockoutMinutes != 60 {
		t.Errorf("expected default lockout_minutes=60, got %d", getResp.Settings.LockoutMinutes)
	}

	// 2. Reject negative max_failed_attempts
	negPayload := `{"ip_mode":"allow_all","lockout_minutes":15,"max_failed_attempts":-1}`
	req = httptest.NewRequest("PUT", "/api/security/sftp", strings.NewReader(negPayload))
	rec = httptest.NewRecorder()
	server.handleSFTPSecurity(rec, req)
	if rec.Code != 400 {
		t.Fatalf("PUT negative max_failed_attempts expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// 3. Reject non-positive lockout_minutes
	zeroLockout := `{"ip_mode":"allow_all","lockout_minutes":0,"max_failed_attempts":5}`
	req = httptest.NewRequest("PUT", "/api/security/sftp", strings.NewReader(zeroLockout))
	rec = httptest.NewRecorder()
	server.handleSFTPSecurity(rec, req)
	if rec.Code != 400 {
		t.Fatalf("PUT 0 lockout_minutes expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// 4. Update valid settings (min 0 is allowed)
	validPayload := `{"ip_mode":"allow_all","lockout_minutes":30,"max_failed_attempts":0}`
	req = httptest.NewRequest("PUT", "/api/security/sftp", strings.NewReader(validPayload))
	rec = httptest.NewRecorder()
	server.handleSFTPSecurity(rec, req)
	if rec.Code != 200 {
		t.Fatalf("PUT valid settings expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify updated
	settings, _, err := db.GetSFTPAccessControl()
	if err != nil {
		t.Fatalf("GetSFTPAccessControl: %v", err)
	}
	if settings.MaxFailedAttempts != 0 || settings.LockoutMinutes != 30 {
		t.Errorf("expected max_failed_attempts=0 lockout=30, got max=%d lockout=%d", settings.MaxFailedAttempts, settings.LockoutMinutes)
	}
}

func TestHandleSFTPBlockedIPsUnblockAuditStatus(t *testing.T) {
	t.Chdir(t.TempDir())

	db, err := database.Init(":memory:")
	if err != nil {
		t.Fatalf("initialize test database: %v", err)
	}
	server := &Server{db: db}

	// Trigger failure to block IP
	now := time.Now()
	for i := 0; i < 5; i++ {
		_, _, _, _ = db.RecordSFTPAuthFailure("171.5.229.191", "testuser", now)
	}

	// Unblock IP via DELETE /api/security/sftp/blocked?ip=171.5.229.191
	req := httptest.NewRequest("DELETE", "/api/security/sftp/blocked?ip=171.5.229.191", nil)
	rec := httptest.NewRecorder()
	server.handleSFTPBlockedIPs(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Check audit log for UNBLOCKED status
	logs, err := db.GetRecentLogs(5, "SFTP_SECURITY")
	if err != nil {
		t.Fatalf("GetRecentLogs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected at least 1 SFTP_SECURITY log")
	}
	latest := logs[0]
	if latest.Status != "UNBLOCKED" {
		t.Errorf("expected unblock log status = UNBLOCKED, got %s", latest.Status)
	}
	if latest.ClientIP != "171.5.229.191" {
		t.Errorf("expected client_ip = 171.5.229.191, got %s", latest.ClientIP)
	}
}

func TestHandlePGPGenerateAndValidate(t *testing.T) {
	t.Chdir(t.TempDir())
	db, err := database.Init(":memory:")
	if err != nil {
		t.Fatalf("initialize test database: %v", err)
	}
	server := &Server{db: db}

	// 1. Generate PGP Key
	body := strings.NewReader(`{"name":"Test Issuer","email":"issuer@example.com"}`)
	req := httptest.NewRequest("POST", "/api/pgp/generate", body)
	rec := httptest.NewRecorder()
	server.handlePGPGenerate(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var res struct {
		PublicKey  string `json:"public_key"`
		PrivateKey string `json:"private_key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(res.PublicKey, "BEGIN PGP PUBLIC KEY BLOCK") {
		t.Fatalf("missing public key block in response")
	}

	// 2. Validate Public Key
	validPayload, _ := json.Marshal(map[string]string{
		"key_type": "public",
		"key_data": res.PublicKey,
	})
	req = httptest.NewRequest("POST", "/api/pgp/validate", strings.NewReader(string(validPayload)))
	rec = httptest.NewRecorder()
	server.handlePGPValidate(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200 for valid pub key, got %d", rec.Code)
	}

	// 3. Validate Invalid Key
	invalidPayload, _ := json.Marshal(map[string]string{
		"key_type": "public",
		"key_data": "not-a-pgp-key",
	})
	req = httptest.NewRequest("POST", "/api/pgp/validate", strings.NewReader(string(invalidPayload)))
	rec = httptest.NewRecorder()
	server.handlePGPValidate(rec, req)
	if rec.Code == 200 {
		t.Fatal("expected error for invalid key, got 200")
	}
}
