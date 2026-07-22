package serve

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	irohkey "github.com/tmc/go-iroh/key"
)

// loadOrCreateSecretKey reads the hex-encoded endpoint secret key from path,
// generating and persisting a fresh one on first run (0600). Deleting the
// file changes the server's endpoint ID.
func loadOrCreateSecretKey(path string) (irohkey.SecretKey, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		sk, err := irohkey.ParseSecretKey(strings.TrimSpace(string(b)))
		if err != nil {
			return irohkey.SecretKey{}, fmt.Errorf("parse key file %s: %w", path, err)
		}
		return sk, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return irohkey.SecretKey{}, fmt.Errorf("read key file: %w", err)
	}
	sk, err := irohkey.GenerateSecretKey()
	if err != nil {
		return irohkey.SecretKey{}, err
	}
	seed := sk.Bytes()
	if err := os.WriteFile(path, []byte(hex.EncodeToString(seed[:])+"\n"), 0o600); err != nil {
		return irohkey.SecretKey{}, fmt.Errorf("write key file: %w", err)
	}
	return sk, nil
}
