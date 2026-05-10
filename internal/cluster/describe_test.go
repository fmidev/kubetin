package cluster

import (
	"encoding/base64"
	"strings"
	"testing"
)

// Both data and stringData must be redacted; redaction must report
// the *decoded* byte count for data (base64) and the literal length
// for stringData.
func TestRedactSecretData_BothFields(t *testing.T) {
	plain := "hunter2" // 7 bytes
	encoded := base64.StdEncoding.EncodeToString([]byte(plain))
	obj := map[string]any{
		"kind": "Secret",
		"data": map[string]any{
			"password": encoded,
		},
		"stringData": map[string]any{
			"token": "raw-token-value", // 15 bytes literal
		},
	}

	if !redactSecretData(obj) {
		t.Fatal("redactSecretData returned false; expected redaction")
	}

	pw := obj["data"].(map[string]any)["password"].(string)
	if !strings.HasPrefix(pw, "<redacted,") || !strings.Contains(pw, "7 bytes") {
		t.Fatalf("data.password not redacted with decoded size: %q", pw)
	}
	if pw == encoded {
		t.Fatal("data.password still contains the base64 ciphertext")
	}

	tok := obj["stringData"].(map[string]any)["token"].(string)
	if !strings.HasPrefix(tok, "<redacted,") || !strings.Contains(tok, "15 bytes") {
		t.Fatalf("stringData.token not redacted with literal size: %q", tok)
	}
	if tok == "raw-token-value" {
		t.Fatal("stringData.token still contains plaintext")
	}
}

// When neither data nor stringData is present, no redaction should
// happen and the function should return false.
func TestRedactSecretData_NoData(t *testing.T) {
	obj := map[string]any{"kind": "Secret"}
	if redactSecretData(obj) {
		t.Fatal("redactSecretData returned true on object with no data")
	}
}

// stringData alone must trigger redaction (the watch-cache pre-
// admission case the team flagged).
func TestRedactSecretData_StringDataOnly(t *testing.T) {
	obj := map[string]any{
		"kind": "Secret",
		"stringData": map[string]any{
			"key": "value",
		},
	}
	if !redactSecretData(obj) {
		t.Fatal("redactSecretData returned false; expected redaction of stringData")
	}
	v := obj["stringData"].(map[string]any)["key"].(string)
	if v == "value" {
		t.Fatal("stringData.key still contains plaintext")
	}
}
