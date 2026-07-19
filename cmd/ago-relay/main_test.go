package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"claudexflow/internal/agorelay"
)

func TestParseConfigRequiresExactlyOneSecureTransport(t *testing.T) {
	tests := [][]string{
		{"-db", "relay.db"},
		{"-db", "relay.db", "-tls-cert", "cert.pem"},
		{"-db", "relay.db", "-tls-cert", "cert.pem", "-tls-key", "key.pem", "-trusted-proxies", "127.0.0.1/32"},
		{"-db", "relay.db", "-trusted-proxies", "not-a-cidr"},
	}
	for _, args := range tests {
		if _, err := parseConfig(args); err == nil {
			t.Fatalf("parseConfig(%q) succeeded", args)
		}
	}
	if _, err := parseConfig([]string{"-db", "relay.db", "-tls-cert", "cert.pem", "-tls-key", "key.pem"}); err != nil {
		t.Fatal(err)
	}
	if _, err := parseConfig([]string{"-db", "relay.db", "-trusted-proxies", "127.0.0.1/32,::1/128"}); err != nil {
		t.Fatal(err)
	}
}

func TestLoadCredentialsRequiresPrivateFileAndPersistsRotation(t *testing.T) {
	directory := t.TempDir()
	store, err := agorelay.Open(filepath.Join(directory, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	path := filepath.Join(directory, "credentials.json")
	data := `[{"account_id":"account","device_id":"device","role":"browser","generation":1,"token":"secret-token","projects":["project"]}]`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadCredentials(context.Background(), store, path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(context.Background(), "secret-token", agorelay.RoleBrowser); err != nil {
		t.Fatal(err)
	}
	if err := loadCredentials(context.Background(), store, path); err != nil {
		t.Fatalf("exact startup credential replay: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := loadCredentials(context.Background(), store, path); err == nil {
		t.Fatal("loadCredentials accepted a group/world-readable file")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "credentials-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := loadCredentials(context.Background(), store, link); err == nil {
		t.Fatal("loadCredentials accepted a symlink")
	}
}
