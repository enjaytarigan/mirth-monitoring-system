package config

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type Config struct {
	MirthURL       string
	MirthUsername  string
	MirthPassword  string
	TLSInsecure    bool
	PollIntervalMS int
	ListenAddr     string
	DataDir        string // optional; only used to migrate legacy connection.json once
	DatabaseURL    string

	NotifyProvider         string
	NotifyDestination      string
	NotifyTelegramBotToken string

	WhatsAppEnabled     bool
	WhatsAppBaseURL     string
	WhatsAppAPIKey      string
	WhatsAppInstance    string
	WhatsAppDestination string
}

// ConnectionFile is the legacy on-disk Settings format (migrated into Postgres once).
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
	mu sync.RWMutex
	db *sql.DB
	cfg Config
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

		// Optional bootstrap seed until Settings are saved in Postgres.
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

// OpenStore loads Settings from Postgres. If the settings row is empty and a legacy
// dataDir/connection.json exists, it is imported once then used as the source of truth.
func OpenStore(databaseURL string, base Config) (*Store, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(time.Hour)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	s := &Store{db: db, cfg: base}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	loaded, err := s.loadDB()
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if !loaded {
		if err := s.importLegacyJSON(filepath.Join(base.DataDir, "connection.json")); err != nil && !os.IsNotExist(err) {
			_ = db.Close()
			return nil, fmt.Errorf("migrate connection.json: %w", err)
		}
	}
	return s, nil
}

func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS app_settings (
  id SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  mirth_url TEXT NOT NULL DEFAULT '',
  mirth_username TEXT NOT NULL DEFAULT '',
  mirth_password TEXT NOT NULL DEFAULT '',
  tls_insecure BOOLEAN NOT NULL DEFAULT TRUE,
  notify_provider TEXT NOT NULL DEFAULT 'none',
  notify_telegram_bot_token TEXT NOT NULL DEFAULT '',
  notify_destination TEXT NOT NULL DEFAULT '',
  whatsapp_enabled BOOLEAN NOT NULL DEFAULT FALSE,
  whatsapp_base_url TEXT NOT NULL DEFAULT '',
  whatsapp_api_key TEXT NOT NULL DEFAULT '',
  whatsapp_instance TEXT NOT NULL DEFAULT '',
  whatsapp_destination TEXT NOT NULL DEFAULT '',
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`)
	return err
}

func (s *Store) loadDB() (bool, error) {
	var (
		url, user, pass, provider, tgToken, tgDest string
		waBase, waKey, waInst, waDest              string
		tlsInsecure, waEnabled                     bool
	)
	err := s.db.QueryRow(`
SELECT mirth_url, mirth_username, mirth_password, tls_insecure,
       notify_provider, notify_telegram_bot_token, notify_destination,
       whatsapp_enabled, whatsapp_base_url, whatsapp_api_key, whatsapp_instance, whatsapp_destination
FROM app_settings WHERE id=1`).Scan(
		&url, &user, &pass, &tlsInsecure,
		&provider, &tgToken, &tgDest,
		&waEnabled, &waBase, &waKey, &waInst, &waDest,
	)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	applySettingsLocked(&s.cfg, url, user, pass, tlsInsecure, provider, tgToken, tgDest, waEnabled, waBase, waKey, waInst, waDest)
	return true, nil
}

func applySettingsLocked(cfg *Config, url, user, pass string, tlsInsecure bool, provider, tgToken, tgDest string, waEnabled bool, waBase, waKey, waInst, waDest string) {
	if strings.TrimSpace(url) != "" {
		cfg.MirthURL = strings.TrimRight(strings.TrimSpace(url), "/")
	}
	if strings.TrimSpace(user) != "" {
		cfg.MirthUsername = strings.TrimSpace(user)
	}
	if pass != "" {
		cfg.MirthPassword = pass
	}
	cfg.TLSInsecure = tlsInsecure
	if provider != "" {
		cfg.NotifyProvider = strings.ToLower(strings.TrimSpace(provider))
	}
	if tgToken != "" {
		cfg.NotifyTelegramBotToken = strings.TrimSpace(tgToken)
	}
	cfg.NotifyDestination = strings.TrimSpace(tgDest)
	cfg.WhatsAppEnabled = waEnabled
	if strings.TrimSpace(waBase) != "" {
		cfg.WhatsAppBaseURL = strings.TrimRight(strings.TrimSpace(waBase), "/")
	}
	if waKey != "" {
		cfg.WhatsAppAPIKey = strings.TrimSpace(waKey)
	}
	if strings.TrimSpace(waInst) != "" {
		cfg.WhatsAppInstance = strings.TrimSpace(waInst)
	}
	cfg.WhatsAppDestination = strings.TrimSpace(waDest)
}

func (s *Store) importLegacyJSON(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var f ConnectionFile
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	applySettingsLocked(
		&s.cfg,
		f.URL, f.Username, f.Password, f.TLSInsecure,
		f.NotifyProvider, f.NotifyTelegramBotToken, f.NotifyDestination,
		f.WhatsAppEnabled, f.WhatsAppBaseURL, f.WhatsAppAPIKey, f.WhatsAppInstance, f.WhatsAppDestination,
	)
	if err := s.writeLocked(); err != nil {
		return err
	}
	// Keep a backup; stop using the live JSON path.
	_ = os.Rename(path, path+".migrated")
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

// Save writes Mirth connection and notify settings to Postgres.
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
	_, err := s.db.Exec(`
INSERT INTO app_settings (
  id, mirth_url, mirth_username, mirth_password, tls_insecure,
  notify_provider, notify_telegram_bot_token, notify_destination,
  whatsapp_enabled, whatsapp_base_url, whatsapp_api_key, whatsapp_instance, whatsapp_destination,
  updated_at
) VALUES (
  1, $1, $2, $3, $4,
  $5, $6, $7,
  $8, $9, $10, $11, $12,
  NOW()
)
ON CONFLICT (id) DO UPDATE SET
  mirth_url=EXCLUDED.mirth_url,
  mirth_username=EXCLUDED.mirth_username,
  mirth_password=EXCLUDED.mirth_password,
  tls_insecure=EXCLUDED.tls_insecure,
  notify_provider=EXCLUDED.notify_provider,
  notify_telegram_bot_token=EXCLUDED.notify_telegram_bot_token,
  notify_destination=EXCLUDED.notify_destination,
  whatsapp_enabled=EXCLUDED.whatsapp_enabled,
  whatsapp_base_url=EXCLUDED.whatsapp_base_url,
  whatsapp_api_key=EXCLUDED.whatsapp_api_key,
  whatsapp_instance=EXCLUDED.whatsapp_instance,
  whatsapp_destination=EXCLUDED.whatsapp_destination,
  updated_at=NOW()
`,
		s.cfg.MirthURL,
		s.cfg.MirthUsername,
		s.cfg.MirthPassword,
		s.cfg.TLSInsecure,
		s.cfg.NotifyProvider,
		s.cfg.NotifyTelegramBotToken,
		s.cfg.NotifyDestination,
		s.cfg.WhatsAppEnabled,
		s.cfg.WhatsAppBaseURL,
		s.cfg.WhatsAppAPIKey,
		s.cfg.WhatsAppInstance,
		s.cfg.WhatsAppDestination,
	)
	return err
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
