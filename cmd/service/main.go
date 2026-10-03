package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"odoo-scb-bridge/internal/api"
	"odoo-scb-bridge/internal/appversion"
	"odoo-scb-bridge/internal/database"
	"odoo-scb-bridge/internal/license"
	"odoo-scb-bridge/internal/scheduler"
	"odoo-scb-bridge/internal/server/sftp"

	"github.com/kardianos/service"
)

type program struct {
	exit         chan struct{}
	sftpServer   *sftp.Server
	apiServer    *api.Server
	scheduler    *scheduler.Manager
	license      *license.Manager
	activationMu sync.Mutex
}

func (p *program) Start(s service.Service) error {
	p.exit = make(chan struct{})
	go p.run()
	return nil
}

func (p *program) run() {
	// Database location in common AppData or executable path
	exePath, err := os.Executable()
	baseDir := "."
	if err == nil {
		baseDir = filepath.Dir(exePath)
	}

	dbPath := filepath.Join(baseDir, "data", "bridge.db")
	db, err := database.Init(dbPath)
	if err != nil {
		log.Fatalf("[Service] DB Init error: %v", err)
	}
	versionInstallMarker := filepath.Join(baseDir, "data", "version-install-time.txt")
	var installedAt time.Time
	markerContents, markerErr := os.ReadFile(versionInstallMarker)
	if markerErr == nil {
		installedAt, err = time.ParseInLocation("2006-01-02 15:04:05", string(bytes.TrimSpace(markerContents)), time.Local)
		if err != nil {
			log.Printf("[Service] Could not parse installer timestamp: %v", err)
			installedAt = time.Time{}
		}
	} else if !os.IsNotExist(markerErr) {
		log.Printf("[Service] Could not read installer timestamp: %v", markerErr)
	}
	if err := db.RecordAppVersionAt(appversion.Current, installedAt); err != nil {
		log.Printf("[Service] Failed to record application version: %v", err)
	} else if markerErr == nil {
		if err := os.Remove(versionInstallMarker); err != nil {
			log.Printf("[Service] Could not remove installer timestamp: %v", err)
		}
	}

	// Read Inbound config
	inboundCfg, err := db.GetInboundConfig()
	if err != nil {
		log.Fatalf("[Service] Config error: %v", err)
	}

	// Keep activation and status APIs available before activation, but do not
	// expose SFTP or scheduled transfers until the signed license is valid.
	p.sftpServer = sftp.NewServer(inboundCfg.SFTPPort, inboundCfg.TargetDir, inboundCfg.TempDir, db)
	p.scheduler = scheduler.NewManager(db)
	p.license = license.NewManager(filepath.Join(baseDir, "data", "license.dat"))
	p.apiServer = api.NewServer(9527, db, p.scheduler, p.sftpServer, p.license, p.startLicensedServices, p.stopLicensedServices)
	go func() {
		if err := p.apiServer.Start(); err != nil {
			log.Printf("[Service] API server stopped: %v", err)
		}
	}()

	if p.license.Activated() {
		p.startLicensedServices()
		log.Println("[Service] Activated Data Bridge Microservice running successfully")
	} else {
		log.Println("[Service] Activation required; SFTP and scheduler remain disabled")
	}
	<-p.exit
}

func (p *program) startLicensedServices() {
	p.activationMu.Lock()
	defer p.activationMu.Unlock()
	if p.license == nil || !p.license.Activated() {
		return
	}
	if p.sftpServer != nil {
		if err := p.sftpServer.Start(); err != nil {
			log.Printf("[Service] Failed to start SFTP: %v", err)
		}
	}
	if p.scheduler != nil {
		p.scheduler.Start()
	}
}

func (p *program) stopLicensedServices() {
	p.activationMu.Lock()
	defer p.activationMu.Unlock()
	if p.sftpServer != nil {
		p.sftpServer.Stop()
	}
	if p.scheduler != nil {
		p.scheduler.Stop()
	}
}

func (p *program) Stop(s service.Service) error {
	p.activationMu.Lock()
	defer p.activationMu.Unlock()
	if p.sftpServer != nil {
		p.sftpServer.Stop()
	}
	if p.scheduler != nil {
		p.scheduler.Stop()
	}
	if p.apiServer != nil {
		_ = p.apiServer.Stop()
	}
	close(p.exit)
	return nil
}

func main() {
	svcConfig := &service.Config{
		Name:        "OdooSCBBridge",
		DisplayName: "Odoo SCB Bridge Data Service",
		Description: "Background microservice for SFTP/FTPS file exchange and automated job dispatching.",
		Option: service.KeyValue{
			"StartType": "automatic",
		},
	}

	prg := &program{}
	s, err := service.New(prg, svcConfig)
	if err != nil {
		log.Fatalf("Service init error: %v", err)
	}

	// Support CLI commands: install, uninstall, start, stop, run
	if len(os.Args) > 1 {
		cmd := os.Args[1]
		switch cmd {
		case "install":
			err = s.Install()
			if err != nil {
				log.Fatalf("Failed to install service: %v", err)
			}
			log.Println("Service installed successfully")
			return
		case "uninstall":
			err = s.Uninstall()
			if err != nil {
				log.Fatalf("Failed to uninstall service: %v", err)
			}
			log.Println("Service uninstalled successfully")
			return
		case "start":
			err = s.Start()
			if err != nil {
				log.Fatalf("Failed to start service: %v", err)
			}
			log.Println("Service started successfully")
			return
		case "stop":
			err = s.Stop()
			if err != nil {
				log.Fatalf("Failed to stop service: %v", err)
			}
			log.Println("Service stopped successfully")
			return
		}
	}

	if err := s.Run(); err != nil {
		log.Fatalf("Service run error: %v", err)
	}
}
