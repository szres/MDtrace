package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mdtrace/internal/intel"
)

func TestReadCookieHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "intel.cookie")
	if _, err := readCookieHeader(path); !errors.Is(err, intel.ErrAuth) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing cookie file should be an auth error, got %v", err)
	}
	if err := os.WriteFile(path, []byte("Cookie: SACSID=s; csrftoken=c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	header, err := readCookieHeader(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := intel.NewClient(nil, intel.DefaultBaseURL, header, ""); err != nil {
		t.Fatalf("cookie file contents should build a client: %v", err)
	}
}
