package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"odoo-scb-bridge/internal/license"
)

func main() {
	privateKeyPath := flag.String("key", "", "path to the private issuer key (never distribute this file)")
	flag.Parse()
	if *privateKeyPath == "" {
		resolved, searched := findPrivateKey()
		if resolved == "" {
			fatal("issuer private key not found; searched:\n  %s\nSpecify its location with --key <path>. Keep this private key secure and do not distribute it.", strings.Join(searched, "\n  "))
		}
		*privateKeyPath = resolved
	} else if _, err := os.Stat(*privateKeyPath); err != nil {
		fatal("cannot access issuer private key at %s: %v", *privateKeyPath, err)
	}

	fmt.Println("Odoo SCB Bridge - Offline License Issuer")
	fmt.Print("Paste activation request code: ")
	reader := bufio.NewReader(os.Stdin)
	requestCode, err := reader.ReadString('\n')
	if err != nil && len(requestCode) == 0 {
		fatal("read activation request: %v", err)
	}
	request, err := license.DecodeRequest(strings.TrimSpace(requestCode))
	if err != nil {
		fatal("invalid activation request: %v", err)
	}
	fmt.Printf("Computer: %s\nProfile: %s\nDevice ID: %s\n", request.HostName, request.Profile, request.MachineID)
	fmt.Print("Issue a license for this device? [y/N]: ")
	confirmation, _ := reader.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(confirmation)) != "y" {
		fmt.Println("License issuance cancelled.")
		return
	}
	licenseCode, err := license.Issue(requestCode, *privateKeyPath)
	if err != nil {
		fatal("issue license: %v", err)
	}
	fmt.Println("\nActivation license code:")
	fmt.Println(licenseCode)
}

func findPrivateKey() (string, []string) {
	var roots []string
	if cwd, err := os.Getwd(); err == nil {
		roots = append(roots, cwd)
	}
	if executable, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(executable)
		roots = append(roots, exeDir, filepath.Dir(exeDir))
	}

	seen := make(map[string]bool)
	var searched []string
	for _, root := range roots {
		candidate, err := filepath.Abs(filepath.Join(root, ".secrets", "issuer-ed25519-private.pem"))
		if err != nil || seen[candidate] {
			continue
		}
		seen[candidate] = true
		searched = append(searched, candidate)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, searched
		}
	}
	return "", searched
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", args...)
	os.Exit(1)
}
