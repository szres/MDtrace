// Package telegram sends plain-text messages through the Telegram Bot API.
package telegram

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const DefaultBaseURL = "https://api.telegram.org"

// MaxMessageLength is Telegram's limit for one message's text.
const MaxMessageLength = 4096

type Client struct {
	httpClient *http.Client
	baseURL    string
	token      string
	chatID     string
}

func NewClient(httpClient *http.Client, baseURL, token, chatID string) (*Client, error) {
	token, chatID = strings.TrimSpace(token), strings.TrimSpace(chatID)
	if token == "" || chatID == "" {
		return nil, errors.New("telegram bot token and chat ID are required")
	}
	return &Client{httpClient: httpClient, baseURL: strings.TrimRight(baseURL, "/"), token: token, chatID: chatID}, nil
}

// Config holds the bot token, the private chat for alerts and updates, and an optional
// channel (Bot API form, e.g. -100…) that receives updates only.
type Config struct {
	Token     string
	ChatID    string
	ChannelID string
}

// LoadConfig reads TELEGRAM_BOT_TOKEN, TELEGRAM_CHAT_ID and optional TELEGRAM_CHANNEL_ID
// from a KEY=VALUE file.
func LoadConfig(path string) (Config, error) {
	var cfg Config
	f, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer f.Close()
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
		case "TELEGRAM_BOT_TOKEN":
			cfg.Token = value
		case "TELEGRAM_CHAT_ID":
			cfg.ChatID = value
		case "TELEGRAM_CHANNEL_ID":
			cfg.ChannelID = value
		}
	}
	if err := scanner.Err(); err != nil {
		return Config{}, err
	}
	if cfg.Token == "" || cfg.ChatID == "" {
		return Config{}, fmt.Errorf("%s must set TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID", path)
	}
	return cfg, nil
}

func (c *Client) Send(ctx context.Context, text string) error {
	if runes := []rune(text); len(runes) > MaxMessageLength {
		text = string(runes[:MaxMessageLength-1]) + "…"
	}
	payload, err := json.Marshal(map[string]any{
		"chat_id":                  c.chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	})
	if err != nil {
		return err
	}
	return c.call(ctx, "sendMessage", bytes.NewReader(payload), "application/json")
}

func (c *Client) call(ctx context.Context, method string, body io.Reader, contentType string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/bot"+c.token+"/"+method, body)
	if err != nil {
		return fmt.Errorf("telegram %s: cannot build request", method)
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// *url.Error embeds the request URL, which contains the bot token; keep only the cause.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("telegram %s: read response: %w", method, err)
	}
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("telegram %s: HTTP %d, undecodable response", method, resp.StatusCode)
	}
	if !result.OK {
		return fmt.Errorf("telegram %s: HTTP %d: %s", method, resp.StatusCode, result.Description)
	}
	return nil
}
