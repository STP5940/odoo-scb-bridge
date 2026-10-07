package utils

import (
	"bytes"
	"crypto"
	_ "crypto/sha256"
	_ "crypto/sha512"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
	"golang.org/x/crypto/openpgp/packet"
	_ "golang.org/x/crypto/ripemd160"
)

// GeneratePGPKeyPair creates a new OpenPGP RSA keypair and returns ASCII-armored keys.
func GeneratePGPKeyPair(name, email, comment string) (string, string, error) {
	config := &packet.Config{
		DefaultHash:   crypto.SHA256,
		DefaultCipher: packet.CipherAES256,
		RSABits:       2048,
	}

	entity, err := openpgp.NewEntity(name, comment, email, config)
	if err != nil {
		return "", "", fmt.Errorf("create pgp entity: %w", err)
	}

	// Armor Public Key
	var pubBuf bytes.Buffer
	pubWriter, err := armor.Encode(&pubBuf, openpgp.PublicKeyType, nil)
	if err != nil {
		return "", "", fmt.Errorf("armor public key: %w", err)
	}
	if err := entity.Serialize(pubWriter); err != nil {
		_ = pubWriter.Close()
		return "", "", fmt.Errorf("serialize public key: %w", err)
	}
	if err := pubWriter.Close(); err != nil {
		return "", "", fmt.Errorf("close public key writer: %w", err)
	}

	// Armor Private Key
	var privBuf bytes.Buffer
	privWriter, err := armor.Encode(&privBuf, openpgp.PrivateKeyType, nil)
	if err != nil {
		return "", "", fmt.Errorf("armor private key: %w", err)
	}
	if err := entity.SerializePrivate(privWriter, config); err != nil {
		_ = privWriter.Close()
		return "", "", fmt.Errorf("serialize private key: %w", err)
	}
	if err := privWriter.Close(); err != nil {
		return "", "", fmt.Errorf("close private key writer: %w", err)
	}

	return pubBuf.String(), privBuf.String(), nil
}

// NormalizePGPArmor ensures standard RFC 4880 OpenPGP ASCII Armor format.
// If the empty line between armor headers and the base64 payload is missing,
// it automatically restores it so standard OpenPGP parsers can decode it seamlessly.
func NormalizePGPArmor(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")

	headers := []string{
		"-----BEGIN PGP PUBLIC KEY BLOCK-----",
		"-----BEGIN PGP PRIVATE KEY BLOCK-----",
		"-----BEGIN PGP MESSAGE-----",
		"-----BEGIN PGP SIGNATURE-----",
	}

	for _, h := range headers {
		idx := strings.Index(s, h)
		if idx == -1 {
			continue
		}

		afterHeader := s[idx+len(h):]
		endMarker := strings.Replace(h, "BEGIN", "END", 1)
		endIdx := strings.Index(afterHeader, endMarker)
		var body string
		var suffix string
		if endIdx != -1 {
			body = afterHeader[:endIdx]
			suffix = afterHeader[endIdx:]
		} else {
			body = afterHeader
		}

		lines := strings.Split(body, "\n")
		var headerLines []string
		var dataLines []string
		foundBlank := false

		for i, line := range lines {
			trimmedLine := strings.TrimSpace(line)
			if trimmedLine == "" && i > 0 && !foundBlank {
				foundBlank = true
				continue
			}
			if !foundBlank {
				if trimmedLine == "" {
					continue
				}
				if strings.Contains(line, ":") {
					headerLines = append(headerLines, line)
				} else {
					foundBlank = true
					dataLines = append(dataLines, line)
				}
			} else {
				if trimmedLine != "" {
					dataLines = append(dataLines, line)
				}
			}
		}

		var buf strings.Builder
		buf.WriteString(s[:idx+len(h)])
		buf.WriteString("\n")
		for _, hl := range headerLines {
			buf.WriteString(hl)
			buf.WriteString("\n")
		}
		// Mandatory RFC 4880 blank line before base64 payload
		buf.WriteString("\n")
		for _, dl := range dataLines {
			buf.WriteString(dl)
			buf.WriteString("\n")
		}
		if suffix != "" {
			buf.WriteString(strings.TrimSpace(suffix))
			buf.WriteString("\n")
		}

		s = buf.String()
	}

	return strings.TrimSpace(s) + "\n"
}

// EncryptFilePGP encrypts srcPath to dstPath using recipient's public key, optionally signing with signer's private key.
func EncryptFilePGP(srcPath, dstPath string, recipientPubKeyArmored, signerPrivKeyArmored, passphrase string) error {
	trimmedPub := NormalizePGPArmor(recipientPubKeyArmored)
	if trimmedPub == "" {
		return fmt.Errorf("recipient public key is required for PGP encryption")
	}

	// Parse Recipient Public Key
	recipientEntities, err := openpgp.ReadArmoredKeyRing(strings.NewReader(trimmedPub))
	if err != nil {
		return fmt.Errorf("invalid recipient public key: %w", err)
	}
	if len(recipientEntities) == 0 {
		return fmt.Errorf("no recipient public key found in key data")
	}

	// Parse Signer Private Key (optional)
	var signerEntity *openpgp.Entity
	trimmedPriv := NormalizePGPArmor(signerPrivKeyArmored)
	if trimmedPriv != "" {
		signerList, err := openpgp.ReadArmoredKeyRing(strings.NewReader(trimmedPriv))
		if err != nil {
			return fmt.Errorf("invalid signer private key: %w", err)
		}
		if len(signerList) > 0 {
			signerEntity = signerList[0]
			if signerEntity.PrivateKey != nil && signerEntity.PrivateKey.Encrypted {
				if passphrase == "" {
					return fmt.Errorf("passphrase required to decrypt signer private key")
				}
				if err := signerEntity.PrivateKey.Decrypt([]byte(passphrase)); err != nil {
					return fmt.Errorf("incorrect passphrase for signer private key: %w", err)
				}
			}
		}
	}

	srcFile, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open source file: %w", err)
	}
	defer srcFile.Close()

	stat, err := srcFile.Stat()
	if err != nil {
		return fmt.Errorf("stat source file: %w", err)
	}

	// Ensure destination directory exists
	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		return fmt.Errorf("create destination dir: %w", err)
	}

	dstFile, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("create destination file: %w", err)
	}
	defer dstFile.Close()

	// Write ASCII Armored PGP Message
	armoredWriter, err := armor.Encode(dstFile, "PGP MESSAGE", nil)
	if err != nil {
		return fmt.Errorf("encode armor: %w", err)
	}
	defer armoredWriter.Close()

	hints := &openpgp.FileHints{
		IsBinary: true,
		FileName: filepath.Base(srcPath),
		ModTime:  stat.ModTime(),
	}

	config := &packet.Config{
		DefaultCipher: packet.CipherAES256,
	}

	plaintextPipe, err := openpgp.Encrypt(armoredWriter, recipientEntities, signerEntity, hints, config)
	if err != nil {
		return fmt.Errorf("init pgp encryption: %w", err)
	}

	if _, err := io.Copy(plaintextPipe, srcFile); err != nil {
		_ = plaintextPipe.Close()
		return fmt.Errorf("write encrypted content: %w", err)
	}

	if err := plaintextPipe.Close(); err != nil {
		return fmt.Errorf("close encryption pipe: %w", err)
	}

	if err := armoredWriter.Close(); err != nil {
		return fmt.Errorf("close armored writer: %w", err)
	}

	return nil
}

// ValidatePGPPublicKey checks if an armored public key is well-formed.
func ValidatePGPPublicKey(armoredKey string) error {
	trimmed := NormalizePGPArmor(armoredKey)
	if trimmed == "" {
		return fmt.Errorf("public key cannot be empty")
	}
	keys, err := openpgp.ReadArmoredKeyRing(strings.NewReader(trimmed))
	if err != nil {
		return fmt.Errorf("invalid PGP public key format: %w", err)
	}
	if len(keys) == 0 {
		return fmt.Errorf("no valid public key blocks found")
	}
	return nil
}

// ValidatePGPPrivateKey checks if an armored private key is well-formed and validates passphrase if encrypted.
func ValidatePGPPrivateKey(armoredKey, passphrase string) error {
	trimmed := NormalizePGPArmor(armoredKey)
	if trimmed == "" {
		return fmt.Errorf("private key cannot be empty")
	}
	keys, err := openpgp.ReadArmoredKeyRing(strings.NewReader(trimmed))
	if err != nil {
		return fmt.Errorf("invalid PGP private key format: %w", err)
	}
	if len(keys) == 0 {
		return fmt.Errorf("no valid private key blocks found")
	}
	entity := keys[0]
	if entity.PrivateKey == nil {
		return fmt.Errorf("no private key block found in key data")
	}
	if entity.PrivateKey.Encrypted {
		if passphrase == "" {
			return fmt.Errorf("private key is encrypted with passphrase; passphrase is required")
		}
		if err := entity.PrivateKey.Decrypt([]byte(passphrase)); err != nil {
			return fmt.Errorf("incorrect passphrase: %w", err)
		}
	}
	return nil
}
