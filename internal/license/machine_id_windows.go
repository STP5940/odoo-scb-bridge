//go:build windows

package license

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"

	"golang.org/x/sys/windows/registry"
)

func MachineID() string {
	machineGuid := ""
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err == nil {
		machineGuid, _, _ = key.GetStringValue("MachineGuid")
		_ = key.Close()
	}
	hostName, _ := os.Hostname()
	identity := strings.ToLower(strings.TrimSpace(machineGuid) + "\x00" + strings.TrimSpace(hostName))
	sum := sha256.Sum256([]byte(ProductID + "\x00" + identity))
	return hex.EncodeToString(sum[:])
}
