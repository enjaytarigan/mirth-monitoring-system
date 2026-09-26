package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type Config struct {
	MirthURL       string
	MirthUsername  string
	MirthPassword  string
	TLSInsecure    bool
	PollIntervalMS int
	ListenAddr     string
	DataDir        string
	DatabaseURL    string

	// Telegram (notifyProvider telegram|none for backward compatibility)
	NotifyProvider         string
	NotifyDestination      string
	NotifyTelegramBotToken string

	// WhatsApp via self-hosted HTTP gateway (Evolution-compatible sendText)
	WhatsAppEnabled     bool
	WhatsAppBaseURL     string
	WhatsAppAPIKey      string
	WhatsAppInstance    string
	WhatsAppDestination string
}

type ConnectionFile struct {
	URL                    string `json:"url"`
	Username               string `json:"username"`
	Password               string `json:"password"`
	TLSInsecure            bool   `json:"tlsInsecure"`
	NotifyProvider         string `json:"notifyProvider,omitempty"`
	NotifyTelegramBotToken string `json:"notifyTelegramBotToken,omitempty"`
	NotifyDestination      string `json:"notifyDestination,omitempty"`
	WhatsAppEnabled        bool   `json:"whatsappEnabled,omitempty"`
	WhatsAppBaseURL        string `json:"whatsappBaseUrl,omitempty"`
	WhatsAppAPIKey         string `json:"whatsappApiKey,omitempty"`
	WhatsAppInstance       string `json:"whatsappInstance,omitempty"`
	WhatsAppDestination    string `json:"whatsappDestination,omitempty"`
}

type Store struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

func Load() Config {
	return Config{
		MirthURL:       envOr("MIRTH_URL", "https://103.125.181.20:9443"),
		MirthUsername:  strings.TrimSpace(os.Getenv("MIRTH_USERNAME")),
		MirthPassword:  os.Getenv("MIRTH_PASSWORD"),
		TLSInsecure:    envBool("MIRTH_TLS_INSECURE", true),
		PollIntervalMS: envInt("POLL_INTERVAL_MS", 15000),
		ListenAddr:     envOr("LISTEN_ADDR", ":8080"),
		DataDir:        envOr("DATA_DIR", "data"),
		DatabaseURL:    envOr("DATABASE_URL", "postgres://mirth:mirth@localhost:5432/mirth_monitor?sslmode=disable"),

		// Optional bootstrap seed; Settings / connection.json owns values after save.
		NotifyProvider:         envOr("NOTIFY_PROVIDER", "none"),
		NotifyDestination:      strings.TrimSpace(os.Getenv("NOTIFY_DESTINATION")),
		NotifyTelegramBotToken: strings.TrimSpace(os.Getenv("NOTIFY_TELEGRAM_BOT_TOKEN")),
		WhatsAppEnabled:        envBool("WHATSAPP_ENABLED", false),
		WhatsAppBaseURL:        strings.TrimRight(strings.TrimSpace(os.Getenv("WHATSAPP_BASE_URL")), "/"),
		WhatsAppAPIKey:         strings.TrimSpace(os.Getenv("WHATSAPP_API_KEY")),
		WhatsAppInstance:       strings.TrimSpace(os.Getenv("WHATSAPP_INSTANCE")),
		WhatsAppDestination:    strings.TrimSpace(os.Getenv("WHATSAPP_DESTINATION")),
	}
}

func OpenStore(dataDir string, base Config) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		path: filepath.Join(dataDir, "connection.json"),
		cfg:  base,
	}
	if err := s.loadFile(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *Store) loadFile() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var f ConnectionFile
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(f.URL) != "" {
		s.cfg.MirthURL = strings.TrimRight(strings.TrimSpace(f.URL), "/")
	}
	if strings.TrimSpace(f.Username) != "" {
		s.cfg.MirthUsername = strings.TrimSpace(f.Username)
	}
	if f.Password != "" {
		s.cfg.MirthPassword = f.Password
	}
	s.cfg.TLSInsecure = f.TLSInsecure
	if f.NotifyProvider != "" {
		s.cfg.NotifyProvider = strings.ToLower(strings.TrimSpace(f.NotifyProvider))
	}
	if f.NotifyTelegramBotToken != "" {
		s.cfg.NotifyTelegramBotToken = strings.TrimSpace(f.NotifyTelegramBotToken)
	}
	if f.NotifyDestination != "" {
		s.cfg.NotifyDestination = strings.TrimSpace(f.NotifyDestination)
	}
	s.cfg.WhatsAppEnabled = f.WhatsAppEnabled
	if strings.TrimSpace(f.WhatsAppBaseURL) != "" {
		s.cfg.WhatsAppBaseURL = strings.TrimRight(strings.TrimSpace(f.WhatsAppBaseURL), "/")
	}
	if f.WhatsAppAPIKey != "" {
		s.cfg.WhatsAppAPIKey = strings.TrimSpace(f.WhatsAppAPIKey)
	}
	if strings.TrimSpace(f.WhatsAppInstance) != "" {
		s.cfg.WhatsAppInstance = strings.TrimSpace(f.WhatsAppInstance)
	}
	if strings.TrimSpace(f.WhatsAppDestination) != "" {
		s.cfg.WhatsAppDestination = strings.TrimSpace(f.WhatsAppDestination)
	}
	return nil
}

func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Store) Configured() bool {
	c := s.Get()
	return strings.TrimSpace(c.MirthURL) != "" &&
		strings.TrimSpace(c.MirthUsername) != "" &&
		c.MirthPassword != ""
}

func (c Config) TelegramEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(c.NotifyProvider), "telegram")
}

type SaveInput struct {
	URL                    string
	Username               string
	Password               string
	TLSInsecure            bool
	NotifyEnabled          bool
	NotifyTelegramBotToken string
	NotifyDestination      string
	WhatsAppEnabled        bool
	WhatsAppBaseURL        string
	WhatsAppAPIKey         string
	WhatsAppInstance       string
	WhatsAppDestination    string
}

// Save writes Mirth connection and notify settings together.
// Blank password / bot token / WhatsApp API key keep previously saved values.
func (s *Store) Save(in SaveInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.MirthURL = strings.TrimRight(strings.TrimSpace(in.URL), "/")
	s.cfg.MirthUsername = strings.TrimSpace(in.Username)
	if in.Password != "" {
		s.cfg.MirthPassword = in.Password
	}
	s.cfg.TLSInsecure = in.TLSInsecure

	if in.NotifyEnabled {
		s.cfg.NotifyProvider = "telegram"
	} else {
		s.cfg.NotifyProvider = "none"
	}
	if strings.TrimSpace(in.NotifyTelegramBotToken) != "" {
		s.cfg.NotifyTelegramBotToken = strings.TrimSpace(in.NotifyTelegramBotToken)
	}
	s.cfg.NotifyDestination = strings.TrimSpace(in.NotifyDestination)

	s.cfg.WhatsAppEnabled = in.WhatsAppEnabled
	s.cfg.WhatsAppBaseURL = strings.TrimRight(strings.TrimSpace(in.WhatsAppBaseURL), "/")
	if strings.TrimSpace(in.WhatsAppAPIKey) != "" {
		s.cfg.WhatsAppAPIKey = strings.TrimSpace(in.WhatsAppAPIKey)
	}
	s.cfg.WhatsAppInstance = strings.TrimSpace(in.WhatsAppInstance)
	s.cfg.WhatsAppDestination = strings.TrimSpace(in.WhatsAppDestination)

	return s.writeLocked()
}

// SaveConnection keeps the older Mirth-only API; notify fields are preserved.
func (s *Store) SaveConnection(url, username, password string, tlsInsecure bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.MirthURL = strings.TrimRight(strings.TrimSpace(url), "/")
	s.cfg.MirthUsername = strings.TrimSpace(username)
	if password != "" {
		s.cfg.MirthPassword = password
	}
	s.cfg.TLSInsecure = tlsInsecure
	return s.writeLocked()
}

func (s *Store) writeLocked() error {
	f := ConnectionFile{
		URL:                    s.cfg.MirthURL,
		Username:               s.cfg.MirthUsername,
		Password:               s.cfg.MirthPassword,
		TLSInsecure:            s.cfg.TLSInsecure,
		NotifyProvider:         s.cfg.NotifyProvider,
		NotifyTelegramBotToken: s.cfg.NotifyTelegramBotToken,
		NotifyDestination:      s.cfg.NotifyDestination,
		WhatsAppEnabled:        s.cfg.WhatsAppEnabled,
		WhatsAppBaseURL:        s.cfg.WhatsAppBaseURL,
		WhatsAppAPIKey:         s.cfg.WhatsAppAPIKey,
		WhatsAppInstance:       s.cfg.WhatsAppInstance,
		WhatsAppDestination:    s.cfg.WhatsAppDestination,
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o600)
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envBool(key string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}
