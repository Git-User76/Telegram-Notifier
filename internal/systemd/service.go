package systemd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"telegram-notifier/internal/config"
	"telegram-notifier/internal/constants"
	"telegram-notifier/internal/ratelimit"
	"telegram-notifier/internal/validation"
)

type SystemdScope int

const (
	ScopeUser SystemdScope = iota
	ScopeSystem
	ScopeBoth
)

type SystemctlResult struct {
	Output []byte
	Scope  SystemdScope
	Error  error
}

type ServiceInfo struct {
	Name        string
	Description string
}

type ExitCodeInfo struct {
	ProcessExitCode int
	ServiceSuccess  bool
	ExitSignal      string
	ExitStatus      string
	InvocationID    string
}

type CommandConfig struct {
	ServiceName  string
	InvocationID string
	SinceTime    string
	OutputFormat string
}

// CommandExecutor abstracts command execution for testing and security
type CommandExecutor interface {
	Execute(ctx context.Context, name string, args ...string) ([]byte, error)
}

type DefaultCommandExecutor struct{}

func NewCommandExecutor() CommandExecutor {
	return &DefaultCommandExecutor{}
}

// Execute runs commands with context for timeout control
// SECURITY: Uses exec.CommandContext with separated arguments to prevent shell injection
func (e *DefaultCommandExecutor) Execute(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.Output()
}

type Service struct {
	executor           CommandExecutor
	config             *config.Config
	commandRateLimiter *ratelimit.TokenBucket
	commandCheckOnce   sync.Once
	commandCheckErr    error
}

func NewService(executor CommandExecutor, cfg *config.Config) *Service {
	return &Service{
		executor: executor,
		config:   cfg,
		// Rate limiter prevents abuse by limiting command execution rate
		commandRateLimiter: ratelimit.NewTokenBucket(
			constants.CommandRateLimitTokens,
			constants.CommandRateLimitRefillRate,
		),
	}
}

// checkCommandAvailability verifies systemd commands exist before use
// SECURITY: Prevents confusing error messages and ensures systemd is installed
func (s *Service) checkCommandAvailability() error {
	s.commandCheckOnce.Do(func() {
		requiredCommands := []string{"systemctl", "journalctl"}
		var missing []string

		for _, cmd := range requiredCommands {
			if _, err := exec.LookPath(cmd); err != nil {
				missing = append(missing, cmd)
			}
		}

		if len(missing) > 0 {
			s.commandCheckErr = fmt.Errorf("required commands not found: %s (is systemd installed?)", strings.Join(missing, ", "))
		}
	})
	return s.commandCheckErr
}

// executeWithRateLimit wraps command execution with rate limiting and availability checks
// SECURITY: Prevents command execution DoS by limiting rate of execution
func (s *Service) executeWithRateLimit(ctx context.Context, name string, args ...string) ([]byte, error) {
	// Verify commands exist before attempting execution
	if err := s.checkCommandAvailability(); err != nil {
		return nil, err
	}

	// Apply rate limiting to prevent command execution abuse
	rateLimitCtx, cancel := context.WithTimeout(ctx, constants.CommandRateLimitMaxWait)
	defer cancel()

	if err := s.commandRateLimiter.Wait(rateLimitCtx); err != nil {
		return nil, fmt.Errorf("command rate limit exceeded: %w", err)
	}

	return s.executor.Execute(ctx, name, args...)
}

// ExecSystemctl executes systemctl commands with automatic scope fallback
// Tries user scope first (safer), then system scope
func (s *Service) ExecSystemctl(ctx context.Context, scope SystemdScope, args ...string) SystemctlResult {
	select {
	case <-ctx.Done():
		return SystemctlResult{Error: validation.FilterSecretsFromError(ctx.Err())}
	default:
	}

	tryScopes := s.getScopesToTry(scope)

	var lastErr error
	for _, isUser := range tryScopes {
		cmdArgs := s.buildCommandArgs(isUser, args)
		output, err := s.executeWithRateLimit(ctx, "systemctl", cmdArgs...)
		if err == nil && len(output) > 0 {
			return SystemctlResult{
				Output: output,
				Scope:  map[bool]SystemdScope{true: ScopeUser, false: ScopeSystem}[isUser],
			}
		}
		lastErr = err
	}

	return SystemctlResult{Scope: scope, Error: validation.FilterSecretsFromError(lastErr)}
}

// ExecJournalctl executes journalctl with validated service name
// SECURITY: Validates service name before execution and filters secrets from errors
func (s *Service) ExecJournalctl(ctx context.Context, config CommandConfig, scope SystemdScope) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, validation.FilterSecretsFromError(ctx.Err())
	default:
	}

	// Prevent command injection via service name
	if err := validation.ValidateServiceName(config.ServiceName); err != nil {
		return nil, validation.FilterSecretsFromError(err)
	}

	tryScopes := s.getScopesToTry(scope)

	var lastErr error
	for _, isUser := range tryScopes {
		cmdArgs := s.buildJournalArgs(isUser, config)
		output, err := s.executeWithRateLimit(ctx, "journalctl", cmdArgs...)
		if err == nil && len(output) > 0 {
			return output, nil
		}
		lastErr = err
	}

	if lastErr != nil {
		return nil, validation.FilterSecretsFromError(fmt.Errorf("journalctl failed for '%s': %w", config.ServiceName, lastErr))
	}
	return nil, fmt.Errorf("no journal output for '%s'", config.ServiceName)
}

// GetSystemctlProperty retrieves a specific systemctl property
// SECURITY: Validates service name and filters secrets from output
func (s *Service) GetSystemctlProperty(ctx context.Context, serviceName, property string, scope SystemdScope) (string, error) {
	// Prevent injection attacks via service name
	if err := validation.ValidateServiceName(serviceName); err != nil {
		return "", validation.FilterSecretsFromError(err)
	}

	result := s.ExecSystemctl(ctx, scope, "show", serviceName, "--property="+property, "--no-pager")
	if result.Error != nil {
		return "", validation.FilterSecretsFromError(fmt.Errorf("getting property '%s': %w", property, result.Error))
	}

	value := strings.TrimSpace(string(result.Output))
	return strings.TrimPrefix(value, property+"="), nil
}

// GetServiceInfo retrieves service description from systemctl or service files
func (s *Service) GetServiceInfo(ctx context.Context, serviceName string) (ServiceInfo, error) {
	// Validate service name to prevent path traversal and injection
	if err := validation.ValidateServiceName(serviceName); err != nil {
		return ServiceInfo{}, validation.FilterSecretsFromError(err)
	}

	select {
	case <-ctx.Done():
		return ServiceInfo{}, validation.FilterSecretsFromError(ctx.Err())
	default:
	}

	// Prefer systemctl (authoritative source)
	description, err := s.GetSystemctlProperty(ctx, serviceName, "Description", ScopeBoth)
	if err == nil && description != "" && description != serviceName {
		return ServiceInfo{Name: serviceName, Description: description}, nil
	}

	// Fallback to reading service files directly
	desc, err := s.readServiceFileDescription(serviceName)
	if err == nil && desc != "" {
		return ServiceInfo{Name: serviceName, Description: desc}, nil
	}

	return ServiceInfo{Name: serviceName, Description: "Service description not available"}, nil
}

// GetServiceExitCodeInfo retrieves exit code information from environment or systemctl
// Prioritizes environment variables (most reliable in systemd context)
func (s *Service) GetServiceExitCodeInfo(ctx context.Context, serviceName string) (ExitCodeInfo, error) {
	// Validate service name first
	if err := validation.ValidateServiceName(serviceName); err != nil {
		return ExitCodeInfo{}, validation.FilterSecretsFromError(err)
	}

	info := ExitCodeInfo{
		ProcessExitCode: 0,
		ServiceSuccess:  true,
		ExitStatus:      "0/SUCCESS",
		InvocationID:    os.Getenv("INVOCATION_ID"),
	}

	select {
	case <-ctx.Done():
		return info, validation.FilterSecretsFromError(ctx.Err())
	default:
	}

	// Read systemd environment variables (most reliable source)
	if exitStatus := os.Getenv("EXIT_STATUS"); exitStatus != "" {
		if code, err := strconv.Atoi(exitStatus); err == nil {
			if err := validation.ValidateExitCode(code); err == nil {
				info.ProcessExitCode = code
				info.ServiceSuccess = (code == 0)
				info.ExitStatus = GetExitStatusString(code)
			}
		}
	}

	if serviceResult := os.Getenv("SERVICE_RESULT"); serviceResult != "" {
		info.ServiceSuccess = (serviceResult == "success")
	}

	// Fallback to systemctl properties
	for prop, handler := range s.getPropertyHandlers(&info) {
		if value, err := s.GetSystemctlProperty(ctx, serviceName, prop, ScopeBoth); err == nil {
			handler(value)
		}
	}

	return info, nil
}

// readServiceFileDescription reads Description from systemd unit files
func (s *Service) readServiceFileDescription(serviceName string) (string, error) {
	paths := s.getServicePaths(serviceName)

	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		// Parse Description= line from [Unit] section
		for _, line := range strings.Split(string(content), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Description=") {
				return strings.TrimPrefix(line, "Description="), nil
			}
		}
	}

	return "", fmt.Errorf("no description found")
}

// getServicePaths generates possible service file locations
func (s *Service) getServicePaths(serviceName string) []string {
	var paths []string
	baseDirs := []string{
		"/etc/systemd/system",
		"/usr/lib/systemd/system",
		"/lib/systemd/system",
		"/etc/systemd/user",
		"/usr/lib/systemd/user",
	}

	if homeDir, err := os.UserHomeDir(); err == nil {
		userDirs := []string{
			filepath.Join(homeDir, ".config/systemd/user"),
			filepath.Join(homeDir, ".local/share/systemd/user"),
		}
		baseDirs = append(userDirs, baseDirs...)
	}

	for _, baseDir := range baseDirs {
		paths = append(paths, filepath.Join(baseDir, serviceName))
	}

	return paths
}

func (s *Service) getScopesToTry(scope SystemdScope) []bool {
	switch scope {
	case ScopeUser:
		return []bool{true}
	case ScopeSystem:
		return []bool{false}
	case ScopeBoth:
		// Try user scope first (safer, less privileged)
		return []bool{true, false}
	default:
		return []bool{false}
	}
}

// buildCommandArgs adds --user flag for user scope commands
func (s *Service) buildCommandArgs(isUser bool, args []string) []string {
	cmdArgs := make([]string, 0, len(args)+1)
	if isUser {
		cmdArgs = append(cmdArgs, "--user")
	}
	return append(cmdArgs, args...)
}

// buildJournalArgs constructs journalctl command arguments safely
// SECURITY: Service name already validated, invocation ID from trusted source
func (s *Service) buildJournalArgs(isUser bool, config CommandConfig) []string {
	cmdArgs := []string{}
	if isUser {
		cmdArgs = append(cmdArgs, "--user")
	}

	cmdArgs = append(cmdArgs, "-u", config.ServiceName)

	// Use invocation ID for precise log scoping (prevents race conditions)
	if config.InvocationID != "" {
		cmdArgs = append(cmdArgs, "_SYSTEMD_INVOCATION_ID="+config.InvocationID)
	} else if config.SinceTime != "" {
		cmdArgs = append(cmdArgs, "--since", config.SinceTime)
	}

	cmdArgs = append(cmdArgs, "--no-pager")

	if config.OutputFormat != "" {
		cmdArgs = append(cmdArgs, "--output="+config.OutputFormat)
	}

	return cmdArgs
}

func (s *Service) getPropertyHandlers(info *ExitCodeInfo) map[string]func(string) {
	return map[string]func(string){
		"ExecMainStatus": func(value string) {
			if code, err := strconv.Atoi(value); err == nil {
				if validation.ValidateExitCode(code) == nil {
					info.ProcessExitCode = code
					info.ExitStatus = GetExitStatusString(code)
				}
			}
		},
		"ExecMainCode": func(value string) {
			if value == "2" || strings.Contains(value, "killed") {
				info.ExitSignal = "killed"
			}
		},
		"Result": func(value string) {
			info.ServiceSuccess = (value == "success")
		},
	}
}

// GetExitStatusString converts numeric exit codes to human-readable strings
// Maps standard systemd exit codes (200-245) to their symbolic names
func GetExitStatusString(code int) string {
	interpretations := map[int]string{
		0: "0/SUCCESS", 1: "1/FAILURE", 2: "2/INVALIDARGUMENT",
		126: "126/CANTEXEC", 127: "127/NOTFOUND", 200: "200/CHDIR",
		201: "201/NICE", 202: "202/FDS", 203: "203/EXEC",
		204: "204/MEMORY", 205: "205/LIMITS", 206: "206/OOM_ADJUST",
		207: "207/SIGNAL_MASK", 208: "208/STDIN", 209: "209/STDOUT",
		210: "210/CHROOT", 211: "211/IOPRIO", 212: "212/TIMERSLACK",
		213: "213/SECUREBITS", 214: "214/SETSCHEDULER", 215: "215/CPUAFFINITY",
		216: "216/GROUP", 217: "217/USER", 218: "218/CAPABILITIES",
		219: "219/CGROUP", 220: "220/SETSID", 221: "221/CONFIRM",
		222: "222/STDERR", 224: "224/PAM", 225: "225/NETWORK",
		226: "226/NAMESPACE", 227: "227/NO_NEW_PRIVILEGES", 228: "228/SECCOMP",
		229: "229/SELINUX_CONTEXT", 230: "230/PERSONALITY", 231: "231/APPARMOR_PROFILE",
		232: "232/ADDRESS_FAMILIES", 233: "233/RUNTIME_DIRECTORY", 234: "234/MAKE_STARTER",
		235: "235/CHOWN", 236: "236/SMACK_PROCESS_LABEL", 237: "237/KEYRING",
		238: "238/STATE_DIRECTORY", 239: "239/CACHE_DIRECTORY", 240: "240/LOGS_DIRECTORY",
		241: "241/CONFIGURATION_DIRECTORY", 242: "242/NUMA_POLICY", 243: "243/CREDENTIALS",
		245: "245/BPF",
	}

	if interpretation, ok := interpretations[code]; ok {
		return interpretation
	}
	return fmt.Sprintf("%d", code)
}
