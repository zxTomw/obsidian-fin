// Package secrets provides a native Keychain adapter; there is no plaintext fallback.
package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

var ErrNotFound = errors.New("secret is not present in Keychain")
var ErrUnavailable = errors.New("macOS Keychain is unavailable or access was denied")

type Store interface {
	Get(context.Context, string) (string, error)
	Set(context.Context, string, string) error
	Delete(context.Context, string) error
}

func Namespace(environment, clientID string) string {
	h := sha256.Sum256([]byte(clientID))
	return "obsidian-fin/" + environment + "/" + hex.EncodeToString(h[:16]) + "/"
}
func ClientRef(environment, clientID string) string {
	return Namespace(environment, clientID) + "client-secret"
}
func AccessRef(environment, clientID, itemID string) string {
	h := sha256.Sum256([]byte(itemID))
	return Namespace(environment, clientID) + "access/" + hex.EncodeToString(h[:16])
}
func ValidateRef(ref, environment, clientID string) bool {
	return strings.HasPrefix(ref, Namespace(environment, clientID)+"access/")
}
func ValidateToken(token, environment string) bool {
	return strings.HasPrefix(token, "access-"+environment+"-") && len(token) > len("access-"+environment+"-")
}
