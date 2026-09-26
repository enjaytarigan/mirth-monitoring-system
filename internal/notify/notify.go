package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type AlarmEvent struct {
	ChannelID     string
	ChannelName   string
	MessageID     int64
	ConnectorName string
	ErrorText     string
	ReceivedDate  string
	MetaData      map[string]string
}

type Config struct {
	// Legacy single-provider field (telegram|none); still honored for Telegram enablement.
	Provider string

	TelegramEnabled  bool
	TelegramBotToken string
	TelegramChatID   string

	WhatsAppEnabled     bool
	WhatsAppBaseURL     string
	WhatsAppAPIKey      string
	WhatsAppInstance    string
	WhatsAppDestination string
}

type Notifier interface {
	Enabled() bool
	Notify(ev AlarmEvent) error
}

func New(cfg Config) Notifier {
	var children []Notifier

	telegramOn := cfg.TelegramEnabled || strings.EqualFold(strings.TrimSpace(cfg.Provider), "telegram")
	if telegramOn {
		token := strings.TrimSpace(cfg.TelegramBotToken)
		chatID := strings.TrimSpace(cfg.TelegramChatID)
		if token == "" || chatID == "" {
			log.Printf("notify: telegram enabled but bot token or chat id empty — skipped")
		} else {
			children = append(children, &telegramNotifier{
				token:  token,
				chatID: chatID,
				client: &http.Client{Timeout: 20 * time.Second},
			})
		}
	}

	if cfg.WhatsAppEnabled {
		base := strings.TrimRight(strings.TrimSpace(cfg.WhatsAppBaseURL), "/")
		key := strings.TrimSpace(cfg.WhatsAppAPIKey)
		instance := strings.TrimSpace(cfg.WhatsAppInstance)
		dest := normalizeWhatsAppDestination(cfg.WhatsAppDestination)
		if base == "" || key == "" || instance == "" || dest == "" {
			log.Printf("notify: whatsapp enabled but base URL, API key, instance, or destination empty — skipped")
		} else {
			children = append(children, &evolutionNotifier{
				baseURL:  base,
				apiKey:   key,
				instance: instance,
				number:   dest,
				client:   &http.Client{Timeout: 20 * time.Second},
			})
		}
	}

	switch len(children) {
	case 0:
		return noop{}
	case 1:
		return children[0]
	default:
		return multiNotifier{children: children}
	}
}

// Holder is a thread-safe Notifier that can be replaced when Settings change.
type Holder struct {
	mu sync.RWMutex
	n  Notifier
}

func NewHolder(cfg Config) *Holder {
	return &Holder{n: New(cfg)}
}

func (h *Holder) Enabled() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.n == nil {
		return false
	}
	return h.n.Enabled()
}

func (h *Holder) Notify(ev AlarmEvent) error {
	h.mu.RLock()
	n := h.n
	h.mu.RUnlock()
	if n == nil {
		return nil
	}
	return n.Notify(ev)
}

func (h *Holder) Replace(cfg Config) {
	n := New(cfg)
	h.mu.Lock()
	h.n = n
	h.mu.Unlock()
	log.Printf("notifications: %s", describeChannels(cfg))
}

func describeChannels(cfg Config) string {
	var parts []string
	telegramOn := cfg.TelegramEnabled || strings.EqualFold(strings.TrimSpace(cfg.Provider), "telegram")
	if telegramOn && strings.TrimSpace(cfg.TelegramBotToken) != "" && strings.TrimSpace(cfg.TelegramChatID) != "" {
		parts = append(parts, "telegram chat_id="+strings.TrimSpace(cfg.TelegramChatID))
	}
	if cfg.WhatsAppEnabled {
		dest := normalizeWhatsAppDestination(cfg.WhatsAppDestination)
		inst := strings.TrimSpace(cfg.WhatsAppInstance)
		if strings.TrimSpace(cfg.WhatsAppBaseURL) != "" && strings.TrimSpace(cfg.WhatsAppAPIKey) != "" && inst != "" && dest != "" {
			parts = append(parts, "whatsapp instance="+inst+" to="+dest)
		}
	}
	if len(parts) == 0 {
		return "disabled"
	}
	return "enabled via " + strings.Join(parts, ", ")
}

type noop struct{}

func (noop) Enabled() bool           { return false }
func (noop) Notify(AlarmEvent) error { return nil }

type multiNotifier struct {
	children []Notifier
}

func (m multiNotifier) Enabled() bool {
	for _, c := range m.children {
		if c.Enabled() {
			return true
		}
	}
	return false
}

func (m multiNotifier) Notify(ev AlarmEvent) error {
	var errs []string
	for _, c := range m.children {
		if !c.Enabled() {
			continue
		}
		if err := c.Notify(ev); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(errs, "; "))
}

func FormatMessage(ev AlarmEvent) string {
	var b strings.Builder
	b.WriteString("*Mirth ERROR*\n")
	b.WriteString(fmt.Sprintf("Channel: `%s`\n", escapeMarkdown(ev.ChannelName)))
	b.WriteString(fmt.Sprintf("Channel ID: `%s`\n", escapeMarkdown(ev.ChannelID)))
	b.WriteString(fmt.Sprintf("Message ID: `%d`\n", ev.MessageID))
	b.WriteString(fmt.Sprintf("Connector: `%s`\n", escapeMarkdown(ev.ConnectorName)))
	if rd := strings.TrimSpace(ev.ReceivedDate); rd != "" {
		b.WriteString(fmt.Sprintf("Received: `%s`\n", escapeMarkdown(rd)))
	}
	if metaLines := formatMetaLines(ev.MetaData); len(metaLines) > 0 {
		b.WriteString("\n*Metadata*\n")
		for _, line := range metaLines {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	if errText := previewError(ev.ErrorText, 600); errText != "" {
		b.WriteString("\n*Error preview*\n```\n")
		b.WriteString(errText)
		b.WriteString("\n```")
	}
	return b.String()
}

func formatMetaLines(meta map[string]string) []string {
	if len(meta) == 0 {
		return nil
	}
	keys := make([]string, 0, len(meta))
	for k, v := range meta {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	const maxKeys = 20
	if len(keys) > maxKeys {
		keys = keys[:maxKeys]
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		v := truncateRunes(strings.TrimSpace(meta[k]), 120)
		out = append(out, fmt.Sprintf("`%s`: `%s`", escapeMarkdown(k), escapeMarkdown(v)))
	}
	return out
}

func previewError(s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\t", " ")
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return truncateRunes(s, max)
}

func truncateRunes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max > 1 {
		return s[:max-1] + "…"
	}
	return s[:max]
}

func escapeMarkdown(s string) string {
	replacer := strings.NewReplacer(
		"_", "\\_",
		"*", "\\*",
		"`", "\\`",
		"[", "\\[",
	)
	return replacer.Replace(s)
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// normalizeWhatsAppDestination accepts a phone (country code digits) or a group JID (...@g.us).
func normalizeWhatsAppDestination(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lower := strings.ToLower(s)
	if strings.Contains(lower, "@g.us") {
		// Keep group JID; strip spaces and accidental whatsapp URL wrappers.
		s = strings.ReplaceAll(s, " ", "")
		if i := strings.Index(strings.ToLower(s), "@g.us"); i >= 0 {
			return s[:i] + "@g.us"
		}
		return s
	}
	if strings.HasSuffix(lower, "@s.whatsapp.net") {
		return digitsOnly(strings.Split(s, "@")[0])
	}
	return digitsOnly(s)
}

type telegramNotifier struct {
	token  string
	chatID string
	client *http.Client
}

func (n *telegramNotifier) Enabled() bool { return true }

func (n *telegramNotifier) Notify(ev AlarmEvent) error {
	api := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.token)
	form := url.Values{}
	form.Set("chat_id", n.chatID)
	form.Set("text", FormatMessage(ev))
	form.Set("parse_mode", "Markdown")
	form.Set("disable_web_page_preview", "true")

	req, err := http.NewRequest(http.MethodPost, api, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode >= 300 {
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		if desc, _ := parsed["description"].(string); strings.Contains(strings.ToLower(desc), "parse") {
			return n.sendPlain(ev)
		}
		return fmt.Errorf("telegram HTTP %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (n *telegramNotifier) sendPlain(ev AlarmEvent) error {
	api := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.token)
	payload := map[string]any{
		"chat_id":                  n.chatID,
		"text":                     plainMessage(ev),
		"disable_web_page_preview": true,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, api, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("telegram HTTP %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	return nil
}

type evolutionNotifier struct {
	baseURL  string
	apiKey   string
	instance string
	number   string
	client   *http.Client
}

func (n *evolutionNotifier) Enabled() bool { return true }

func (n *evolutionNotifier) Notify(ev AlarmEvent) error {
	api := fmt.Sprintf("%s/message/sendText/%s", n.baseURL, url.PathEscape(n.instance))
	payload := map[string]any{
		"number": n.number,
		"text":   plainMessage(ev),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, api, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", n.apiKey)
	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("whatsapp: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("whatsapp HTTP %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	return nil
}

func plainMessage(ev AlarmEvent) string {
	var b strings.Builder
	b.WriteString("Mirth ERROR\n")
	b.WriteString(fmt.Sprintf("Channel: %s\n", ev.ChannelName))
	b.WriteString(fmt.Sprintf("Channel ID: %s\n", ev.ChannelID))
	b.WriteString(fmt.Sprintf("Message ID: %d\n", ev.MessageID))
	b.WriteString(fmt.Sprintf("Connector: %s\n", ev.ConnectorName))
	if rd := strings.TrimSpace(ev.ReceivedDate); rd != "" {
		b.WriteString(fmt.Sprintf("Received: %s\n", rd))
	}
	if len(ev.MetaData) > 0 {
		b.WriteString("\nMetadata\n")
		keys := make([]string, 0, len(ev.MetaData))
		for k, v := range ev.MetaData {
			if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for i, k := range keys {
			if i >= 20 {
				break
			}
			b.WriteString(fmt.Sprintf("%s: %s\n", k, truncateRunes(strings.TrimSpace(ev.MetaData[k]), 120)))
		}
	}
	if errText := previewError(ev.ErrorText, 600); errText != "" {
		b.WriteString("\nError preview\n")
		b.WriteString(errText)
	}
	return b.String()
}
