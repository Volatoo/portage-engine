// Package notification provides build completion notification capabilities.
package notification

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"time"
)

// BuildNotification represents a build completion notification.
type BuildNotification struct {
	JobID       string    `json:"job_id"`
	PackageName string    `json:"package_name"`
	Version     string    `json:"version"`
	Status      string    `json:"status"` // success, failed
	StartTime   time.Time `json:"start_time"`
	EndTime     time.Time `json:"end_time"`
	Duration    string    `json:"duration"`
	BuildLog    string    `json:"build_log,omitempty"`
	Error       string    `json:"error,omitempty"`
	ArtifactURL string    `json:"artifact_url,omitempty"`
}

// Config represents notification configuration.
type Config struct {
	Email    *EmailConfig    `json:"email,omitempty"`
	Webhook  *WebhookConfig  `json:"webhook,omitempty"`
	IRC      *IRCConfig      `json:"irc,omitempty"`
	Slack    *SlackConfig    `json:"slack,omitempty"`
	Telegram *TelegramConfig `json:"telegram,omitempty"`
}

// EmailConfig represents email notification configuration.
type EmailConfig struct {
	Enabled  bool     `json:"enabled"`
	SMTPHost string   `json:"smtp_host"`
	SMTPPort int      `json:"smtp_port"`
	Username string   `json:"username"`
	Password string   `json:"password"`
	From     string   `json:"from"`
	To       []string `json:"to"`
	Subject  string   `json:"subject"`
}

// WebhookConfig represents webhook notification configuration.
type WebhookConfig struct {
	Enabled bool              `json:"enabled"`
	URL     string            `json:"url"`
	Method  string            `json:"method"` // POST, PUT
	Headers map[string]string `json:"headers,omitempty"`
	Timeout int               `json:"timeout"` // seconds
}

// IRCConfig represents IRC notification configuration.
type IRCConfig struct {
	Enabled  bool     `json:"enabled"`
	Server   string   `json:"server"`
	Port     int      `json:"port"`
	Nick     string   `json:"nick"`
	Channels []string `json:"channels"`
	UseTLS   bool     `json:"use_tls"`
}

// SlackConfig represents Slack notification configuration.
type SlackConfig struct {
	Enabled    bool   `json:"enabled"`
	WebhookURL string `json:"webhook_url"`
	Channel    string `json:"channel,omitempty"`
	Username   string `json:"username,omitempty"`
	IconEmoji  string `json:"icon_emoji,omitempty"`
}

// TelegramConfig represents Telegram notification configuration.
type TelegramConfig struct {
	Enabled  bool   `json:"enabled"`
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
}

// Notifier handles sending notifications.
type Notifier struct {
	config *Config
	client *http.Client
}

// NewNotifier creates a new notification handler.
func NewNotifier(config *Config) *Notifier {
	return &Notifier{
		config: config,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// LoadConfig loads notification configuration from a file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is an operator-selected notification config file.
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	return &config, nil
}

// Notify sends notification through all enabled channels.
func (n *Notifier) Notify(notification *BuildNotification) error {
	var errors []string

	if err := n.notifyEmail(notification); err != nil {
		errors = append(errors, fmt.Sprintf("email: %v", err))
	}

	if err := n.notifyWebhook(notification); err != nil {
		errors = append(errors, fmt.Sprintf("webhook: %v", err))
	}

	n.notifyIRC(notification)

	if err := n.notifySlack(notification); err != nil {
		errors = append(errors, fmt.Sprintf("slack: %v", err))
	}

	if err := n.notifyTelegram(notification); err != nil {
		errors = append(errors, fmt.Sprintf("telegram: %v", err))
	}

	if len(errors) > 0 {
		return fmt.Errorf("notification errors: %s", strings.Join(errors, "; "))
	}

	return nil
}

// notifyEmail sends email notification if enabled.
func (n *Notifier) notifyEmail(notification *BuildNotification) error {
	if n.config.Email == nil || !n.config.Email.Enabled {
		return nil
	}
	return n.sendEmail(notification)
}

// notifyWebhook sends webhook notification if enabled.
func (n *Notifier) notifyWebhook(notification *BuildNotification) error {
	if n.config.Webhook == nil || !n.config.Webhook.Enabled {
		return nil
	}
	return n.sendWebhook(notification)
}

// notifyIRC sends IRC notification if enabled.
func (n *Notifier) notifyIRC(notification *BuildNotification) {
	if n.config.IRC != nil && n.config.IRC.Enabled {
		n.sendIRC(notification)
	}
}

// notifySlack sends Slack notification if enabled.
func (n *Notifier) notifySlack(notification *BuildNotification) error {
	if n.config.Slack == nil || !n.config.Slack.Enabled {
		return nil
	}
	return n.sendSlack(notification)
}

// notifyTelegram sends Telegram notification if enabled.
func (n *Notifier) notifyTelegram(notification *BuildNotification) error {
	if n.config.Telegram == nil || !n.config.Telegram.Enabled {
		return nil
	}
	return n.sendTelegram(notification)
}

// sendEmail sends email notification.
func (n *Notifier) sendEmail(notification *BuildNotification) error {
	cfg := n.config.Email

	subject := cfg.Subject
	if subject == "" {
		subject = fmt.Sprintf("Build %s: %s-%s", notification.Status, notification.PackageName, notification.Version)
	}

	body := n.formatEmailBody(notification)

	// Sanitize every value that goes into a header line so a package name /
	// version / subject containing CR/LF cannot inject extra headers.
	to := make([]string, len(cfg.To))
	for i, addr := range cfg.To {
		to[i] = sanitizeHeader(addr)
	}
	msg := fmt.Sprintf("From: %s\r\n", sanitizeHeader(cfg.From))
	msg += fmt.Sprintf("To: %s\r\n", strings.Join(to, ","))
	msg += fmt.Sprintf("Subject: %s\r\n", sanitizeHeader(subject))
	msg += "Content-Type: text/plain; charset=UTF-8\r\n"
	msg += "\r\n"
	msg += body

	auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.SMTPHost)
	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort)

	// smtp.SendMail has no timeout, so a hung SMTP server would stall the build
	// worker forever. Bound it: run the send in a goroutine and abandon it after
	// emailSendTimeout.
	done := make(chan error, 1)
	go func() { done <- smtp.SendMail(addr, auth, cfg.From, cfg.To, []byte(msg)) }()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("failed to send email: %w", err)
		}
	case <-time.After(emailSendTimeout):
		return fmt.Errorf("email send timed out after %s", emailSendTimeout)
	}

	log.Printf("Email notification sent to %v", cfg.To)
	return nil
}

// emailSendTimeout bounds a single SMTP send so a hung server cannot stall a
// build worker.
const emailSendTimeout = 30 * time.Second

// formatEmailBody formats the email body.
func (n *Notifier) formatEmailBody(notification *BuildNotification) string {
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "Build Status: %s\n", strings.ToUpper(notification.Status))
	fmt.Fprintf(&buf, "Job ID: %s\n", notification.JobID)
	fmt.Fprintf(&buf, "Package: %s-%s\n", notification.PackageName, notification.Version)
	fmt.Fprintf(&buf, "Duration: %s\n", notification.Duration)
	fmt.Fprintf(&buf, "Started: %s\n", notification.StartTime.Format(time.RFC3339))
	fmt.Fprintf(&buf, "Finished: %s\n", notification.EndTime.Format(time.RFC3339))

	if notification.ArtifactURL != "" {
		fmt.Fprintf(&buf, "\nArtifact: %s\n", notification.ArtifactURL)
	}

	if notification.Error != "" {
		fmt.Fprintf(&buf, "\nError: %s\n", notification.Error)
	}

	if notification.BuildLog != "" && len(notification.BuildLog) < 500 {
		fmt.Fprintf(&buf, "\nBuild Log:\n%s\n", notification.BuildLog)
	}

	return buf.String()
}

// sendWebhook sends webhook notification.
func (n *Notifier) sendWebhook(notification *BuildNotification) error {
	cfg := n.config.Webhook

	payload, err := json.Marshal(notification)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	method := cfg.Method
	if method == "" {
		method = "POST"
	}

	req, err := http.NewRequest(method, cfg.URL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	for key, value := range cfg.Headers {
		req.Header.Set(key, value)
	}

	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("webhook returned status %d: %s", resp.StatusCode, string(body))
	}

	log.Printf("Webhook notification sent to %s", cfg.URL)
	return nil
}

// sendIRC sends IRC notification.
func (n *Notifier) sendIRC(notification *BuildNotification) {
	// IRC notification is implemented as a placeholder
	// In production, this would use an IRC library like github.com/thoj/go-ircevent
	cfg := n.config.IRC

	message := fmt.Sprintf("[Build %s] %s-%s (Job: %s) - Duration: %s",
		notification.Status,
		notification.PackageName,
		notification.Version,
		notification.JobID,
		notification.Duration,
	)

	log.Printf("IRC notification would be sent to %s:%d channels=%v: %s",
		cfg.Server, cfg.Port, cfg.Channels, message)

	// Actual IRC implementation would go here
}

// sendSlack sends Slack notification.
func (n *Notifier) sendSlack(notification *BuildNotification) error {
	cfg := n.config.Slack

	color := "good"
	if notification.Status == "failed" {
		color = "danger"
	}

	payload := map[string]interface{}{
		"username":   cfg.Username,
		"icon_emoji": cfg.IconEmoji,
		"channel":    cfg.Channel,
		"attachments": []map[string]interface{}{
			{
				"color": color,
				"title": fmt.Sprintf("Build %s: %s-%s", strings.ToUpper(notification.Status), notification.PackageName, notification.Version),
				"fields": []map[string]interface{}{
					{
						"title": "Job ID",
						"value": notification.JobID,
						"short": true,
					},
					{
						"title": "Duration",
						"value": notification.Duration,
						"short": true,
					},
					{
						"title": "Package",
						"value": fmt.Sprintf("%s-%s", notification.PackageName, notification.Version),
						"short": false,
					},
				},
				"footer": "Portage Engine",
				"ts":     notification.EndTime.Unix(),
			},
		},
	}

	if notification.ArtifactURL != "" {
		payload["attachments"].([]map[string]interface{})[0]["fields"] = append(
			payload["attachments"].([]map[string]interface{})[0]["fields"].([]map[string]interface{}),
			map[string]interface{}{
				"title": "Artifact",
				"value": notification.ArtifactURL,
				"short": false,
			},
		)
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal slack payload: %w", err)
	}

	resp, err := n.client.Post(cfg.WebhookURL, "application/json", bytes.NewReader(jsonPayload))
	if err != nil {
		return fmt.Errorf("failed to send slack notification: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("slack returned status %d: %s", resp.StatusCode, string(body))
	}

	log.Printf("Slack notification sent")
	return nil
}

// sendTelegram sends Telegram notification.
func (n *Notifier) sendTelegram(notification *BuildNotification) error {
	cfg := n.config.Telegram

	status := "✅ SUCCESS"
	if notification.Status == "failed" {
		status = "❌ FAILED"
	}

	text := fmt.Sprintf(
		"*Build %s*\n\n"+
			"*Package:* %s-%s\n"+
			"*Job ID:* `%s`\n"+
			"*Duration:* %s\n"+
			"*Finished:* %s",
		status,
		notification.PackageName,
		notification.Version,
		notification.JobID,
		notification.Duration,
		notification.EndTime.Format("2006-01-02 15:04:05"),
	)

	if notification.ArtifactURL != "" {
		text += fmt.Sprintf("\n*Artifact:* %s", notification.ArtifactURL)
	}

	if notification.Error != "" {
		text += fmt.Sprintf("\n*Error:* %s", notification.Error)
	}

	payload := map[string]interface{}{
		"chat_id":    cfg.ChatID,
		"text":       text,
		"parse_mode": "Markdown",
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal telegram payload: %w", err)
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", cfg.BotToken)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(jsonPayload))
	if err != nil {
		return fmt.Errorf("failed to build telegram request: %w", redactToken(err, cfg.BotToken))
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		// The transport error (*url.Error) embeds the full request URL, which
		// contains the bot token — redact it before returning/logging.
		return fmt.Errorf("failed to send telegram notification: %w", redactToken(err, cfg.BotToken))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Report status only; the response body could echo the token.
		return fmt.Errorf("telegram returned status %d", resp.StatusCode)
	}

	log.Printf("Telegram notification sent to chat %s", cfg.ChatID)
	return nil
}

// sanitizeHeader strips CR and LF from a value destined for an email header,
// preventing header injection via user-controlled package name / version /
// subject.
func sanitizeHeader(v string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(v)
}

// redactToken returns an error whose message has the bot token replaced with
// "REDACTED", so a token embedded in a wrapped transport error never leaks.
func redactToken(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), token, "REDACTED"))
}
