package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"odoo-scb-bridge/internal/database"
	"odoo-scb-bridge/internal/models"
	"odoo-scb-bridge/internal/scheduler"
	"odoo-scb-bridge/internal/ui"
)

type Server struct {
	port      int
	db        *database.DB
	scheduler *scheduler.Manager
	srv       *http.Server
}

func NewServer(port int, db *database.DB, sch *scheduler.Manager) *Server {
	return &Server{
		port:      port,
		db:        db,
		scheduler: sch,
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
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusOK)
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("/api/status", cors(s.handleStatus))
	mux.HandleFunc("/api/service/start", cors(s.handleServiceStart))
	mux.HandleFunc("/api/service/stop", cors(s.handleServiceStop))
	mux.HandleFunc("/api/logs", cors(s.handleLogs))
	mux.HandleFunc("/api/inbound", cors(s.handleInboundConfig))
	mux.HandleFunc("/api/users", cors(s.handleUsers))
	mux.HandleFunc("/api/outbound/jobs", cors(s.handleOutboundJobs))

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
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":  "RUNNING",
		"version": "1.0.0",
	})
}

func (s *Server) handleServiceStart(w http.ResponseWriter, r *http.Request) {
	s.scheduler.Start()
	jsonResponse(w, http.StatusOK, map[string]string{"status": "started"})
}

func (s *Server) handleServiceStop(w http.ResponseWriter, r *http.Request) {
	s.scheduler.Stop()
	jsonResponse(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	limit := 100
	if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
		limit = l
	}
	eventType := r.URL.Query().Get("event_type")

	logs, err := s.db.GetRecentLogs(limit, eventType)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
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
