package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"telegram-notifier/internal/config"
	"telegram-notifier/internal/notifier"
	"telegram-notifier/internal/systemd"
	"telegram-notifier/internal/telegram"
	"telegram-notifier/internal/validation"
)

func main() {
	if len(os.Args) < 2 {
		printError("Missing required arguments")
		printUsage()
		os.Exit(1)
	}

	if os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		printUsage()
		os.Exit(0)
	}

	// Load and validate configuration from environment
	cfg, err := config.New()
	if err != nil {
		// SECURITY: Sanitize error messages to prevent information disclosure
		log.Fatalf("Configuration error: %s", validation.SanitizeErrorMessage(err))
	}

	// Create context with timeout to prevent indefinite hangs
	ctx, cancel := context.WithTimeout(context.Background(), cfg.CommandTimeout)
	defer cancel()

	// Parse command-line arguments with validation
	exitInfo, serviceName, serviceDesc, customMessage, err := parseCommandLineArgs(os.Args)
	if err != nil {
		printError(validation.SanitizeErrorMessage(err))
		printUsage()
		os.Exit(1)
	}

	// SECURITY: Validate service name early to prevent injection attacks
	if err := validation.ValidateServiceName(serviceName); err != nil {
		log.Fatalf("Invalid service name: %s", validation.SanitizeErrorMessage(err))
	}

	// Initialize services with dependency injection for testability
	commandExecutor := systemd.NewCommandExecutor()
	systemdService := systemd.NewService(commandExecutor, cfg)
	telegramClient := telegram.NewClient(cfg, nil)
	notifierService := notifier.New(systemdService, telegramClient, cfg)

	// Send notification with full error context
	if err := notifierService.SendServiceNotification(ctx, exitInfo, serviceName, serviceDesc, customMessage); err != nil {
		if notifErr, ok := err.(*notifier.NotificationError); ok {
			log.Fatalf("Notification failed - %s: %s", notifErr.Op, validation.SanitizeErrorMessage(notifErr.Err))
		}
		log.Fatalf("Notification failed: %s", validation.SanitizeErrorMessage(err))
	}

	fmt.Printf("Notification sent successfully for service: %s (exit code: %d, status: %s)\n",
		serviceName,
		exitInfo.ProcessExitCode,
		map[bool]string{true: "succeeded", false: "failed"}[exitInfo.ServiceSuccess])
}

// parseCommandLineArgs determines execution mode and extracts arguments
// Supports two modes: systemd integration (automatic) and manual testing
func parseCommandLineArgs(args []string) (systemd.ExitCodeInfo, string, string, string, error) {
	var exitInfo systemd.ExitCodeInfo

	// Detect systemd context by checking for systemd environment variables
	exitStatusEnv := os.Getenv("EXIT_STATUS")
	serviceResultEnv := os.Getenv("SERVICE_RESULT")
	mainPidEnv := os.Getenv("MAINPID")
	invocationIDEnv := os.Getenv("INVOCATION_ID")

	inSystemdContext := exitStatusEnv != "" || serviceResultEnv != "" || mainPidEnv != "" || invocationIDEnv != ""

	// Create temporary service for systemd mode detection
	tempConfig := &config.Config{}
	tempConfig.SetDefaults()
	systemdService := systemd.NewService(systemd.NewCommandExecutor(), tempConfig)

	// Auto-detect mode: systemd integration if in systemd context or single arg
	if inSystemdContext || len(args) == 2 {
		return parseSystemdMode(args, systemdService)
	} else if len(args) >= 3 {
		return parseManualMode(args)
	}

	return exitInfo, "", "", "", fmt.Errorf("invalid number of arguments")
}

// parseSystemdMode handles systemd ExecStartPost/ExecStopPost execution
// Reads exit code from systemd environment variables or systemctl
func parseSystemdMode(args []string, systemdService *systemd.Service) (systemd.ExitCodeInfo, string, string, string, error) {
	serviceName := args[1]

	// SECURITY: Validate service name immediately to prevent injection
	if err := validation.ValidateServiceName(serviceName); err != nil {
		return systemd.ExitCodeInfo{}, "", "", "", fmt.Errorf("invalid service name: %w", err)
	}

	// Get exit code info from systemd (uses environment vars + systemctl)
	exitInfo, err := systemdService.GetServiceExitCodeInfo(context.Background(), serviceName)
	if err != nil {
		log.Printf("Warning: failed to get exit code info: %s", validation.SanitizeErrorMessage(err))
	}

	// Parse optional service description and custom message
	var serviceDesc, customMessage string
	if len(args) >= 3 {
		if len(args) >= 4 {
			serviceDesc = args[2]
			customMessage = args[3]
		} else {
			// Auto-detect if arg is status message or description
			if isStatusMessage(args[2]) {
				customMessage = args[2]
			} else {
				serviceDesc = args[2]
			}
		}
	}

	return exitInfo, serviceName, serviceDesc, customMessage, nil
}

// parseManualMode handles manual invocation for testing
// Usage: telegram-notifier <exit_code> <service_name> [description] [message]
func parseManualMode(args []string) (systemd.ExitCodeInfo, string, string, string, error) {
	exitCodeStr := args[1]
	serviceName := args[2]

	// SECURITY: Validate service name to prevent injection
	if err := validation.ValidateServiceName(serviceName); err != nil {
		return systemd.ExitCodeInfo{}, "", "", "", fmt.Errorf("invalid service name: %w", err)
	}

	// Parse and validate exit code
	code, err := strconv.Atoi(exitCodeStr)
	if err != nil {
		return systemd.ExitCodeInfo{}, "", "", "", fmt.Errorf("invalid exit code '%s': %w", exitCodeStr, err)
	}

	// SECURITY: Ensure exit code is in valid range (0-255)
	if err := validation.ValidateExitCode(code); err != nil {
		return systemd.ExitCodeInfo{}, "", "", "", err
	}

	exitInfo := systemd.ExitCodeInfo{
		ProcessExitCode: code,
		ServiceSuccess:  (code == 0),
		ExitStatus:      systemd.GetExitStatusString(code),
		InvocationID:    os.Getenv("INVOCATION_ID"),
	}

	// Parse optional service description and custom message
	var serviceDesc, customMessage string
	if len(args) >= 4 {
		if isStatusMessage(args[3]) {
			customMessage = args[3]
			if len(args) >= 5 {
				serviceDesc = args[4]
			}
		} else {
			serviceDesc = args[3]
			if len(args) >= 5 {
				customMessage = args[4]
			}
		}
	}

	return exitInfo, serviceName, serviceDesc, customMessage, nil
}

// isStatusMessage heuristically detects if argument is a status message
// Used to auto-detect argument order when description/message are swapped
func isStatusMessage(arg string) bool {
	lowerArg := strings.ToLower(arg)
	statusWords := []string{"success", "fail", "complete", "error", "start", "stop"}
	for _, word := range statusWords {
		if strings.Contains(lowerArg, word) {
			return true
		}
	}
	return false
}

func printError(msg string) {
	fmt.Fprintf(os.Stderr, "Error: %s\n\n", msg)
}

func printUsage() {
	fmt.Println("Usage:")
	fmt.Println("  Mode 1 - Manual (for testing):")
	fmt.Println("    ./telegram-notifier <exit_code> <service_name> [custom_message]")
	fmt.Println("")
	fmt.Println("  Mode 2 - Systemd Integration:")
	fmt.Println("    ./telegram-notifier <service_name> [custom_message]")
	fmt.Println("    ./telegram-notifier <service_name> [service_description] [custom_message]")
	fmt.Println("    (Uses $EXIT_STATUS, $SERVICE_RESULT, and other environment variables)")
	fmt.Println("")
	fmt.Println("Examples:")
	fmt.Println("  # Manual mode")
	fmt.Println("  ./telegram-notifier 0 my-backup.service \"Backup completed\"")
	fmt.Println("  ./telegram-notifier 200 my-app.service \"Application failed with CHDIR error\"")
	fmt.Println("")
	fmt.Println("  # Systemd mode (in ExecStartPost/ExecStopPost)")
	fmt.Println("  ExecStartPost=/usr/local/bin/telegram-notifier %n")
	fmt.Println("  ExecStopPost=/usr/local/bin/telegram-notifier %n")
	fmt.Println("")
	fmt.Println("Security:")
	fmt.Println("  Service names must match systemd naming conventions (alphanumeric, :_.@-)")
	fmt.Println("  Shell metacharacters are rejected to prevent command injection")
	fmt.Println("  Exit codes must be in range 0-255")
	fmt.Println("  Sensitive data is automatically filtered from output")
	fmt.Println("  Command execution is rate-limited to prevent abuse")
	fmt.Println("")
	fmt.Println("Privacy:")
	fmt.Println("  Set NOTIFIER_HOSTNAME_ALIAS to use a custom hostname in notifications")
	fmt.Println("  All error messages are sanitized before logging")
	fmt.Println("")
	fmt.Println("Configuration (set in ~/.config/environment.d/*.conf):")
	fmt.Println("  TELEGRAM_BOT_TOKEN       - Telegram bot token (required)")
	fmt.Println("  TELEGRAM_CHAT_ID         - Telegram chat ID (required)")
	fmt.Println("  NOTIFIER_HOSTNAME_ALIAS  - Custom hostname for privacy")
	fmt.Println("  TZ                       - Timezone (e.g., America/New_York, UTC)")
	fmt.Println("  NOTIFIER_COMMAND_TIMEOUT - Max command execution time (default: 30s)")
	fmt.Println("  NOTIFIER_MAX_OUTPUT_SIZE - Max output characters (default: 2500)")
	fmt.Println("")
	fmt.Println("Exit Codes:")
	fmt.Println("  0   - SUCCESS")
	fmt.Println("  1   - Generic failure")
	fmt.Println("  126 - Command cannot execute")
	fmt.Println("  127 - Command not found")
	fmt.Println("  200 - Change directory failed (CHDIR)")
	fmt.Println("  203 - Cannot execute (EXEC)")
	fmt.Println("  See systemd.exec(5) for full list")
}
