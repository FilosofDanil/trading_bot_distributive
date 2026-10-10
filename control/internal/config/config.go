// Package config loads control settings from the process environment. On Railway every value,
// secrets included, comes from service variables; locally an optional .env file is loaded first.
package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	BotToken         string
	AllowedUserIDs   map[int64]bool  // numeric Telegram user ids; checked first
	AllowedUsernames map[string]bool // lower-case, without "@"; legacy fallback
	// Both allowlists empty = everyone (refused when preset keys are set).

	// Preset Binance keys (BINANCE_API_KEY / BINANCE_API_SECRET secrets).
	// When set, /start uses them instead of asking in the chat.
	PresetKey    string
	PresetSecret string

	FapiURL string
	WsURL   string
	SapiURL string

	CheckInterval   time.Duration
	AlertLossPct    float64
	BlockLossPct    float64
	BlockDuration   time.Duration
	Period          time.Duration
	AlertResetAfter time.Duration

	StateFile     string
	EncryptionKey []byte // nil = auto-generate next to the state file
	DefaultLang   string
	Location      *time.Location
}

// Load reads envFile (if present, local development only) and then the process environment.
// Variables already set in the environment win over the file.
func Load(envFile string) (*Config, error) {
	if envFile != "" {
		if err := godotenv.Load(envFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("load %s: %w", envFile, err)
		}
	}

	c := &Config{
		BotToken:    strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		FapiURL:     strings.TrimRight(get("BINANCE_FAPI_URL", "https://fapi.binance.com"), "/"),
		WsURL:       strings.TrimRight(get("BINANCE_WS_URL", "wss://fstream.binance.com"), "/"),
		SapiURL:     strings.TrimRight(get("BINANCE_SAPI_URL", "https://api.binance.com"), "/"),
		StateFile:   get("STATE_FILE", "data/state.json"),
		DefaultLang: get("DEFAULT_LANG", "en"),
	}
	if c.BotToken == "" {
		return nil, errors.New("TELEGRAM_BOT_TOKEN is not set")
	}

	var err error
	if c.AllowedUserIDs, err = parseUserIDs(os.Getenv("ALLOWED_USER_IDS")); err != nil {
		return nil, fmt.Errorf("ALLOWED_USER_IDS: %w", err)
	}
	c.AllowedUsernames = parseUsernames(os.Getenv("ALLOWED_USERNAMES"))

	c.PresetKey = strings.TrimSpace(os.Getenv("BINANCE_API_KEY"))
	c.PresetSecret = strings.TrimSpace(os.Getenv("BINANCE_API_SECRET"))
	if (c.PresetKey == "") != (c.PresetSecret == "") {
		return nil, errors.New("preset Binance keys: both BINANCE_API_KEY and BINANCE_API_SECRET are required")
	}
	if c.PresetKey != "" && !c.HasAllowlist() {
		return nil, errors.New("preset Binance keys require ALLOWED_USER_IDS, otherwise anyone could control the account")
	}
	durations := []struct {
		dst *time.Duration
		key string
		def string
	}{
		{&c.CheckInterval, "CHECK_INTERVAL", "10s"},
		{&c.BlockDuration, "BLOCK_DURATION", "24h"},
		{&c.Period, "PERIOD", "24h"},
		{&c.AlertResetAfter, "ALERT_RESET_AFTER", "1h"},
	}
	for _, d := range durations {
		if *d.dst, err = time.ParseDuration(get(d.key, d.def)); err != nil {
			return nil, fmt.Errorf("%s: %w", d.key, err)
		}
		if *d.dst <= 0 {
			return nil, fmt.Errorf("%s must be positive", d.key)
		}
	}
	if c.AlertLossPct, err = strconv.ParseFloat(get("ALERT_LOSS_PCT", "10"), 64); err != nil {
		return nil, fmt.Errorf("ALERT_LOSS_PCT: %w", err)
	}
	if c.BlockLossPct, err = strconv.ParseFloat(get("BLOCK_LOSS_PCT", "20"), 64); err != nil {
		return nil, fmt.Errorf("BLOCK_LOSS_PCT: %w", err)
	}
	if c.AlertLossPct <= 0 || c.BlockLossPct <= c.AlertLossPct {
		return nil, errors.New("need 0 < ALERT_LOSS_PCT < BLOCK_LOSS_PCT")
	}
	if c.Location, err = time.LoadLocation(get("TIMEZONE", "UTC")); err != nil {
		return nil, fmt.Errorf("TIMEZONE: %w", err)
	}
	if k := strings.TrimSpace(os.Getenv("ENCRYPTION_KEY")); k != "" {
		if c.EncryptionKey, err = hex.DecodeString(k); err != nil || len(c.EncryptionKey) != 32 {
			return nil, errors.New("ENCRYPTION_KEY must be 64 hex characters (32 bytes)")
		}
	}
	return c, nil
}

// HasAllowlist reports whether access is restricted to specific Telegram users.
func (c *Config) HasAllowlist() bool {
	return len(c.AllowedUserIDs) > 0 || len(c.AllowedUsernames) > 0
}

func get(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// NormalizeUsername lower-cases a Telegram username and drops a leading "@".
func NormalizeUsername(s string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "@"))
}

func parseUsernames(s string) map[string]bool {
	names := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		if n := NormalizeUsername(part); n != "" {
			names[n] = true
		}
	}
	return names
}

func parseUserIDs(s string) (map[int64]bool, error) {
	ids := map[int64]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("%q is not a numeric Telegram user id", part)
		}
		ids[id] = true
	}
	return ids, nil
}
