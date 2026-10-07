package utils

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
)

func TestPGPKeyGenAndEncryption(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Generate Key Pair
	pubKey, privKey, err := GeneratePGPKeyPair("Test User", "test@example.com", "Odoo SCB Bridge")
	if err != nil {
		t.Fatalf("GeneratePGPKeyPair: %v", err)
	}

	if !strings.Contains(pubKey, "-----BEGIN PGP PUBLIC KEY BLOCK-----") {
		t.Fatalf("unexpected pubKey header: %s", pubKey)
	}
	if !strings.Contains(privKey, "-----BEGIN PGP PRIVATE KEY BLOCK-----") {
		t.Fatalf("unexpected privKey header: %s", privKey)
	}

	// 2. Validate Keys
	if err := ValidatePGPPublicKey(pubKey); err != nil {
		t.Fatalf("ValidatePGPPublicKey: %v", err)
	}
	if err := ValidatePGPPrivateKey(privKey, ""); err != nil {
		t.Fatalf("ValidatePGPPrivateKey: %v", err)
	}

	// 3. Encrypt a file
	srcPath := filepath.Join(tmpDir, "plain.txt")
	testData := "Hello Odoo SCB Bridge Financial Transfer 2026!"
	if err := os.WriteFile(srcPath, []byte(testData), 0644); err != nil {
		t.Fatalf("write src file: %v", err)
	}

	dstPath := filepath.Join(tmpDir, "encrypted.pgp")
	if err := EncryptFilePGP(srcPath, dstPath, pubKey, privKey, ""); err != nil {
		t.Fatalf("EncryptFilePGP: %v", err)
	}

	encBytes, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("read encrypted file: %v", err)
	}
	if !strings.Contains(string(encBytes), "-----BEGIN PGP MESSAGE-----") {
		t.Fatalf("expected PGP MESSAGE block, got: %s", string(encBytes))
	}

	// 4. Decrypt and verify
	block, err := armor.Decode(bytes.NewReader(encBytes))
	if err != nil {
		t.Fatalf("armor decode: %v", err)
	}

	privKeyEntities, err := openpgp.ReadArmoredKeyRing(strings.NewReader(privKey))
	if err != nil {
		t.Fatalf("read priv key ring: %v", err)
	}

	md, err := openpgp.ReadMessage(block.Body, privKeyEntities, nil, nil)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}

	decryptedContent, err := io.ReadAll(md.UnverifiedBody)
	if err != nil {
		t.Fatalf("read decrypted body: %v", err)
	}

	if string(decryptedContent) != testData {
		t.Fatalf("decrypted = %q, want %q", string(decryptedContent), testData)
	}
}

func TestPGPWithoutBlankLine(t *testing.T) {
	pubKey, privKey, err := GeneratePGPKeyPair("Test", "test@test.com", "")
	if err != nil {
		t.Fatal(err)
	}

	// 1. Test public key with blank line removed
	noBlankPub := strings.Replace(pubKey, "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\n", "-----BEGIN PGP PUBLIC KEY BLOCK-----\n", 1)
	if err := ValidatePGPPublicKey(noBlankPub); err != nil {
		t.Fatalf("ValidatePGPPublicKey on noBlankPub failed: %v", err)
	}

	// 2. Test private key with blank line removed
	noBlankPriv := strings.Replace(privKey, "-----BEGIN PGP PRIVATE KEY BLOCK-----\n\n", "-----BEGIN PGP PRIVATE KEY BLOCK-----\n", 1)
	if err := ValidatePGPPrivateKey(noBlankPriv, ""); err != nil {
		t.Fatalf("ValidatePGPPrivateKey on noBlankPriv failed: %v", err)
	}

	// 3. Test NormalizePGPArmor directly
	normalizedPub := NormalizePGPArmor(noBlankPub)
	if !strings.Contains(normalizedPub, "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\n") {
		t.Fatalf("NormalizePGPArmor did not restore blank line: %s", normalizedPub)
	}
}
