//go:build !windows

package license

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/user"
	"strings"
)

func MachineID() string {
	hostName, _ := os.Hostname()
	profile := ""
	if current, err := user.Current(); err == nil {
		profile = current.Username
	}
	identity := strings.ToLower(strings.TrimSpace(hostName) + "\x00" + strings.TrimSpace(profile))
	sum := sha256.Sum256([]byte(ProductID + "\x00" + identity))
	return hex.EncodeToString(sum[:])
}
