package notifier

import (
	"context"
	"fmt"
	"time"

	"telegram-notifier/internal/config"
	"telegram-notifier/internal/constants"
	"telegram-notifier/internal/systemd"
	"telegram-notifier/internal/validation"
)

// NotificationError provides structured error context for notification failures
type NotificationError struct {
	Op      string
	Service string
	Err     error
}

func (e *NotificationError) Error() string {
	if e.Service != "" {
		return fmt.Sprintf("%s for service '%s': %v", e.Op, e.Service, e.Err)
	}
	return fmt.Sprintf("%s: %v", e.Op, e.Err)
}

func (e *NotificationError) Unwrap() error {
	return e.Err
}

// NotificationData contains all information for formatting a notification
type NotificationData struct {
	Hostname        string
	DateTime        string
	ProcessExitCode int
	ServiceStatus   string
	ServiceName     string
	ServiceDesc     string
	Message         string
	IsSuccess       bool
}

// SystemdService abstracts systemd operations for testing
type SystemdService interface {
	GetServiceInfo(ctx context.Context, serviceName string) (systemd.ServiceInfo, error)
	GetServiceCommandOutput(ctx context.Context, serviceName string, exitInfo systemd.ExitCodeInfo) (string, error)
	GetServiceExitCodeInfo(ctx context.Context, serviceName string) (systemd.ExitCodeInfo, error)
}

// TelegramClient abstracts Telegram API for testing
type TelegramClient interface {
	SendNotification(ctx context.Context, message string) error
}

type Service struct {
	systemd  SystemdService
	telegram TelegramClient
	config   *config.Config
}

func New(systemdService SystemdService, telegramClient TelegramClient, cfg *config.Config) *Service {
	return &Service{
		systemd:  systemdService,
		telegram: telegramClient,
		config:   cfg,
	}
}

// SendServiceNotification orchestrates notification creation and delivery
// SECURITY: Validates inputs, filters secrets, and sanitizes all output
func (s *Service) SendServiceNotification(ctx context.Context, exitInfo systemd.ExitCodeInfo, serviceName, serviceDesc, customMessage string) error {
	// Check for context cancellation early
	select {
	case <-ctx.Done():
		return s.wrapError("context cancelled", serviceName, ctx.Err())
	default:
	}

	// SECURITY: Validate service name to prevent injection attacks
	if err := validation.ValidateServiceName(serviceName); err != nil {
		return s.wrapError("validation failed", serviceName, err)
	}

	// Get service description from systemd or use provided value
	finalServiceDesc := s.getServiceDescription(ctx, serviceName, serviceDesc)

	// Get command output with automatic secret filtering
	finalMessage := s.getCommandOutput(ctx, serviceName, exitInfo, customMessage)

	// Get hostname (uses privacy alias if configured)
	hostname := s.config.GetHostname()

	// Build notification data structure
	data := NotificationData{
		Hostname:        hostname,
		DateTime:        s.config.FormatDateTime(time.Now()),
		ProcessExitCode: exitInfo.ProcessExitCode,
		ServiceStatus:   exitInfo.ExitStatus,
		ServiceName:     serviceName,
		ServiceDesc:     finalServiceDesc,
		Message:         finalMessage,
		IsSuccess:       exitInfo.ServiceSuccess,
	}

	// Format message and ensure it fits Telegram limits
	formattedMessage := s.formatAndValidateMessage(data)

	// Final context check before sending
	select {
	case <-ctx.Done():
		return s.wrapError("context cancelled before sending", serviceName, ctx.Err())
	default:
	}

	// Send notification via Telegram API
	if err := s.telegram.SendNotification(ctx, formattedMessage); err != nil {
		return s.wrapError("sending telegram notification", serviceName, err)
	}

	return nil
}

// getServiceDescription retrieves service description from systemd or uses provided value
func (s *Service) getServiceDescription(ctx context.Context, serviceName, providedDesc string) string {
	// Use provided description if it's meaningful (not empty or same as service name)
	if providedDesc != "" && providedDesc != serviceName {
		return providedDesc
	}

	// Fallback to systemd's description
	serviceInfo, err := s.systemd.GetServiceInfo(ctx, serviceName)
	if err != nil {
		return "Service description not available"
	}
	return serviceInfo.Description
}

// getCommandOutput retrieves and filters command output
// SECURITY: Filters secrets from both custom messages and systemd output
func (s *Service) getCommandOutput(ctx context.Context, serviceName string, exitInfo systemd.ExitCodeInfo, customMessage string) string {
	// Use custom message if provided
	if customMessage != "" {
		return validation.FilterSecrets(customMessage)
	}

	// Get output from systemd journal
	output, err := s.systemd.GetServiceCommandOutput(ctx, serviceName, exitInfo)
	if err != nil {
		// SECURITY: Filter secrets from error messages to prevent leakage
		sanitized := validation.SanitizeErrorMessage(err)
		return fmt.Sprintf("Unable to retrieve command output: %s", sanitized)
	}

	// Filter secrets and truncate to size limits
	filtered := validation.FilterSecrets(output)
	return validation.TruncateMessage(filtered, s.config.MaxOutputSize)
}

// formatAndValidateMessage creates Telegram-formatted message with size validation
func (s *Service) formatAndValidateMessage(data NotificationData) string {
	// Select status emoji based on success/failure
	status := "SUCCESS 🟢"
	if !data.IsSuccess {
		status = "FAILURE 🔴"
	}

	exitCodeDisplay := fmt.Sprintf("%d", data.ProcessExitCode)

	// Format message using Markdown for Telegram
	message := fmt.Sprintf(`*Automated Notification:* %s

- 🖥️  *Host:* `+"`%s`"+`
- 🕒  *Date/Time:* `+"`%s`"+`
- 🔢  *Process Exit Code:* `+"`%s`"+`
- ⚙️  *Service:* `+"`%s`"+`
- 📄  *Description:* `+"`%s`"+`

%s`,
		status,
		data.Hostname,
		data.DateTime,
		exitCodeDisplay,
		data.ServiceName,
		data.ServiceDesc,
		data.Message)

	// Ensure message fits within Telegram's 4096 character limit with safety margin
	maxSize := constants.TelegramMaxMessageSize - constants.MessageSafetyMargin
	if len(message) > maxSize {
		// Calculate how much space is available for the message content
		headerSize := len(message) - len(data.Message)
		allowedMessageSize := maxSize - headerSize

		if allowedMessageSize > 0 {
			// Truncate just the message content, keep headers intact
			truncatedMsg := validation.TruncateMessage(data.Message, allowedMessageSize)
			message = fmt.Sprintf(`*Automated Notification:* %s

- 🖥️  *Host:* `+"`%s`"+`
- 🕒  *Date/Time:* `+"`%s`"+`
- 🔢  *Process Exit Code:* `+"`%s`"+`
- ⚙️  *Service:* `+"`%s`"+`
- 📄  *Description:* `+"`%s`"+`

%s`,
				status, data.Hostname, data.DateTime,
				exitCodeDisplay, data.ServiceName, data.ServiceDesc, truncatedMsg)
		}
	}

	return message
}

// wrapError wraps errors with context and filters secrets
// SECURITY: All errors are filtered for secrets before being returned
func (s *Service) wrapError(op, service string, err error) error {
	if err == nil {
		return nil
	}
	// SECURITY: Filter secrets from all wrapped errors to prevent leakage
	filteredErr := validation.FilterSecretsFromError(err)
	return &NotificationError{Op: op, Service: service, Err: filteredErr}
}
