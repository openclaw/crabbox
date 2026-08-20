package shared

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

func CacheVolumeName(key string) string {
	key = strings.TrimSpace(key)
	sum := sha256.Sum256([]byte(key))
	var safe strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z':
			safe.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			safe.WriteRune(r + ('a' - 'A'))
		case r >= '0' && r <= '9':
			safe.WriteRune(r)
		case r == '.' || r == '_' || r == '-':
			safe.WriteRune(r)
		default:
			safe.WriteByte('-')
		}
		if safe.Len() >= 80 {
			break
		}
	}
	name := strings.Trim(safe.String(), ".-_")
	if name == "" {
		name = "volume"
	}
	return fmt.Sprintf("crabbox-cache-%s-%x", name, sum[:6])
}

func RandomSuffix() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:6]
	}
	return hex.EncodeToString(b[:])
}
