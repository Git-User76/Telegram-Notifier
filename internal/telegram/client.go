package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"telegram-notifier/internal/config"
	"telegram-notifier/internal/constants"
	"telegram-notifier/internal/ratelimit"
	"telegram-notifier/internal/validation"
)

// Message represents a Telegram API message request
type Message struct {
	ChatID    string `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode"` // "Markdown" for formatted messages
}

// HTTPClient abstracts HTTP operations for testing and customization
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client handles communication with Telegram Bot API
type Client struct {
	config      *config.Config
	httpClient  HTTPClient
	apiBaseURL  string
	rateLimiter *ratelimit.TokenBucket
}

// NewClient creates a new Telegram API client with rate limiting
func NewClient(cfg *config.Config, httpClient HTTPClient) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: cfg.HTTPTimeout}
	}

	return &Client{
		config:     cfg,
		httpClient: httpClient,
		apiBaseURL: "https://api.telegram.org",
		// SECURITY: Rate limiter prevents API abuse and respects Telegram's limits
		rateLimiter: ratelimit.NewTokenBucket(constants.RateLimitTokens, constants.RateLimitRefillRate),
	}
}

// SendNotification sends a message to Telegram with retry logic
// SECURITY: Validates message size, applies rate limiting, and uses exponential backoff
func (c *Client) SendNotification(ctx context.Context, message string) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("context cancelled: %w", ctx.Err())
	default:
	}

	// SECURITY: Validate message doesn't exceed Telegram's limits
	if err := validation.ValidateMessageSize(message); err != nil {
		return fmt.Errorf("message validation failed: %w", err)
	}

	// SECURITY: Apply rate limiting to prevent API abuse
	if err := c.rateLimiter.Wait(ctx); err != nil {
		return fmt.Errorf("rate limit error: %w", err)
	}

	// Retry with exponential backoff for transient failures
	var lastErr error
	for attempt := 0; attempt <= constants.MaxHTTPRetries; attempt++ {
		if attempt > 0 {
			delay := c.calculateBackoff(attempt)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return fmt.Errorf("retry cancelled: %w", ctx.Err())
			}
		}

		err := c.sendRequest(ctx, message)
		if err == nil {
			return nil
		}

		lastErr = err

		// Don't retry on client errors (4xx) - these won't succeed on retry
		if isClientError(err) {
			return err
		}
	}

	return fmt.Errorf("failed after %d retries: %w", constants.MaxHTTPRetries, lastErr)
}

// sendRequest performs the actual HTTP request to Telegram API
// SECURITY: Uses context for timeout control and proper error handling
func (c *Client) sendRequest(ctx context.Context, message string) error {
	url := fmt.Sprintf("%s/bot%s/sendMessage", c.apiBaseURL, c.config.BotToken)

	msg := Message{
		ChatID:    c.config.ChatID,
		Text:      message,
		ParseMode: "Markdown",
	}

	jsonData, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal error: %w", err)
	}

	// Create request with context for cancellation support
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("request creation error: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		select {
		case <-ctx.Done():
			return fmt.Errorf("request cancelled: %w", ctx.Err())
		default:
			return fmt.Errorf("http error: %w", err)
		}
	}
	defer resp.Body.Close()

	// Check for API errors and extract meaningful error messages
	if resp.StatusCode != http.StatusOK {
		var errorResponse map[string]interface{}
		if json.NewDecoder(resp.Body).Decode(&errorResponse) == nil {
			if description, ok := errorResponse["description"].(string); ok {
				return &HTTPError{StatusCode: resp.StatusCode, Message: description}
			}
		}
		return &HTTPError{StatusCode: resp.StatusCode, Message: "unknown error"}
	}

	return nil
}

// calculateBackoff computes exponential backoff delay for retries
// Implements exponential backoff: delay = InitialDelay * (BackoffFactor ^ (attempt-1))
func (c *Client) calculateBackoff(attempt int) time.Duration {
	delay := time.Duration(float64(constants.InitialRetryDelay) * math.Pow(constants.RetryBackoffFactor, float64(attempt-1)))
	// Cap maximum delay to prevent excessive wait times
	if delay > constants.MaxRetryDelay {
		delay = constants.MaxRetryDelay
	}
	return delay
}

// HTTPError represents a Telegram API error response
type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("telegram API error (status %d): %s", e.StatusCode, e.Message)
}

// isClientError determines if error is a client error (4xx) that shouldn't be retried
func isClientError(err error) bool {
	if httpErr, ok := err.(*HTTPError); ok {
		return httpErr.StatusCode >= 400 && httpErr.StatusCode < 500
	}
	return false
}
