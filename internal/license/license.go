package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	ProductID       = "odoo-scb-bridge"
	requestPrefix   = "OSCB-REQUEST-1."
	licensePrefix   = "OSCB-LICENSE-1."
	publicKeyBase64 = "549Qfd8SyZsB9i/UxAKvFCUEBn8Wm5tJnj1fq7Az4Xw"
)

type Request struct {
	ProductID string `json:"product_id"`
	MachineID string `json:"machine_id"`
	HostName  string `json:"computer_name,omitempty"`
	Profile   string `json:"profile_name,omitempty"`
}

type Payload struct {
	ProductID string `json:"product_id"`
	MachineID string `json:"machine_id"`
	LicenseID string `json:"license_id"`
	IssuedAt  string `json:"issued_at"`
}

type Manager struct {
	path      string
	machineID string
	mu        sync.RWMutex
	active    bool
}

func NewManager(path string) *Manager {
	m := &Manager{path: path, machineID: MachineID()}
	m.active = m.loadAndVerify() == nil
	return m
}

func (m *Manager) MachineID() string { return m.machineID }

func (m *Manager) Activated() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active
}

func (m *Manager) RequestCode(hostName, profile string) (string, error) {
	return EncodeRequest(Request{ProductID: ProductID, MachineID: m.machineID, HostName: hostName, Profile: profile})
}

func (m *Manager) Activate(code string) error {
	payload, err := Verify(code, m.machineID)
	if err != nil {
		return err
	}
	_ = payload
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(m.path), 0750); err != nil {
		return fmt.Errorf("create license directory: %w", err)
	}
	if err := os.WriteFile(m.path, []byte(strings.TrimSpace(code)), 0600); err != nil {
		return fmt.Errorf("save license: %w", err)
	}
	m.active = true
	return nil
}

// Deactivate removes the locally stored license and disables licensed features.
func (m *Manager) Deactivate() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.Remove(m.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove license: %w", err)
	}
	m.active = false
	return nil
}

func (m *Manager) loadAndVerify() error {
	data, err := os.ReadFile(m.path)
	if err != nil {
		return err
	}
	_, err = Verify(string(data), m.machineID)
	return err
}

func EncodeRequest(req Request) (string, error) {
	if req.ProductID != ProductID || req.MachineID == "" {
		return "", errors.New("invalid activation request")
	}
	data, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	return requestPrefix + base64.RawURLEncoding.EncodeToString(data), nil
}

func DecodeRequest(code string) (Request, error) {
	var req Request
	code = strings.TrimSpace(code)
	if !strings.HasPrefix(code, requestPrefix) {
		return req, errors.New("invalid request code format")
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(code, requestPrefix))
	if err != nil || json.Unmarshal(data, &req) != nil || req.ProductID != ProductID || req.MachineID == "" {
		return Request{}, errors.New("invalid activation request code")
	}
	return req, nil
}

func Issue(requestCode, privateKeyPath string) (string, error) {
	req, err := DecodeRequest(requestCode)
	if err != nil {
		return "", err
	}
	keyData, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return "", fmt.Errorf("read issuer private key: %w", err)
	}
	block, _ := pem.Decode(keyData)
	if block == nil {
		return "", errors.New("issuer key is not valid PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse issuer private key: %w", err)
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return "", errors.New("issuer key must be Ed25519")
	}
	publicKey, err := publicKey()
	if err != nil || !privateKey.Public().(ed25519.PublicKey).Equal(publicKey) {
		return "", errors.New("issuer private key does not match this application's public key")
	}
	licenseID := make([]byte, 16)
	if _, err := rand.Read(licenseID); err != nil {
		return "", err
	}
	payload := Payload{
		ProductID: ProductID,
		MachineID: req.MachineID,
		LicenseID: base64.RawURLEncoding.EncodeToString(licenseID),
		IssuedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signature := ed25519.Sign(privateKey, payloadBytes)
	return licensePrefix + base64.RawURLEncoding.EncodeToString(payloadBytes) + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func Verify(code, machineID string) (Payload, error) {
	var payload Payload
	parts := strings.Split(strings.TrimSpace(code), ".")
	if len(parts) != 3 || parts[0] != strings.TrimSuffix(licensePrefix, ".") {
		return payload, errors.New("invalid license code format")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payloadBytes, &payload) != nil {
		return Payload{}, errors.New("invalid license data")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Payload{}, errors.New("invalid license signature")
	}
	key, err := publicKey()
	if err != nil || !ed25519.Verify(key, payloadBytes, signature) {
		return Payload{}, errors.New("license signature is not valid")
	}
	if payload.ProductID != ProductID {
		return Payload{}, errors.New("license is for a different product")
	}
	if payload.MachineID != machineID {
		return Payload{}, errors.New("license is for a different computer")
	}
	if payload.LicenseID == "" || payload.IssuedAt == "" {
		return Payload{}, errors.New("license is incomplete")
	}
	return payload, nil
}

func publicKey() (ed25519.PublicKey, error) {
	key, err := base64.RawStdEncoding.DecodeString(publicKeyBase64)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("application license public key is not configured")
	}
	return ed25519.PublicKey(key), nil
}
