package api

import (
	"encoding/json"
	"net/http/httptest"
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
