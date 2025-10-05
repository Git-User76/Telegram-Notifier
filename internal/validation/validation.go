package validation

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"telegram-notifier/internal/constants"
)

// ValidateServiceName ensures service name follows systemd naming conventions
// and prevents command injection via shell metacharacters
func ValidateServiceName(name string) error {
	if name == "" {
		return fmt.Errorf("service name cannot be empty")
	}
	if len(name) > 256 {
		return fmt.Errorf("service name too long (max 256 characters)")
	}

	// Prevent null byte injection and control character attacks
	for _, r := range name {
		if r == 0 || r == '\n' || r == '\r' || unicode.IsControl(r) {
			return fmt.Errorf("service name contains invalid control characters")
		}
	}

	// Prevent homograph attacks using Unicode lookalikes
	for _, r := range name {
		if r > 127 {
			return fmt.Errorf("service name must contain only ASCII characters")
		}
	}

	// Defense-in-depth: Block shell metacharacters even though we use exec.CommandContext
	// This prevents potential injection if code is ever modified to use shell execution
	dangerousChars := []rune{'$', '`', '|', ';', '&', '\\', '\n', '\r', '<', '>', '(', ')', '{', '}', '[', ']', '!', '*', '?', '~'}
	for _, danger := range dangerousChars {
		if strings.ContainsRune(name, danger) {
			return fmt.Errorf("service name contains potentially dangerous character: %c", danger)
		}
	}

	if !constants.ServiceNamePattern.MatchString(name) {
		return fmt.Errorf("invalid service name format: must match pattern %s", constants.ServiceNamePattern.String())
	}
	return nil
}

// ValidateExitCode ensures exit code is in valid range (0-255)
func ValidateExitCode(code int) error {
	if code < constants.ExitCodeMin || code > constants.ExitCodeMax {
		return fmt.Errorf("exit code %d out of valid range [%d-%d]", code, constants.ExitCodeMin, constants.ExitCodeMax)
	}
	return nil
}

// SanitizePath prevents path traversal attacks by validating the path is within baseDir
// SECURITY: Returns fully resolved path to prevent TOCTOU race conditions where
// an attacker could replace a directory with a symlink between validation and use
func SanitizePath(baseDir, filename string) (string, error) {
	cleanBase := filepath.Clean(baseDir)
	cleanFile := filepath.Clean(filename)
	fullPath := filepath.Join(cleanBase, cleanFile)

	// Resolve all symlinks in the target path to detect traversal attempts
	resolvedPath, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		// If target doesn't exist, resolve parent and append filename
		parentDir := filepath.Dir(fullPath)
		resolvedParent, err := filepath.EvalSymlinks(parentDir)
		if err != nil {
			return "", fmt.Errorf("cannot resolve parent directory: %w", err)
		}
		resolvedPath = filepath.Join(resolvedParent, filepath.Base(fullPath))
	}

	// Resolve base directory symlinks for accurate comparison
	resolvedBase, err := filepath.EvalSymlinks(cleanBase)
	if err != nil {
		return "", fmt.Errorf("cannot resolve base directory: %w", err)
	}

	// Verify resolved path doesn't escape the base directory
	relPath, err := filepath.Rel(resolvedBase, resolvedPath)
	if err != nil || strings.HasPrefix(relPath, "..") || strings.HasPrefix(relPath, string(filepath.Separator)) {
		return "", fmt.Errorf("path traversal detected: %s escapes %s", filename, baseDir)
	}

	// Return RESOLVED path to prevent TOCTOU attacks
	// This ensures subsequent file operations use the validated symlink-resolved path
	return resolvedPath, nil
}

// FilterSecrets removes sensitive information from output using regex patterns
// SECURITY: Prevents credential leakage in logs and notifications
func FilterSecrets(input string) string {
	result := input
	// Apply all secret detection patterns and redact matches
	for _, pattern := range constants.SecretPatterns {
		result = pattern.ReplaceAllStringFunc(result, func(match string) string {
			if len(match) > 20 {
				return match[:20] + "[REDACTED]"
			}
			return "[REDACTED]"
		})
	}
	return result
}

// FilterSecretsFromError filters sensitive information from error objects
func FilterSecretsFromError(err error) error {
	if err == nil {
		return nil
	}
	filtered := FilterSecrets(err.Error())
	return fmt.Errorf("%s", filtered)
}

// SanitizeErrorMessage prevents information disclosure by removing system paths
// and filtering secrets from error messages before logging or display
func SanitizeErrorMessage(err error) string {
	if err == nil {
		return ""
	}

	msg := err.Error()

	// Remove secrets first
	msg = FilterSecrets(msg)

	// Obscure system directory structure to prevent reconnaissance
	msg = strings.ReplaceAll(msg, "/etc/systemd/", "[systemd]/")
	msg = strings.ReplaceAll(msg, "/usr/lib/systemd/", "[systemd]/")
	msg = strings.ReplaceAll(msg, "/lib/systemd/", "[systemd]/")

	// Remove home directory paths for privacy
	if homeDir, err := os.UserHomeDir(); err == nil && homeDir != "" {
		msg = strings.ReplaceAll(msg, homeDir, "~")
	}

	return msg
}

// TruncateMessage ensures message fits within Telegram's limits
// Shows most recent output (end of message) as it's typically most relevant
func TruncateMessage(msg string, maxSize int) string {
	if len(msg) <= maxSize {
		return msg
	}

	truncMsg := constants.OutputTruncatedMsg
	availableSize := maxSize - len(truncMsg)

	if availableSize <= 0 {
		return msg[:maxSize]
	}

	// Keep the END of the message (most recent output)
	truncated := truncMsg + msg[len(msg)-availableSize:]

	// Ensure valid UTF-8 to prevent encoding issues
	return strings.ToValidUTF8(truncated, "�")
}

// ValidateMessageSize checks total message size before sending to Telegram
func ValidateMessageSize(msg string) error {
	if len(msg) > constants.TelegramMaxMessageSize {
		return fmt.Errorf("message size %d exceeds Telegram limit of %d", len(msg), constants.TelegramMaxMessageSize)
	}
	return nil
}
