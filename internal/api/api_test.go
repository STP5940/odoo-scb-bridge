package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

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
