package weather

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sampleResponse = `{
 "currentWeather": {"asOf": "2026-11-06T23:00:00Z", "conditionCode": "MostlyCloudy", "temperature": 21.4,
   "temperatureApparent": 22.9, "humidity": 0.83, "precipitationIntensity": 0.2, "windSpeed": 14.5},
 "forecastHourly": {"hours": [
   {"forecastStart": "2026-11-06T22:00:00Z", "conditionCode": "Drizzle", "temperature": 21.0,
    "precipitationChance": 0.6, "precipitationAmount": 0.4},
   {"forecastStart": "2026-11-06T23:00:00Z", "conditionCode": "Cloudy", "temperature": 21.5,
    "precipitationChance": 0.2, "precipitationAmount": 0}
 ]}
}`

func writeKey(t *testing.T, dir string) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(filepath.Join(dir, "weatherkit.p8"), pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return key
}

func testConfig(dir string) Config {
	return Config{TeamID: "TEAM123456", KeyID: "KEY1234567", ServiceID: "com.example.weather",
		KeyPath: filepath.Join(dir, "weatherkit.p8"), Lat: 22.2738, Lng: 113.5790}
}

// verifyToken checks the developer token the way WeatherKit does: header, claims and ES256 signature.
func verifyToken(t *testing.T, token string, pub *ecdsa.PublicKey, now time.Time) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	dec := base64.RawURLEncoding
	var header map[string]string
	var claims map[string]any
	h, _ := dec.DecodeString(parts[0])
	c, _ := dec.DecodeString(parts[1])
	if err := json.Unmarshal(h, &header); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(c, &claims); err != nil {
		t.Fatal(err)
	}
	if header["alg"] != "ES256" || header["kid"] != "KEY1234567" || header["id"] != "TEAM123456.com.example.weather" {
		t.Errorf("header = %v", header)
	}
	if claims["iss"] != "TEAM123456" || claims["sub"] != "com.example.weather" ||
		claims["iat"] != float64(now.Unix()) || claims["exp"] != float64(now.Add(time.Hour).Unix()) {
		t.Errorf("claims = %v", claims)
	}
	sig, err := dec.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		t.Fatalf("signature must be 64 raw bytes, got %d (%v)", len(sig), err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		t.Error("signature does not verify with the public key")
	}
}

func TestFetchSignsRequestAndParsesReport(t *testing.T) {
	dir := t.TempDir()
	key := writeKey(t, dir)
	now := time.Date(2026, 11, 6, 23, 5, 0, 0, time.UTC)
	from, to := now.Add(-24*time.Hour), now.Add(6*time.Hour)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/weather/en/22.2738/113.5790" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("dataSets") != "currentWeather,forecastHourly" ||
			q.Get("hourlyStart") != "2026-11-05T23:05:00Z" || q.Get("hourlyEnd") != "2026-11-07T05:05:00Z" {
			t.Errorf("query = %v", q)
		}
		verifyToken(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), &key.PublicKey, now)
		io.WriteString(w, sampleResponse)
	}))
	defer srv.Close()

	c, err := NewClient(srv.Client(), srv.URL, testConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	report, err := c.Fetch(context.Background(), now, from, to)
	if err != nil {
		t.Fatal(err)
	}
	cur := report.Current
	if cur.Condition != "MostlyCloudy" || cur.Temperature != 21.4 || cur.ApparentTemperature != 22.9 ||
		cur.Humidity != 0.83 || cur.PrecipitationIntensity != 0.2 || cur.WindSpeed != 14.5 ||
		!cur.AsOf.Equal(time.Date(2026, 11, 6, 23, 0, 0, 0, time.UTC)) {
		t.Errorf("current = %+v", cur)
	}
	if len(report.Hours) != 2 || report.Hours[0].Condition != "Drizzle" || report.Hours[0].PrecipitationAmount != 0.4 ||
		report.Hours[1].PrecipitationChance != 0.2 {
		t.Errorf("hours = %+v", report.Hours)
	}
}

func TestFetchErrors(t *testing.T) {
	dir := t.TempDir()
	writeKey(t, dir)
	status, body := http.StatusUnauthorized, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	defer srv.Close()
	c, err := NewClient(srv.Client(), srv.URL, testConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	fetch := func() error {
		_, err := c.Fetch(context.Background(), now, now.Add(-time.Hour), now)
		return err
	}

	if err := fetch(); !errors.Is(err, ErrAuth) {
		t.Errorf("401 should be ErrAuth, got %v", err)
	}
	status, body = http.StatusInternalServerError, "boom"
	if err := fetch(); err == nil || errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "boom") {
		t.Errorf("500 should carry the body, got %v", err)
	}
	status, body = http.StatusOK, `{"forecastHourly": {"hours": []}}`
	if err := fetch(); err == nil || !strings.Contains(err.Error(), "no current weather") {
		t.Errorf("missing currentWeather should fail, got %v", err)
	}
	if _, err := c.Fetch(context.Background(), now, now, now); err == nil {
		t.Error("empty hourly range should fail")
	}
}

func TestNewClientRejectsBadKeys(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	if _, err := NewClient(http.DefaultClient, DefaultBaseURL, cfg); err == nil {
		t.Error("missing key file should fail")
	}
	os.WriteFile(cfg.KeyPath, []byte("not a key"), 0o600)
	if _, err := NewClient(http.DefaultClient, DefaultBaseURL, cfg); err == nil {
		t.Error("non-PEM key should fail")
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "weatherkit.env")
	os.WriteFile(path, []byte(`# WeatherKit
WEATHERKIT_TEAM_ID=TEAM123456
WEATHERKIT_KEY_ID="KEY1234567"
WEATHERKIT_SERVICE_ID=com.example.weather
WEATHERKIT_LAT=22.2738
WEATHERKIT_LNG=113.5790
`), 0o600)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := testConfig(dir)
	if cfg != want {
		t.Errorf("config = %+v, want %+v", cfg, want)
	}

	for name, content := range map[string]string{
		"missing ids":  "WEATHERKIT_LAT=1\nWEATHERKIT_LNG=1\n",
		"bad latitude": "WEATHERKIT_TEAM_ID=a\nWEATHERKIT_KEY_ID=b\nWEATHERKIT_SERVICE_ID=c\nWEATHERKIT_LAT=95\nWEATHERKIT_LNG=1\n",
		"no longitude": "WEATHERKIT_TEAM_ID=a\nWEATHERKIT_KEY_ID=b\nWEATHERKIT_SERVICE_ID=c\nWEATHERKIT_LAT=1\n",
	} {
		os.WriteFile(path, []byte(content), 0o600)
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestDescribeAndIcon(t *testing.T) {
	if got := Describe("MostlyCloudy"); got != "Mostly cloudy" {
		t.Errorf("Describe = %q", got)
	}
	if got := Describe("Rain"); got != "Rain" {
		t.Errorf("Describe = %q", got)
	}
	if Icon("HeavyRain") != "🌧️" || Icon("SomethingNew") != "" {
		t.Error("Icon mapping")
	}
}
