package scheduler

import (
	"log"
	"sync"

	"odoo-scb-bridge/internal/database"
	"odoo-scb-bridge/internal/outbound"

	"github.com/robfig/cron/v3"
)

type Manager struct {
	db      *database.DB
	worker  *outbound.Worker
	cron    *cron.Cron
	entryIDs map[int64]cron.EntryID
	mu      sync.Mutex
}

func NewManager(db *database.DB) *Manager {
	return &Manager{
		db:       db,
		worker:   outbound.NewWorker(db),
		cron:     cron.New(cron.WithSeconds()), // Supports 6-field standard with seconds
		entryIDs: make(map[int64]cron.EntryID),
	}
}

// Start begins the cron scheduler and loads enabled jobs
func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.ReloadJobs()
	m.cron.Start()
	log.Println("[Scheduler] Cron manager started")
}

// Stop shuts down the cron scheduler
func (m *Manager) Stop() {
	m.cron.Stop()
	log.Println("[Scheduler] Cron manager stopped")
}

// ReloadJobs refreshes all active jobs from the database
func (m *Manager) ReloadJobs() {
	// Clear existing entries
	for _, entryID := range m.entryIDs {
		m.cron.Remove(entryID)
	}
	m.entryIDs = make(map[int64]cron.EntryID)

	jobs, err := m.db.ListOutboundJobs()
	if err != nil {
		log.Printf("[Scheduler] Error loading jobs: %v", err)
		return
	}

	for _, job := range jobs {
		if !job.Enabled {
			continue
		}

		j := job // capture loop variable
		id, err := m.cron.AddFunc(j.CronExpr, func() {
			log.Printf("[Scheduler] Executing Job ID: %d (%s)", j.ID, j.Name)
			if err := m.worker.ExecuteJob(&j); err != nil {
				log.Printf("[Scheduler] Job %d failed: %v", j.ID, err)
			}
		})

		if err != nil {
			log.Printf("[Scheduler] Failed to schedule job %s (Cron: %s): %v", j.Name, j.CronExpr, err)
			continue
		}

		m.entryIDs[j.ID] = id
		log.Printf("[Scheduler] Scheduled Job %s with cron %s", j.Name, j.CronExpr)
	}
}
