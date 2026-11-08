// Package intel talks to the Ingress Intel map API using a browser session cookie.
package intel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
)

const DefaultBaseURL = "https://intel.ingress.com"

// DefaultUserAgent mimics a desktop browser; Intel serves the dashboard only to browser-like clients.
const DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"

// ErrAuth means the Intel session cookie is missing, expired or rejected.
var ErrAuth = errors.New("intel session is not authenticated; refresh the cookie")

const maxBodyBytes = 4 << 20

// Intel embeds the API version as the hash in its dashboard script name; every /r/ call must send it.
var versionPattern = regexp.MustCompile(`gen_dashboard_([0-9a-f]+)\.js`)

type Client struct {
	httpClient *http.Client
	baseURL    string
	cookie     string
	csrfToken  string
	userAgent  string
	version    string
}

// NewClient builds a client from a raw "Cookie:" header value copied from a logged-in browser.
func NewClient(httpClient *http.Client, baseURL, cookieHeader, userAgent string) (*Client, error) {
	cookieHeader = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cookieHeader), "Cookie:"))
	if cookieHeader == "" {
		return nil, fmt.Errorf("%w: cookie is empty", ErrAuth)
	}
	cookies, err := http.ParseCookie(cookieHeader)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot parse cookie header: %v", ErrAuth, err)
	}
	var csrf string
	var hasSession bool
	for _, c := range cookies {
		switch c.Name {
		case "csrftoken":
			csrf = c.Value
		default:
			if IsSessionCookie(c.Name) {
				hasSession = hasSession || c.Value != ""
			}
		}
	}
	if !hasSession || csrf == "" {
		return nil, fmt.Errorf("%w: cookie must contain a session (sessionid or SACSID) and csrftoken", ErrAuth)
	}
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	return &Client{
		httpClient: httpClient,
		baseURL:    strings.TrimRight(baseURL, "/"),
		cookie:     cookieHeader,
		csrfToken:  csrf,
		userAgent:  userAgent,
	}, nil
}

// LoadVersion fetches the Intel dashboard page and extracts the API version hash.
// It also serves as the authentication check: the login page carries no dashboard script.
func (c *Client) LoadVersion(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/", nil)
	if err != nil {
		return err
	}
	c.setCommonHeaders(req)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("load intel dashboard: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("read intel dashboard: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("load intel dashboard: HTTP %d", resp.StatusCode)
	}
	match := versionPattern.FindSubmatch(body)
	if match == nil {
		return fmt.Errorf("%w: dashboard page has no gen_dashboard script (login page served)", ErrAuth)
	}
	c.version = string(match[1])
	return nil
}

func (c *Client) Version() string { return c.version }

// IsSessionCookie reports whether name is Intel's login session cookie.
// Current Intel uses sessionid; SACSID was the older Google-auth session.
func IsSessionCookie(name string) bool {
	return name == "sessionid" || name == "SACSID"
}

// MissionDetails calls /r/getMissionDetails for one mission GUID.
func (c *Client) MissionDetails(ctx context.Context, guid string) (MissionDetail, error) {
	if c.version == "" {
		return MissionDetail{}, errors.New("intel version not loaded; call LoadVersion first")
	}
	payload, err := json.Marshal(map[string]string{"guid": guid, "v": c.version})
	if err != nil {
		return MissionDetail{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/r/getMissionDetails", bytes.NewReader(payload))
	if err != nil {
		return MissionDetail{}, err
	}
	c.setCommonHeaders(req)
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("X-CSRFToken", c.csrfToken)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", c.baseURL)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return MissionDetail{}, fmt.Errorf("getMissionDetails %s: %w", guid, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return MissionDetail{}, fmt.Errorf("read getMissionDetails %s: %w", guid, err)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return MissionDetail{}, fmt.Errorf("%w: getMissionDetails HTTP %d", ErrAuth, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return MissionDetail{}, fmt.Errorf("getMissionDetails %s: HTTP %d: %s", guid, resp.StatusCode, preview(body))
	}
	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType == "text/html" {
		return MissionDetail{}, fmt.Errorf("%w: getMissionDetails returned an HTML page", ErrAuth)
	}

	detail, err := ParseMissionDetail(body)
	if err != nil {
		return MissionDetail{}, fmt.Errorf("mission %s: %w", guid, err)
	}
	if detail.GUID != guid {
		return MissionDetail{}, fmt.Errorf("mission %s: response is for mission %s", guid, detail.GUID)
	}
	return detail, nil
}

func (c *Client) setCommonHeaders(req *http.Request) {
	req.Header.Set("Cookie", c.cookie)
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Referer", c.baseURL+"/")
}

func preview(body []byte) string {
	const limit = 300
	text := strings.TrimSpace(string(body))
	if len(text) > limit {
		return text[:limit] + "..."
	}
	return text
}
