package secrets_test

import (
	"strings"
	"testing"

	"obsidian-fin/internal/secrets"
)

func TestNamespacingAndEnvironmentValidation(t *testing.T) {
	a := secrets.AccessRef("sandbox", "client", "item-a")
	b := secrets.AccessRef("sandbox", "client", "item-b")
	prod := secrets.AccessRef("production", "client", "item-a")
	other := secrets.AccessRef("sandbox", "other-client", "item-a")
	if a == b || a == prod || a == other || strings.Contains(a, "item-a") {
		t.Fatal("Keychain namespace is not isolated/opaque")
	}
	if !secrets.ValidateRef(a, "sandbox", "client") || secrets.ValidateRef(a, "production", "client") || secrets.ValidateRef(a, "sandbox", "other-client") {
		t.Fatal("reference mismatch not rejected")
	}
	if !secrets.ValidateToken("access-sandbox-test", "sandbox") || secrets.ValidateToken("access-sandbox-test", "production") || secrets.ValidateToken("public-sandbox-test", "sandbox") || secrets.ValidateToken("access-sandbox-", "sandbox") {
		t.Fatal("token validation")
	}
}
