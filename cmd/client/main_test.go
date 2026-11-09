package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestStartupRefusesUnknownDockerNetworking(t *testing.T) {
	dir := t.TempDir()
	err := run(t.Context(), "https://server.invalid", dir, "/unused-frpc", "", "127.0.0.1:0", filepath.Join(dir, "missing.sock"), false)
	if err == nil || !strings.Contains(err.Error(), "can't safely serve the local page") {
		t.Fatalf("startup without Docker networking: %v", err)
	}
}
