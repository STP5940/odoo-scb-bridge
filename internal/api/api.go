package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"odoo-scb-bridge/internal/database"
	"odoo-scb-bridge/internal/license"
	"odoo-scb-bridge/internal/models"
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
		running:      true,
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
	mux.HandleFunc("/api/inbound", cors(s.requireLicense(s.handleInboundConfig)))
	mux.HandleFunc("/api/users", cors(s.requireLicense(s.handleUsers)))
	mux.HandleFunc("/api/outbound/jobs", cors(s.requireLicense(s.handleOutboundJobs)))
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
		"version":      "0.0.12",
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

	total, err := s.db.CountLogs(eventType)
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
	logs, err := s.db.GetRecentLogs(pageSize, eventType, offset)
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

func jsonResponse(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}
