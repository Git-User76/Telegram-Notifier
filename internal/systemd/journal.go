package systemd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"telegram-notifier/internal/validation"
)

// JournalOutput contains parsed journal logs and command output
type JournalOutput struct {
	SystemdLogs      []string  // Systemd service lifecycle messages
	ExecutionResults []string  // Actual command/script output
	StartTime        time.Time // Service start timestamp
}

// GetCurrentExecutionLogs retrieves logs for the current service execution
// SECURITY: Uses invocation ID from environment to prevent race conditions
func (s *Service) GetCurrentExecutionLogs(ctx context.Context, serviceName string) (JournalOutput, error) {
	var output JournalOutput

	// Check context cancellation
	select {
	case <-ctx.Done():
		return output, validation.FilterSecretsFromError(ctx.Err())
	default:
	}

	// Use invocation ID from environment if available (prevents TOCTOU race)
	// This ensures we get logs for THIS exact execution, not a concurrent one
	invocationID := os.Getenv("INVOCATION_ID")
	sinceTime := time.Now().Add(-s.config.JournalLookback).Format("2006-01-02 15:04:05")

	config := CommandConfig{
		ServiceName:  serviceName,
		InvocationID: invocationID,
		SinceTime:    sinceTime,
		OutputFormat: "short",
	}

	journalRaw, err := s.ExecJournalctl(ctx, config, ScopeBoth)
	if err != nil {
		return output, validation.FilterSecretsFromError(fmt.Errorf("executing journalctl: %w", err))
	}

	// Parse journal output line by line
	lines := strings.Split(string(journalRaw), "\n")
	foundStart := invocationID != "" // If we have invocation ID, already scoped
	var lastProcessName string
	inCommandOutput := false

	for _, line := range lines {
		processJournalLine(line, serviceName, &output, &foundStart, &lastProcessName, &inCommandOutput)
	}

	return output, nil
}

// GetSimpleCommandOutput retrieves command output in compact format
// Falls back to cat format for cleaner output without timestamps
func (s *Service) GetSimpleCommandOutput(ctx context.Context, serviceName string) (string, error) {
	select {
	case <-ctx.Done():
		return "", validation.FilterSecretsFromError(ctx.Err())
	default:
	}

	sinceTime := s.config.JournalSinceDefault

	// Try to get the command name for better output filtering
	execStart, _ := s.GetSystemctlProperty(ctx, serviceName, "ExecStart", ScopeBoth)
	var execCommand string
	if execStart != "" {
		parts := strings.Fields(execStart)
		if len(parts) > 0 {
			execCommand = parts[0]
			// Extract just the command name (strip path)
			if idx := strings.LastIndex(execCommand, "/"); idx != -1 {
				execCommand = execCommand[idx+1:]
			}
		}
	}

	// Use 'cat' format for cleaner output (no timestamps/metadata)
	config := CommandConfig{
		ServiceName:  serviceName,
		SinceTime:    sinceTime,
		OutputFormat: "cat",
	}

	output, err := s.ExecJournalctl(ctx, config, ScopeBoth)
	if err == nil && len(output) > 0 {
		result := s.processSimpleOutput(string(output), serviceName, execCommand)
		if result != "" {
			return result, nil
		}
	}

	return "", fmt.Errorf("no command output found for service '%s'", serviceName)
}

// GetServiceCommandOutput retrieves command output with fallback strategies
// SECURITY: Uses invocation ID from exitInfo to ensure consistency across calls
func (s *Service) GetServiceCommandOutput(ctx context.Context, serviceName string, exitInfo ExitCodeInfo) (string, error) {
	select {
	case <-ctx.Done():
		return "", validation.FilterSecretsFromError(ctx.Err())
	default:
	}

	// Try using invocation ID first (most reliable, prevents race conditions)
	if exitInfo.InvocationID != "" {
		config := CommandConfig{
			ServiceName:  serviceName,
			InvocationID: exitInfo.InvocationID,
			OutputFormat: "cat",
		}
		if output, err := s.ExecJournalctl(ctx, config, ScopeBoth); err == nil && len(output) > 0 {
			result := s.processSimpleOutput(string(output), serviceName, "")
			if result != "" {
				return result, nil
			}
		}
	}

	// Fallback to time-based log retrieval
	output, err := s.GetCurrentExecutionLogs(ctx, serviceName)
	if err != nil {
		return "", validation.FilterSecretsFromError(fmt.Errorf("getting execution logs: %w", err))
	}

	return s.FormatServiceOutput(ctx, output, exitInfo, serviceName), nil
}

// FormatServiceOutput formats systemd logs and command output for notification
func (s *Service) FormatServiceOutput(ctx context.Context, output JournalOutput, exitInfo ExitCodeInfo, serviceName string) string {
	var result strings.Builder

	// Format systemd lifecycle logs
	result.WriteString("*Systemd Service*\n```\n")
	if len(output.SystemdLogs) == 0 {
		if exitInfo.ServiceSuccess {
			result.WriteString("Service completed successfully")
		} else {
			result.WriteString(fmt.Sprintf("Service failed with exit code %d", exitInfo.ProcessExitCode))
		}
	} else {
		for _, log := range output.SystemdLogs {
			// Add exit code interpretation to main process exit messages
			if strings.Contains(log, "Main process exited") && exitInfo.ProcessExitCode != 0 {
				log = fmt.Sprintf("%s\n→ Process exit code: %s", log, GetExitStatusString(exitInfo.ProcessExitCode))
			}
			result.WriteString(log)
			result.WriteString("\n")
		}
	}
	result.WriteString("```\n")

	// Format command output
	result.WriteString("\n*Command Output*\n```\n")
	if len(output.ExecutionResults) == 0 {
		// Try fallback method if no execution results captured
		simpleOutput, err := s.GetSimpleCommandOutput(ctx, serviceName)
		if err != nil {
			if exitInfo.ServiceSuccess {
				result.WriteString("Command completed with no output")
			} else {
				result.WriteString(fmt.Sprintf("Command failed with exit code %d (no output)", exitInfo.ProcessExitCode))
			}
		} else {
			result.WriteString(simpleOutput)
		}
	} else {
		fullOutput := strings.Join(output.ExecutionResults, "\n")
		result.WriteString(validation.TruncateMessage(fullOutput, s.config.MaxOutputSize))
	}
	result.WriteString("\n```")

	return result.String()
}

// processSimpleOutput extracts command output from journal, filtering systemd metadata
func (s *Service) processSimpleOutput(output, serviceName, execCommand string) string {
	lines := strings.Split(output, "\n")
	var commandOutput []string
	captureOutput := false
	serviceStartSeen := false

	for _, line := range lines {
		skip, reset := shouldSkipLine(line, serviceName)

		// Reset capture state when service starts (new execution)
		if reset {
			captureOutput = true
			serviceStartSeen = true
			commandOutput = []string{}
			continue
		}

		// Skip systemd metadata lines
		if skip {
			if strings.Contains(line, "Finished") || strings.Contains(line, "Failed") {
				captureOutput = false
			}
			continue
		}

		// Capture command output lines
		if captureOutput {
			commandOutput = append(commandOutput, line)
		} else if !serviceStartSeen && execCommand != "" && strings.Contains(line, execCommand) {
			// Start capturing when we see the command execution
			captureOutput = true
			commandOutput = append(commandOutput, line)
		}
	}

	if len(commandOutput) > 0 {
		result := strings.Join(commandOutput, "\n")
		// Clean up extra whitespace
		result = strings.TrimPrefix(result, "\n\n")
		result = strings.TrimSuffix(result, "\n\n")
		return validation.TruncateMessage(result, s.config.MaxOutputSize)
	}

	return ""
}

// processJournalLine parses a single journal line and categorizes it
// Separates systemd lifecycle messages from actual command output
func processJournalLine(line, serviceName string, output *JournalOutput, foundStart *bool, lastProcessName *string, inCommandOutput *bool) {
	// Skip separator lines and self-referential logs
	if strings.HasPrefix(line, "-- ") || strings.Contains(line, "telegram-notifier") {
		return
	}

	// Detect service start to reset state (new execution)
	if strings.Contains(line, "Starting") && strings.Contains(line, serviceName) {
		*foundStart = true
		output.SystemdLogs = []string{}
		output.ExecutionResults = []string{}
		*lastProcessName = ""
		*inCommandOutput = false
		return
	}

	// Only process logs after service start
	if !*foundStart {
		return
	}

	processName := extractProcessName(line)
	msg := extractMessage(line)

	// Categorize systemd lifecycle messages
	if processName == "systemd" || strings.Contains(processName, "systemd[") {
		if strings.Contains(msg, "Starting") || strings.Contains(msg, "Started") ||
			strings.Contains(msg, "Finished") || strings.Contains(msg, "Failed") ||
			strings.Contains(msg, "Deactivated") {
			output.SystemdLogs = append(output.SystemdLogs, msg)
			*inCommandOutput = false
		}
	} else if processName != "" && processName != "systemd" {
		// Categorize actual command output
		if msg == "" && *inCommandOutput {
			output.ExecutionResults = append(output.ExecutionResults, "")
		} else if msg != "" {
			output.ExecutionResults = append(output.ExecutionResults, msg)
			*inCommandOutput = true
		}
		*lastProcessName = processName
	} else if *lastProcessName != "" && *lastProcessName != "systemd" {
		// Continue capturing output from same process
		output.ExecutionResults = append(output.ExecutionResults, msg)
		*inCommandOutput = true
	}
}

// extractProcessName extracts process name from journal line
// Format: "month day time hostname processname[pid]: message"
func extractProcessName(line string) string {
	if idx := strings.Index(line, "["); idx > 0 {
		beforeBracket := line[:idx]
		lastSpace := strings.LastIndex(beforeBracket, " ")
		if lastSpace != -1 {
			processName := beforeBracket[lastSpace+1:]
			// Verify there's a closing bracket with colon
			if endIdx := strings.Index(line[idx:], "]:"); endIdx > 0 {
				return processName
			}
		}
	}
	return ""
}

// extractMessage extracts message content from journal line
func extractMessage(line string) string {
	if line == "" {
		return ""
	}

	// Standard format: "processname[pid]: message"
	if idx := strings.Index(line, "]: "); idx != -1 {
		return line[idx+3:]
	}

	// Alternative format: "field: message" (after 3+ space-separated fields)
	if idx := strings.Index(line, ": "); idx != -1 {
		beforeColon := line[:idx]
		if strings.Count(beforeColon, " ") >= 3 {
			parts := strings.Fields(line)
			if len(parts) > 3 {
				msgStart := strings.Index(line, parts[3])
				if msgStart != -1 {
					remaining := line[msgStart:]
					if colonIdx := strings.Index(remaining, ": "); colonIdx != -1 {
						return remaining[colonIdx+2:]
					}
				}
			}
		} else {
			return line
		}
	}

	// Indented continuation lines
	if strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t") {
		return line
	}

	return ""
}

// shouldSkipLine determines if a journal line should be filtered out
// Returns (skip, reset) where reset indicates a service restart
func shouldSkipLine(line, serviceName string) (skip bool, reset bool) {
	trimmedLine := strings.TrimSpace(line)

	// Skip journal separator lines
	if strings.HasPrefix(trimmedLine, "-- ") {
		return true, false
	}

	// Reset on service start (new execution)
	if trimmedLine != "" && strings.Contains(trimmedLine, "Starting ") && strings.Contains(trimmedLine, serviceName) {
		return false, true
	}

	// Skip self-referential logs and completion messages
	if trimmedLine != "" && (strings.Contains(trimmedLine, "telegram-notifier") ||
		(strings.Contains(trimmedLine, "Finished ") && strings.Contains(trimmedLine, serviceName)) ||
		(strings.Contains(trimmedLine, "Failed ") && strings.Contains(trimmedLine, serviceName))) {
		return true, false
	}

	// Skip systemd metadata lines
	systemdMessages := []string{
		"Starting ", "Started ", "Stopping ", "Stopped ",
		"Deactivated ", "systemd[", "Triggering", "Consumed",
		"memory peak", "Failed with result", "Control process exited",
	}

	if trimmedLine != "" {
		for _, msg := range systemdMessages {
			if strings.Contains(trimmedLine, msg) {
				return true, false
			}
		}
	}

	return false, false
}
