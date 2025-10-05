package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"telegram-notifier/internal/constants"
)

// Config holds all application configuration loaded from environment variables
type Config struct {
	BotToken            string         // Telegram bot token (TELEGRAM_BOT_TOKEN)
	ChatID              string         // Telegram chat ID (TELEGRAM_CHAT_ID)
	CommandTimeout      time.Duration  // Max time for command execution
	HTTPTimeout         time.Duration  // Max time for HTTP requests
	JournalLookback     time.Duration  // How far back to look in journal
	MaxOutputSize       int            // Max characters in output messages
	TruncationMsgSize   int            // Size of truncation message
	DateTimeFormat      string         // Format string for timestamps
	JournalSinceDefault string         // Default since parameter for journal
	HostnameAlias       string         // Privacy: custom hostname for notifications
	TimeLocation        *time.Location // Timezone for timestamp formatting
}

// New creates and validates configuration from environment variables
// SECURITY: Validates required credentials exist before proceeding
func New() (*Config, error) {
	cfg := &Config{}
	cfg.BotToken = os.Getenv("TELEGRAM_BOT_TOKEN")
	cfg.ChatID = os.Getenv("TELEGRAM_CHAT_ID")

	// Fail fast if required credentials missing
	if cfg.BotToken == "" || cfg.ChatID == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID must be set")
	}

	// Load defaults first, then override with environment variables
	cfg.SetDefaults()
	if err := cfg.loadFromEnv(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// SetDefaults initializes configuration with sensible default values
func (c *Config) SetDefaults() {
	c.CommandTimeout = constants.DefaultCommandTimeout
	c.HTTPTimeout = constants.DefaultHTTPTimeout
	c.JournalLookback = constants.DefaultJournalLookback
	c.MaxOutputSize = constants.DefaultMaxOutputSize
	c.TruncationMsgSize = constants.DefaultTruncationMsgSize
	c.DateTimeFormat = constants.DefaultDateTimeFormat
	c.JournalSinceDefault = constants.DefaultJournalSince
	c.HostnameAlias = ""

	// Use TZ environment variable or system local time
	c.TimeLocation = getTimeLocation()
}

// loadFromEnv loads and parses configuration from environment variables
// Uses a map of parsers for extensibility and error handling
func (c *Config) loadFromEnv() error {
	// Map of environment variable name to parsing function
	parsers := map[string]func(string) error{
		"NOTIFIER_COMMAND_TIMEOUT": func(v string) error {
			d, err := time.ParseDuration(v)
			if err != nil {
				return err
			}
			c.CommandTimeout = d
			return nil
		},
		"NOTIFIER_HTTP_TIMEOUT": func(v string) error {
			d, err := time.ParseDuration(v)
			if err != nil {
				return err
			}
			c.HTTPTimeout = d
			return nil
		},
		"NOTIFIER_JOURNAL_LOOKBACK": func(v string) error {
			d, err := time.ParseDuration(v)
			if err != nil {
				return err
			}
			c.JournalLookback = d
			return nil
		},
		"NOTIFIER_MAX_OUTPUT_SIZE": func(v string) error {
			size, err := strconv.Atoi(v)
			if err != nil {
				return err
			}
			c.MaxOutputSize = size
			return nil
		},
		"NOTIFIER_DATETIME_FORMAT": func(v string) error {
			c.DateTimeFormat = v
			return nil
		},
		"NOTIFIER_JOURNAL_SINCE_DEFAULT": func(v string) error {
			c.JournalSinceDefault = v
			return nil
		},
		"NOTIFIER_HOSTNAME_ALIAS": func(v string) error {
			// PRIVACY: Allow users to set custom hostname alias
			c.HostnameAlias = v
			return nil
		},
	}

	// Parse each environment variable if present
	for envVar, parser := range parsers {
		if val := os.Getenv(envVar); val != "" {
			if err := parser(val); err != nil {
				return fmt.Errorf("parsing %s: %w", envVar, err)
			}
		}
	}

	// Reload timezone in case TZ was changed
	c.TimeLocation = getTimeLocation()

	return nil
}

// getTimeLocation loads timezone from TZ environment variable or uses system local
// PRIVACY: Respects user's timezone preference for timestamp formatting
func getTimeLocation() *time.Location {
	if tz := os.Getenv("TZ"); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.Local
}

// GetTimeLocation returns the configured timezone
func (c *Config) GetTimeLocation() *time.Location {
	return c.TimeLocation
}

// FormatDateTime formats a timestamp according to configured timezone and format
func (c *Config) FormatDateTime(t time.Time) string {
	return t.In(c.TimeLocation).Format(c.DateTimeFormat)
}

// GetHostname returns the configured hostname alias or actual hostname
// PRIVACY: Uses alias if set to protect user's real hostname
func (c *Config) GetHostname() string {
	if c.HostnameAlias != "" {
		return c.HostnameAlias
	}

	hostname, err := os.Hostname()
	if err != nil {
		return "unknown-host"
	}
	return hostname
}
