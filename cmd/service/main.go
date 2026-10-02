package main

import (
	"log"
	"os"
	"path/filepath"

	"odoo-scb-bridge/internal/api"
	"odoo-scb-bridge/internal/database"
	"odoo-scb-bridge/internal/scheduler"
	"odoo-scb-bridge/internal/server/sftp"

	"github.com/kardianos/service"
)

type program struct {
	exit       chan struct{}
	sftpServer *sftp.Server
	apiServer  *api.Server
	scheduler  *scheduler.Manager
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

	// Read Inbound config
	inboundCfg, err := db.GetInboundConfig()
	if err != nil {
		log.Fatalf("[Service] Config error: %v", err)
	}

	// 1. Start Embedded Inbound SFTP Server
	p.sftpServer = sftp.NewServer(inboundCfg.SFTPPort, inboundCfg.TargetDir, inboundCfg.TempDir, db)
	if err := p.sftpServer.Start(); err != nil {
		log.Printf("[Service] Failed to start SFTP: %v", err)
	}

	// 2. Start Outbound Cron Scheduler
	p.scheduler = scheduler.NewManager(db)
	p.scheduler.Start()

	// 3. Start Local REST API Server (Port 9527) for Desktop UI
	p.apiServer = api.NewServer(9527, db, p.scheduler, p.sftpServer)
	go func() {
		if err := p.apiServer.Start(); err != nil {
			log.Printf("[Service] API server stopped: %v", err)
		}
	}()

	log.Println("[Service] Data Bridge Microservice running successfully")
	<-p.exit
}

func (p *program) Stop(s service.Service) error {
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
