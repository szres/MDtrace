package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSend(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTOKEN/sendMessage" {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"ok":false,"description":"Not Found"}`)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer srv.Close()

	c, err := NewClient(srv.Client(), srv.URL, "TOKEN", "42")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Send(context.Background(), "你好"); err != nil {
		t.Fatal(err)
	}
	if got["chat_id"] != "42" || got["text"] != "你好" {
		t.Fatalf("unexpected payload %v", got)
	}

	long := strings.Repeat("海", MaxMessageLength+10)
	if err := c.Send(context.Background(), long); err != nil {
		t.Fatal(err)
	}
	if n := utf8.RuneCountInString(got["text"].(string)); n != MaxMessageLength {
		t.Fatalf("long message has %d runes, want %d", n, MaxMessageLength)
	}
}

func TestSendReportsAPIErrorWithoutToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"ok":false,"description":"Bad Request: chat not found"}`)
	}))
	defer srv.Close()
	c, _ := NewClient(srv.Client(), srv.URL, "SECRET", "1")
	err := c.Send(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("expected API error, got %v", err)
	}

	srv.Close()
	err = c.Send(context.Background(), "x")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("transport error must not leak the token: %v", err)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "telegram.env")
	os.WriteFile(path, []byte("# comment\nTELEGRAM_BOT_TOKEN=\"abc:def\"\nTELEGRAM_CHAT_ID=123\n"), 0o600)
	cfg, err := LoadConfig(path)
	if err != nil || cfg != (Config{Token: "abc:def", ChatID: "123"}) {
		t.Fatalf("got %+v %v", cfg, err)
	}
	os.WriteFile(path, []byte("TELEGRAM_BOT_TOKEN=t\nTELEGRAM_CHAT_ID=1\nTELEGRAM_CHANNEL_ID=-1001234567890\n"), 0o600)
	if cfg, err := LoadConfig(path); err != nil || cfg.ChannelID != "-1001234567890" {
		t.Fatalf("channel: %+v %v", cfg, err)
	}
	os.WriteFile(path, []byte("TELEGRAM_BOT_TOKEN=x\n"), 0o600)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected error for missing chat ID")
	}
	if _, err := NewClient(http.DefaultClient, DefaultBaseURL, "", "1"); err == nil {
		t.Fatal("expected error for empty token")
	}
}
