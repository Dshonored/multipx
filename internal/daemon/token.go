package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// TokenPath is the on-disk location of this machine's auth token. Every
// multipxd instance and every local client/CLI invocation for the current
// user reads the same file, so connections that never leave the machine
// authenticate with zero setup; a remote client has no way to read this
// file, so it needs the value copied over out-of-band — see
// "multipxd -print-token".
func TokenPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".multipx", "token"), nil
}

// EnsureToken returns this machine's persisted auth token, generating and
// saving a new random one on first use.
func EnsureToken() (string, error) {
	path, err := TokenPath()
	if err != nil {
		return "", err
	}
	if b, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(b)); tok != "" {
			return tok, nil
		}
	}

	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw[:])

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}
