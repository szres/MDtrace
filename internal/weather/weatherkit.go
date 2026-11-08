// Package weather fetches conditions from Apple's WeatherKit REST API.
package weather

import (
	"bufio"
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
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const DefaultBaseURL = "https://weatherkit.apple.com"

// tokenLifetime is how long a signed request token stays valid; one is minted per fetch.
const tokenLifetime = time.Hour

// ErrAuth means WeatherKit rejected the credentials (wrong IDs, revoked key, or WeatherKit not enabled).
var ErrAuth = errors.New("weatherkit rejected the credentials")

type Config struct {
	TeamID    string
	KeyID     string
	ServiceID string
	KeyPath   string
	Lat, Lng  float64
}

// LoadConfig reads weatherkit.env. WEATHERKIT_KEY_FILE is resolved relative to the env file.
func LoadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	cfg := Config{KeyPath: "weatherkit.p8"}
	var lat, lng string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(key) {
		case "WEATHERKIT_TEAM_ID":
			cfg.TeamID = value
		case "WEATHERKIT_KEY_ID":
			cfg.KeyID = value
		case "WEATHERKIT_SERVICE_ID":
			cfg.ServiceID = value
		case "WEATHERKIT_KEY_FILE":
			cfg.KeyPath = value
		case "WEATHERKIT_LAT":
			lat = value
		case "WEATHERKIT_LNG":
			lng = value
		}
	}
	if err := scanner.Err(); err != nil {
		return Config{}, err
	}
	if cfg.TeamID == "" || cfg.KeyID == "" || cfg.ServiceID == "" {
		return Config{}, fmt.Errorf("%s must set WEATHERKIT_TEAM_ID, WEATHERKIT_KEY_ID and WEATHERKIT_SERVICE_ID", path)
	}
	if cfg.Lat, err = parseCoordinate(lat, 90); err != nil {
		return Config{}, fmt.Errorf("%s: WEATHERKIT_LAT: %w", path, err)
	}
	if cfg.Lng, err = parseCoordinate(lng, 180); err != nil {
		return Config{}, fmt.Errorf("%s: WEATHERKIT_LNG: %w", path, err)
	}
	if !filepath.IsAbs(cfg.KeyPath) {
		cfg.KeyPath = filepath.Join(filepath.Dir(path), cfg.KeyPath)
	}
	return cfg, nil
}

func parseCoordinate(s string, limit float64) (float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid coordinate %q", s)
	}
	if math.Abs(v) > limit {
		return 0, fmt.Errorf("coordinate %v out of range", v)
	}
	return v, nil
}

type Client struct {
	http    *http.Client
	baseURL string
	cfg     Config
	key     *ecdsa.PrivateKey
}

func NewClient(httpClient *http.Client, baseURL string, cfg Config) (*Client, error) {
	raw, err := os.ReadFile(cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("weatherkit key: %w", err)
	}
	key, err := parsePrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("weatherkit key %s: %w", filepath.Base(cfg.KeyPath), err)
	}
	return &Client{http: httpClient, baseURL: strings.TrimRight(baseURL, "/"), cfg: cfg, key: key}, nil
}

// parsePrivateKey reads the .p8 file Apple issues: a PEM-wrapped PKCS#8 P-256 key.
func parsePrivateKey(raw []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("not a PEM file")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("key must be an ECDSA P-256 key")
	}
	return key, nil
}

// token returns the ES256 developer token WeatherKit expects.
func (c *Client) token(now time.Time) (string, error) {
	header, err := json.Marshal(map[string]string{
		"alg": "ES256", "kid": c.cfg.KeyID, "id": c.cfg.TeamID + "." + c.cfg.ServiceID,
	})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"iss": c.cfg.TeamID, "sub": c.cfg.ServiceID,
		"iat": now.Unix(), "exp": now.Add(tokenLifetime).Unix(),
	})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, c.key, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign weatherkit token: %w", err)
	}
	// JWS ES256 signatures are the fixed-width concatenation r || s, not ASN.1.
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return signingInput + "." + enc.EncodeToString(signature), nil
}

// Current is the observed weather at the configured location.
type Current struct {
	AsOf                   time.Time
	Condition              string // WeatherKit condition code, e.g. "MostlyCloudy"
	Temperature            float64
	ApparentTemperature    float64
	Humidity               float64 // 0-1
	PrecipitationIntensity float64 // mm/h
	WindSpeed              float64 // km/h
}

// Hour is one hour of the hourly forecast; past hours carry WeatherKit's historical values.
type Hour struct {
	Start               time.Time
	Condition           string
	Temperature         float64
	PrecipitationChance float64 // 0-1
	PrecipitationAmount float64 // mm
}

type Report struct {
	Current Current
	Hours   []Hour
}

// Fetch returns current conditions and the hourly series from hoursFrom to hoursTo.
func (c *Client) Fetch(ctx context.Context, now, hoursFrom, hoursTo time.Time) (Report, error) {
	if !hoursTo.After(hoursFrom) {
		return Report{}, errors.New("weatherkit: empty hourly range")
	}
	token, err := c.token(now)
	if err != nil {
		return Report{}, err
	}
	query := url.Values{
		"dataSets":    {"currentWeather,forecastHourly"},
		"hourlyStart": {hoursFrom.UTC().Format(time.RFC3339)},
		"hourlyEnd":   {hoursTo.UTC().Format(time.RFC3339)},
	}
	endpoint := fmt.Sprintf("%s/api/v1/weather/en/%s/%s?%s", c.baseURL,
		strconv.FormatFloat(c.cfg.Lat, 'f', 4, 64), strconv.FormatFloat(c.cfg.Lng, 'f', 4, 64), query.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Report{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return Report{}, fmt.Errorf("weatherkit: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Report{}, fmt.Errorf("weatherkit: read response: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Report{}, fmt.Errorf("%w (HTTP %d)", ErrAuth, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return Report{}, fmt.Errorf("weatherkit: HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return parseReport(body)
}

func parseReport(body []byte) (Report, error) {
	var payload struct {
		CurrentWeather *struct {
			AsOf                   time.Time `json:"asOf"`
			ConditionCode          string    `json:"conditionCode"`
			Temperature            float64   `json:"temperature"`
			TemperatureApparent    float64   `json:"temperatureApparent"`
			Humidity               float64   `json:"humidity"`
			PrecipitationIntensity float64   `json:"precipitationIntensity"`
			WindSpeed              float64   `json:"windSpeed"`
		} `json:"currentWeather"`
		ForecastHourly *struct {
			Hours []struct {
				ForecastStart       time.Time `json:"forecastStart"`
				ConditionCode       string    `json:"conditionCode"`
				Temperature         float64   `json:"temperature"`
				PrecipitationChance float64   `json:"precipitationChance"`
				PrecipitationAmount float64   `json:"precipitationAmount"`
			} `json:"hours"`
		} `json:"forecastHourly"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Report{}, fmt.Errorf("weatherkit: decode response: %w", err)
	}
	cw := payload.CurrentWeather
	if cw == nil || cw.AsOf.IsZero() {
		return Report{}, errors.New("weatherkit: response has no current weather for this location")
	}
	report := Report{Current: Current{
		AsOf: cw.AsOf, Condition: cw.ConditionCode, Temperature: cw.Temperature,
		ApparentTemperature: cw.TemperatureApparent, Humidity: cw.Humidity,
		PrecipitationIntensity: cw.PrecipitationIntensity, WindSpeed: cw.WindSpeed,
	}}
	if payload.ForecastHourly != nil {
		for _, h := range payload.ForecastHourly.Hours {
			if h.ForecastStart.IsZero() {
				continue
			}
			report.Hours = append(report.Hours, Hour{
				Start: h.ForecastStart, Condition: h.ConditionCode, Temperature: h.Temperature,
				PrecipitationChance: h.PrecipitationChance, PrecipitationAmount: h.PrecipitationAmount,
			})
		}
	}
	return report, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Describe turns a condition code such as "MostlyCloudy" into "Mostly cloudy".
func Describe(code string) string {
	var b strings.Builder
	for i, r := range code {
		if i > 0 && unicode.IsUpper(r) {
			b.WriteByte(' ')
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Icon returns an emoji for a condition code, or "" when there is no obvious one.
func Icon(code string) string {
	switch code {
	case "Clear", "MostlyClear", "Hot":
		return "☀️"
	case "PartlyCloudy":
		return "⛅"
	case "MostlyCloudy", "Cloudy":
		return "☁️"
	case "Foggy", "Haze", "Smoky":
		return "🌫️"
	case "Drizzle", "Rain", "HeavyRain", "SunShowers", "FreezingDrizzle", "FreezingRain":
		return "🌧️"
	case "Thunderstorms", "IsolatedThunderstorms", "ScatteredThunderstorms", "StrongStorms":
		return "⛈️"
	case "Breezy", "Windy":
		return "💨"
	case "TropicalStorm", "Hurricane":
		return "🌀"
	case "Snow", "HeavySnow", "Flurries", "Sleet", "Blizzard", "BlowingSnow", "WintryMix", "SunFlurries", "Hail":
		return "🌨️"
	}
	return ""
}
