package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"odoo-scb-bridge/internal/appversion"
	"odoo-scb-bridge/internal/database"
	"odoo-scb-bridge/internal/license"
	"odoo-scb-bridge/internal/models"
	"odoo-scb-bridge/internal/outbound"
	"odoo-scb-bridge/internal/scheduler"
	"odoo-scb-bridge/internal/server/sftp"
	"odoo-scb-bridge/internal/ui"
)

type Server struct {
	port         int
	db           *database.DB
	scheduler    *scheduler.Manager
	sftpServer   *sftp.Server
	license      *license.Manager
	onActivate   func()
	onDeactivate func()
	srv          *http.Server
	mu           sync.Mutex
	running      bool
}

func NewServer(port int, db *database.DB, sch *scheduler.Manager, sftpSrv *sftp.Server, licenseManager *license.Manager, onActivate, onDeactivate func()) *Server {
	return &Server{
		port:         port,
		db:           db,
		scheduler:    sch,
		sftpServer:   sftpSrv,
		license:      licenseManager,
		onActivate:   onActivate,
		onDeactivate: onDeactivate,
		running:      licenseManager != nil && licenseManager.Activated(),
	}
}

func (s *Server) Start() error {
	mux := http.NewServeMux()

	// CORS wrapper
	cors := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Expose-Headers", "X-Total-Count, X-Page, X-Page-Size, X-Total-Pages")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusOK)
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("/api/status", cors(s.handleStatus))
	mux.HandleFunc("/api/activation/request", cors(s.handleActivationRequest))
	mux.HandleFunc("/api/activation/activate", cors(s.handleActivation))
	mux.HandleFunc("/api/activation/deactivate", cors(s.handleLicenseDeactivation))
	mux.HandleFunc("/api/service/start", cors(s.requireLicense(s.handleServiceStart)))
	mux.HandleFunc("/api/service/stop", cors(s.requireLicense(s.handleServiceStop)))
	mux.HandleFunc("/api/logs", cors(s.requireLicense(s.handleLogs)))
	mux.HandleFunc("/api/security/pin-lockout", cors(s.handlePinLockout))
	mux.HandleFunc("/api/security/sftp", cors(s.requireLicense(s.handleSFTPSecurity)))
	mux.HandleFunc("/api/security/sftp/rules", cors(s.requireLicense(s.handleSFTPIPRules)))
	mux.HandleFunc("/api/security/sftp/blocked", cors(s.requireLicense(s.handleSFTPBlockedIPs)))
	mux.HandleFunc("/api/inbound", cors(s.requireLicense(s.handleInboundConfig)))
	mux.HandleFunc("/api/users", cors(s.requireLicense(s.handleUsers)))
	mux.HandleFunc("/api/outbound/jobs", cors(s.requireLicense(s.handleOutboundJobs)))
	mux.HandleFunc("/api/outbound/jobs/", cors(s.requireLicense(s.handleOutboundJobLogs)))
	mux.HandleFunc("/api/outbound/test-connection", cors(s.requireLicense(s.handleOutboundTestConnection)))
	mux.HandleFunc("/locales.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		_, _ = w.Write([]byte(ui.LocalesContent))
	})

	// Serve Frontend Web Console directly from the microservice
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(ui.HTMLContent))
	})

	s.srv = &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", s.port),
		Handler: mux,
	}

	return s.srv.ListenAndServe()
}

func (s *Server) Stop() error {
	if s.srv != nil {
		return s.srv.Close()
	}
	return nil
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()

	statusStr := "RUNNING"
	if !running {
		statusStr = "STOPPED"
	}

	sftpRunning := false
	if s.sftpServer != nil {
		sftpRunning = s.sftpServer.IsRunning()
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":       statusStr,
		"running":      running,
		"sftp_running": sftpRunning,
		"activated":    s.license != nil && s.license.Activated(),
		"machine_id":   s.machineID(),
		"version":      appversion.Current,
	})
}

func (s *Server) machineID() string {
	if s.license == nil {
		return ""
	}
	return s.license.MachineID()
}

func (s *Server) requireLicense(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.license == nil || !s.license.Activated() {
			jsonResponse(w, http.StatusForbidden, map[string]interface{}{"error": "product activation required", "activated": false})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleActivationRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.license == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]string{"error": "activation unavailable"})
		return
	}
	requestCode, err := s.license.RequestCode(r.URL.Query().Get("computer_name"), r.URL.Query().Get("profile_name"))
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"activated":    s.license.Activated(),
		"machine_id":   s.license.MachineID(),
		"request_code": requestCode,
	})
}

func (s *Server) handleActivation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.license == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]string{"error": "activation unavailable"})
		return
	}
	var request struct {
		Code string `json:"license_code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&request); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid activation payload"})
		return
	}
	if err := s.license.Activate(request.Code); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if s.onActivate != nil {
		s.onActivate()
	}
	s.mu.Lock()
	s.running = true
	s.mu.Unlock()
	jsonResponse(w, http.StatusOK, map[string]interface{}{"activated": true})
}

func (s *Server) handleLicenseDeactivation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.license == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]string{"error": "activation unavailable"})
		return
	}
	if err := s.license.Deactivate(); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if s.db != nil {
		_ = s.db.LogAudit(models.AuditLog{
			EventType: "LICENSE",
			Protocol:  "SYSTEM",
			Username:  "admin",
			Status:    "WARNING",
			Details:   "Product license deactivated locally via Desktop UI",
		})
	}
	if s.onDeactivate != nil {
		s.onDeactivate()
	}
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()
	jsonResponse(w, http.StatusOK, map[string]interface{}{"activated": false})
}

func (s *Server) handleServiceStart(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.running = true
	s.mu.Unlock()

	if s.sftpServer != nil {
		_ = s.sftpServer.Start()
	}
	if s.scheduler != nil {
		s.scheduler.Start()
	}

	_ = s.db.LogAudit(models.AuditLog{
		EventType: "SERVICE",
		Protocol:  "SYSTEM",
		Username:  "admin",
		Status:    "SUCCESS",
		Details:   "Bridge Service started via Desktop UI",
	})

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":  "started",
		"running": true,
	})
}

func (s *Server) handleServiceStop(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()

	if s.sftpServer != nil {
		s.sftpServer.Stop()
	}
	if s.scheduler != nil {
		s.scheduler.Stop()
	}

	_ = s.db.LogAudit(models.AuditLog{
		EventType: "SERVICE",
		Protocol:  "SYSTEM",
		Username:  "admin",
		Status:    "WARNING",
		Details:   "Bridge Service stopped via Desktop UI",
	})

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":  "stopped",
		"running": false,
	})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	pageSize := 10
	pageSizeStr := query.Get("page_size")
	if pageSizeStr == "" {
		pageSizeStr = query.Get("limit")
	}
	if size, err := strconv.Atoi(pageSizeStr); err == nil && size > 0 {
		pageSize = size
	}
	if pageSize > 100 {
		pageSize = 100
	}
	page := 1
	if requestedPage, err := strconv.Atoi(query.Get("page")); err == nil && requestedPage > 0 {
		page = requestedPage
	}
	eventType := query.Get("event_type")
	username := strings.TrimSpace(query.Get("username"))
	total, err := s.db.CountLogsFiltered(eventType, username)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}
	offset := (page - 1) * pageSize
	logs, err := s.db.GetRecentLogsFiltered(pageSize, eventType, username, offset)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	w.Header().Set("X-Page", strconv.Itoa(page))
	w.Header().Set("X-Page-Size", strconv.Itoa(pageSize))
	w.Header().Set("X-Total-Pages", strconv.Itoa(totalPages))
	jsonResponse(w, http.StatusOK, logs)
}

// handleOutboundJobLogs serves a dedicated log history for a single outbound job.
func (s *Server) handleOutboundJobLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/outbound/jobs/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[1] != "logs" {
		http.NotFound(w, r)
		return
	}
	jobID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || jobID <= 0 {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid outbound job id"})
		return
	}
	pageSize := 10
	if size, err := strconv.Atoi(r.URL.Query().Get("page_size")); err == nil && size > 0 {
		pageSize = size
	}
	if pageSize > 100 {
		pageSize = 100
	}
	page := 1
	if requestedPage, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && requestedPage > 0 {
		page = requestedPage
	}
	total, err := s.db.CountOutboundJobLogs(jobID)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}
	logs, err := s.db.GetOutboundJobLogs(jobID, pageSize, (page-1)*pageSize)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	w.Header().Set("X-Page", strconv.Itoa(page))
	w.Header().Set("X-Page-Size", strconv.Itoa(pageSize))
	w.Header().Set("X-Total-Pages", strconv.Itoa(totalPages))
	jsonResponse(w, http.StatusOK, logs)
}

func (s *Server) handleSFTPSecurity(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		settings, rules, err := s.db.GetSFTPAccessControl()
		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		blocked, err := s.db.ListSFTPBlockedIPs()
		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, map[string]interface{}{"settings": settings, "rules": rules, "blocked_ips": blocked})
	case http.MethodPut, http.MethodPost:
		var request struct {
			IPMode string `json:"ip_mode"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&request); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid settings"})
			return
		}
		if request.IPMode != "allow_all" && request.IPMode != "allow_list" {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "ip_mode must be allow_all or allow_list"})
			return
		}
		if err := s.db.SetSFTPIPMode(request.IPMode); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		_ = s.db.LogAudit(models.AuditLog{EventType: "SFTP_SECURITY", Protocol: "SFTP", Username: "admin", Status: "SUCCESS", Details: "SFTP IP access mode changed to " + request.IPMode})
		jsonResponse(w, http.StatusOK, map[string]string{"status": "updated"})
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSFTPIPRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var request struct {
			CIDR   string `json:"cidr"`
			Action string `json:"action"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&request); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid IP rule"})
			return
		}
		request.CIDR = strings.TrimSpace(request.CIDR)
		request.Action = strings.ToLower(strings.TrimSpace(request.Action))
		if request.Action != "allow" && request.Action != "block" {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "action must be allow or block"})
			return
		}
		if ip := net.ParseIP(request.CIDR); ip != nil {
			request.CIDR = ip.String()
		} else if _, network, err := net.ParseCIDR(request.CIDR); err == nil {
			request.CIDR = network.String()
		} else {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "enter a valid IP address or CIDR range"})
			return
		}
		if err := s.db.AddSFTPIPRule(request.CIDR, request.Action, nil); err != nil {
			jsonResponse(w, http.StatusConflict, map[string]string{"error": "this IP rule already exists"})
			return
		}
		status := "SUCCESS"
		details := fmt.Sprintf("Permanent SFTP IP %s rule added for %s", request.Action, request.CIDR)
		if request.Action == "block" {
			status = "BLOCKED"
		}
		_ = s.db.LogAudit(models.AuditLog{EventType: "SFTP_SECURITY", Protocol: "SFTP", Username: "admin", ClientIP: request.CIDR, Status: status, Details: details})
		jsonResponse(w, http.StatusCreated, map[string]string{"status": "created"})
	case http.MethodDelete:
		id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if err != nil || id <= 0 {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "valid rule id is required"})
			return
		}
		if err := s.db.DeleteSFTPIPRule(id); err != nil {
			jsonResponse(w, http.StatusNotFound, map[string]string{"error": "IP rule not found"})
			return
		}
		_ = s.db.LogAudit(models.AuditLog{EventType: "SFTP_SECURITY", Protocol: "SFTP", Username: "admin", Status: "SUCCESS", Details: fmt.Sprintf("SFTP IP rule %d removed", id)})
		jsonResponse(w, http.StatusOK, map[string]string{"status": "deleted"})
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSFTPBlockedIPs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		blocked, err := s.db.ListSFTPBlockedIPs()
		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, blocked)
	case http.MethodDelete:
		ip := net.ParseIP(strings.TrimSpace(r.URL.Query().Get("ip")))
		if ip == nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "valid IP address is required"})
			return
		}
		canonicalIP := ip.String()
		if err := s.db.UnblockSFTPIP(canonicalIP); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		_ = s.db.LogAudit(models.AuditLog{EventType: "SFTP_SECURITY", Protocol: "SFTP", Username: "admin", ClientIP: canonicalIP, Status: "SUCCESS", Details: "Temporary SFTP IP block cleared"})
		jsonResponse(w, http.StatusOK, map[string]string{"status": "unblocked"})
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handlePinLockout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var event struct {
		Timestamp     time.Time `json:"timestamp"`
		Attempts      int       `json:"attempts"`
		LockoutSecond int       `json:"lockout_seconds"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&event); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid security event"})
		return
	}
	if event.Attempts < 1 || event.Attempts > 100 || event.LockoutSecond < 1 || event.LockoutSecond > 3600 {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid security event"})
		return
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	details := fmt.Sprintf("Application PIN locked after %d failed attempts for %d seconds", event.Attempts, event.LockoutSecond)
	if err := s.db.LogAudit(models.AuditLog{
		EventType: "PIN_LOCKOUT",
		Protocol:  "APP",
		Username:  "application",
		ClientIP:  "127.0.0.1",
		Status:    "LOCKED",
		Details:   details,
		Timestamp: event.Timestamp,
	}); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "could not store security event"})
		return
	}
	jsonResponse(w, http.StatusCreated, map[string]string{"status": "recorded"})
}

func (s *Server) handleInboundConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cfg, err := s.db.GetInboundConfig()
		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, cfg)
		return
	}

	if r.Method == http.MethodPut || r.Method == http.MethodPost {
		var cfg models.InboundConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
			return
		}
		if err := s.db.UpdateInboundConfig(&cfg); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"status": "updated"})
		return
	}

	http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		users, err := s.db.ListUsers()
		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, users)
		return
	}

	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		var u models.User
		if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
			return
		}
		if err := s.db.SaveUser(&u); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"status": "saved"})
		return
	}

	if r.Method == http.MethodDelete {
		idStr := r.URL.Query().Get("id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid user id"})
			return
		}
		if err := s.db.DeleteUser(id); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"status": "deleted"})
		return
	}

	http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
}

func (s *Server) handleOutboundJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		jobs, err := s.db.ListOutboundJobs()
		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, jobs)
		return
	}

	if r.Method == http.MethodPost {
		var job models.OutboundJob
		if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
			return
		}
		if err := s.db.SaveOutboundJob(&job); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		// Refresh active cron jobs
		s.scheduler.ReloadJobs()
		jsonResponse(w, http.StatusOK, map[string]string{"status": "saved"})
		return
	}

	if r.Method == http.MethodDelete {
		idStr := r.URL.Query().Get("id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid job id"})
			return
		}
		if err := s.db.DeleteOutboundJob(id); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.scheduler.ReloadJobs()
		jsonResponse(w, http.StatusOK, map[string]string{"status": "deleted"})
		return
	}

	http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
}

func (s *Server) handleOutboundTestConnection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var job models.OutboundJob
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&job); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid connection settings"})
		return
	}
	if err := outbound.TestConnection(&job); err != nil {
		jsonResponse(w, http.StatusBadGateway, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func jsonResponse(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}
