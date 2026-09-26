package main

import (
	"io/fs"
	"log"
	"os"

	"github.com/enjaytarigan/mirth_monitoring_system/internal/alarms"
	"github.com/enjaytarigan/mirth_monitoring_system/internal/config"
	"github.com/enjaytarigan/mirth_monitoring_system/internal/mirth"
	"github.com/enjaytarigan/mirth_monitoring_system/internal/notify"
	"github.com/enjaytarigan/mirth_monitoring_system/internal/server"
	"github.com/enjaytarigan/mirth_monitoring_system/web"
)

func main() {
	loadDotEnv(".env")
	base := config.Load()

	cfgStore, err := config.OpenStore(base.DataDir, base)
	if err != nil {
		log.Fatalf("config store: %v", err)
	}

	client := mirth.NewClient()
	cfg := cfgStore.Get()
	if cfgStore.Configured() {
		if err := client.Configure(cfg.MirthURL, cfg.MirthUsername, cfg.MirthPassword, cfg.TLSInsecure); err != nil {
			log.Fatalf("mirth client: %v", err)
		}
		if err := client.Ping(); err != nil {
			log.Printf("warning: cannot reach Mirth at %s: %v (update Settings after start)", cfg.MirthURL, err)
		} else {
			log.Printf("connected to Mirth at %s as %s", cfg.MirthURL, cfg.MirthUsername)
		}
	} else {
		log.Printf("Mirth credentials not set — open http://localhost%s/settings", cfg.ListenAddr)
	}

	store, err := alarms.Open(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("alarms store: %v", err)
	}
	defer store.Close()
	log.Printf("connected to PostgreSQL")

	tmplFS, err := fs.Sub(web.FS, ".")
	if err != nil {
		log.Fatalf("templates fs: %v", err)
	}
	staticFS, err := fs.Sub(web.FS, "static")
	if err != nil {
		log.Fatalf("static fs: %v", err)
	}

	notifier := notify.NewHolder(notify.Config{
		Provider:            cfg.NotifyProvider,
		TelegramEnabled:     cfg.TelegramEnabled(),
		TelegramBotToken:    cfg.NotifyTelegramBotToken,
		TelegramChatID:      cfg.NotifyDestination,
		WhatsAppEnabled:     cfg.WhatsAppEnabled,
		WhatsAppBaseURL:     cfg.WhatsAppBaseURL,
		WhatsAppAPIKey:      cfg.WhatsAppAPIKey,
		WhatsAppInstance:    cfg.WhatsAppInstance,
		WhatsAppDestination: cfg.WhatsAppDestination,
	})
	if notifier.Enabled() {
		log.Printf("notifications enabled")
	} else {
		log.Printf("notifications disabled — configure Telegram and/or WhatsApp in Settings")
	}

	app, err := server.New(cfgStore, client, store, notifier, tmplFS, staticFS)
	if err != nil {
		log.Fatalf("server: %v", err)
	}

	poller := alarms.NewPoller(client, store, notifier, cfg.PollIntervalMS)
	poller.Start()
	defer poller.Stop()

	if err := server.ListenAndServe(cfg.ListenAddr, app.Handler()); err != nil {
		log.Fatal(err)
	}
}

func loadDotEnv(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range splitLines(string(b)) {
		line = trimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		i := indexByte(line, '=')
		if i <= 0 {
			continue
		}
		key := trimSpace(line[:i])
		val := trimSpace(line[i+1:])
		// Strip unquoted inline comments ("value  # comment"); keep values like pass#word.
		if len(val) == 0 || (val[0] != '"' && val[0] != '\'') {
			if j := indexSpaceHash(val); j >= 0 {
				val = trimSpace(val[:j])
			}
		}
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
}

func splitLines(s string) []string {
	out := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// indexSpaceHash finds " #" / "\t#" used as an inline comment start.
func indexSpaceHash(s string) int {
	for i := 0; i+1 < len(s); i++ {
		if (s[i] == ' ' || s[i] == '\t') && s[i+1] == '#' {
			return i
		}
	}
	return -1
}
