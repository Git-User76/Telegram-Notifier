package constants

import (
	"regexp"
	"time"
)

// Timeouts
const (
	DefaultCommandTimeout  = 30 * time.Second
	DefaultHTTPTimeout     = 10 * time.Second
	DefaultJournalLookback = 30 * time.Second
)

// Size limits
const (
	DefaultMaxOutputSize     = 2500
	DefaultTruncationMsgSize = 30
	TelegramMaxMessageSize   = 4096
	MessageSafetyMargin      = 500
)

// Time formatting
const (
	DefaultDateTimeFormat = "02-Jan 15:04:05"
	DefaultJournalSince   = "1 minute ago"
)

// HTTP retry configuration
const (
	MaxHTTPRetries     = 3
	InitialRetryDelay  = 1 * time.Second
	MaxRetryDelay      = 10 * time.Second
	RetryBackoffFactor = 2.0
)

// Rate limiting for Telegram API
const (
	RateLimitTokens      = 10
	RateLimitRefillRate  = 1 * time.Second
	RateLimitMaxWaitTime = 5 * time.Second
)

// Rate limiting for command execution (prevent abuse)
const (
	CommandRateLimitTokens     = 30 // Allow 30 commands
	CommandRateLimitRefillRate = 1 * time.Second
	CommandRateLimitMaxWait    = 10 * time.Second
)

// Validation patterns
var (
	ServiceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9:_.@-]+\.service$`)
	ExitCodeMin        = 0
	ExitCodeMax        = 255
)

// Secret patterns for filtering (enhanced)
var SecretPatterns = []*regexp.Regexp{
	// Passwords and API keys
	regexp.MustCompile(`(?i)(password|passwd|pwd)[\s:=]+['"]?([^\s'"]+)`),
	regexp.MustCompile(`(?i)(api[_-]?key|apikey)[\s:=]+['"]?([^\s'"]+)`),
	regexp.MustCompile(`(?i)(secret|token)[\s:=]+['"]?([^\s'"]+)`),
	regexp.MustCompile(`(?i)(auth[_-]?token)[\s:=]+['"]?([^\s'"]+)`),

	// Bearer tokens
	regexp.MustCompile(`(?i)bearer\s+([a-zA-Z0-9\-._~+/]+=*)`),

	// SSH/TLS keys (all types)
	regexp.MustCompile(`-----BEGIN\s+(?:RSA|DSA|EC|OPENSSH|ENCRYPTED)?\s*PRIVATE\s+KEY-----`),

	// Cloud provider keys
	regexp.MustCompile(`(?i)(aws_secret_access_key|aws_access_key_id)[\s:=]+['"]?([^\s'"]+)`),
	regexp.MustCompile(`(?i)(gcp|google)[-_]?(service[-_]?account|credentials)[\s:=]+['"]?([^\s'"]+)`),
	regexp.MustCompile(`(?i)(azure|az)[-_]?(key|secret|token)[\s:=]+['"]?([^\s'"]+)`),

	// Database connection strings
	regexp.MustCompile(`(?i)[a-z]+://[^:/@\s]+:([^@/\s]+)@`),
	regexp.MustCompile(`(?i)(mongodb|postgresql|mysql|redis)://[^\s]+`),

	// JWT tokens
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),

	// GitHub/GitLab tokens
	regexp.MustCompile(`(?i)(gh[pousr]_[A-Za-z0-9]{36,})`),
	regexp.MustCompile(`(?i)(glpat-[A-Za-z0-9\-_]{20,})`),

	// Generic base64-encoded secrets
	regexp.MustCompile(`(?i)(secret|key|token|password|credential)[\s:=]+['"]?([A-Za-z0-9+/]{32,}={0,2})`),

	// OAuth tokens
	regexp.MustCompile(`(?i)(access_token|refresh_token)[\s:=]+['"]?([^\s'"]+)`),

	// Slack tokens
	regexp.MustCompile(`xox[baprs]-[0-9]{10,13}-[0-9]{10,13}-[a-zA-Z0-9]{24,}`),

	// Generic credentials in environment variable format
	regexp.MustCompile(`(?i)(export\s+)?[A-Z_]+_(PASSWORD|SECRET|KEY|TOKEN)=['"]([^'"]+)['"]`),
}

const OutputTruncatedMsg = "...(output truncated)\n\n"
