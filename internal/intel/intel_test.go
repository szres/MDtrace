package intel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testCookie = "SACSID=session123; csrftoken=csrf456; ingress.intelmap.zoom=16"

const sampleResult = `{"result":["abc.1c","MD 2026: Zhuhai, 海滨泳场","desc","agentX","E",987654,1800000,321,0,[],"https://img"]}`

func TestParseMissionDetail(t *testing.T) {
	d, err := ParseMissionDetail([]byte(sampleResult))
	if err != nil {
		t.Fatal(err)
	}
	want := MissionDetail{
		GUID: "abc.1c", Title: "MD 2026: Zhuhai, 海滨泳场", Author: "agentX", Faction: "ENLIGHTENED",
		RatingE6: 987654, AverageDurationMs: 1800000, CompletedCount: 321,
	}
	if d != want {
		t.Fatalf("got %+v, want %+v", d, want)
	}
	if got := d.RatingPercent(); got != 98.7654 {
		t.Fatalf("RatingPercent = %v", got)
	}
}

func TestParseMissionDetailErrors(t *testing.T) {
	cases := map[string]string{
		"intel error":    `{"error":"missing version"}`,
		"null result":    `{"result":null}`,
		"short array":    `{"result":["a","b"]}`,
		"not array":      `{"result":{"x":1}}`,
		"bad rating":     `{"result":["a","t","d","au","E","x",1,1]}`,
		"rating range":   `{"result":["a","t","d","au","E",2000000,1,1]}`,
		"negative count": `{"result":["a","t","d","au","E",1,1,-1]}`,
		"empty guid":     `{"result":["","t","d","au","E",1,1,1]}`,
		"html":           `<html>login</html>`,
	}
	for name, body := range cases {
		if _, err := ParseMissionDetail([]byte(body)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestNewClientValidatesCookie(t *testing.T) {
	for _, cookie := range []string{"", "csrftoken=x", "SACSID=x", "Cookie: SACSID=; csrftoken=x", "sessionid=x"} {
		if _, err := NewClient(http.DefaultClient, DefaultBaseURL, cookie, ""); !errors.Is(err, ErrAuth) {
			t.Errorf("cookie %q: expected ErrAuth, got %v", cookie, err)
		}
	}
	for _, cookie := range []string{"Cookie: " + testCookie + "\n", "sessionid=abc; csrftoken=x"} {
		if _, err := NewClient(http.DefaultClient, DefaultBaseURL, cookie, ""); err != nil {
			t.Fatalf("valid cookie %q rejected: %v", cookie, err)
		}
	}
}

func newFakeIntel(t *testing.T, loggedIn bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if loggedIn && strings.Contains(r.Header.Get("Cookie"), "SACSID=session123") {
			io.WriteString(w, `<script src="/jsc/gen_dashboard_0123abcdef.js"></script>`)
			return
		}
		io.WriteString(w, `<html>Sign in</html>`)
	})
	mux.HandleFunc("POST /r/getMissionDetails", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-CSRFToken") != "csrf456" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["v"] != "0123abcdef" {
			io.WriteString(w, `{"error":"out of date"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if body["guid"] == "html.1c" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, "<html></html>")
			return
		}
		io.WriteString(w, strings.Replace(sampleResult, "abc.1c", body["guid"], 1))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestClientFetchesMissionDetails(t *testing.T) {
	srv := newFakeIntel(t, true)
	c, err := NewClient(srv.Client(), srv.URL, testCookie, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.MissionDetails(ctx, "x.1c"); err == nil {
		t.Fatal("expected error before LoadVersion")
	}
	if err := c.LoadVersion(ctx); err != nil {
		t.Fatal(err)
	}
	if c.Version() != "0123abcdef" {
		t.Fatalf("version = %q", c.Version())
	}
	d, err := c.MissionDetails(ctx, "zzz.1c")
	if err != nil {
		t.Fatal(err)
	}
	if d.GUID != "zzz.1c" || d.CompletedCount != 321 {
		t.Fatalf("unexpected detail %+v", d)
	}
	if _, err := c.MissionDetails(ctx, "html.1c"); !errors.Is(err, ErrAuth) {
		t.Fatalf("HTML response: expected ErrAuth, got %v", err)
	}
}

func TestClientDetectsLoggedOutSession(t *testing.T) {
	srv := newFakeIntel(t, false)
	c, err := NewClient(srv.Client(), srv.URL, testCookie, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.LoadVersion(context.Background()); !errors.Is(err, ErrAuth) {
		t.Fatalf("expected ErrAuth, got %v", err)
	}
}

func TestClientRejectedCSRFIsAuthError(t *testing.T) {
	srv := newFakeIntel(t, true)
	c, err := NewClient(srv.Client(), srv.URL, "SACSID=session123; csrftoken=wrong", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.LoadVersion(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.MissionDetails(context.Background(), "a.1c"); !errors.Is(err, ErrAuth) {
		t.Fatalf("expected ErrAuth, got %v", err)
	}
}
