package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/enjaytarigan/mirth_monitoring_system/internal/alarms"
	"github.com/enjaytarigan/mirth_monitoring_system/internal/config"
	"github.com/enjaytarigan/mirth_monitoring_system/internal/mirth"
	"github.com/enjaytarigan/mirth_monitoring_system/internal/notify"
)

type App struct {
	cfgStore *config.Store
	client   *mirth.Client
	store    *alarms.Store
	notifier *notify.Holder
	tmpl     *template.Template
	staticFS fs.FS
}

type pageData struct {
	Title           string
	Active          string
	Mode            mirth.ConnectionMode
	ModeLabel       string
	Unacked         int
	PollMS          int
	Now             time.Time
	Error           string
	Success         string
	Configured      bool
	Dashboard       *mirth.DashboardSummary
	Channels        []mirth.ChannelInfo
	Columns         []mirth.MetaDataColumn
	Results         []mirth.MessageSummary
	Detail          *mirth.MessageDetail
	Alarms          []alarms.Alarm
	Form            searchForm
	Settings        settingsForm
	WatchChannels   []watchChannelOption
	WatchConfigured bool
	Pager           *pagerData
}

type pagerData struct {
	Page       int
	PageSize   int
	Total      int
	TotalPages int
	Offset     int
	HasPrev    bool
	HasNext    bool
	PrevPage   int
	NextPage   int
	Query      template.URL
}

type watchChannelOption struct {
	ID      string
	Name    string
	Watched bool
}

type searchForm struct {
	ChannelID  string
	Column     string
	Operator   string
	Value      string
	Status     string
	StartDate  string
	EndDate    string
	IgnoreCase bool
}

type settingsForm struct {
	URL                   string
	Username              string
	PasswordSet           bool
	TLSInsecure           bool
	NotifyEnabled         bool
	NotifyDestination     string
	NotifyTokenSet        bool
	WhatsAppEnabled       bool
	WhatsAppBaseURL       string
	WhatsAppAPIKeySet     bool
	WhatsAppInstance      string
	WhatsAppDestination   string
}

func New(cfgStore *config.Store, client *mirth.Client, store *alarms.Store, notifier *notify.Holder, tmplFS fs.FS, staticFS fs.FS) (*App, error) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"lower": strings.ToLower,
		"formatTime": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Local().Format("2006-01-02 15:04:05")
		},
		"formatPtrTime": func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return t.Local().Format("2006-01-02 15:04:05")
		},
		"nonzero": func(n int64) bool { return n != 0 },
		"stateClass": func(state string) string {
			switch strings.ToLower(state) {
			case "started":
				return "state-ok"
			case "paused":
				return "state-warn"
			case "stopped", "undeployed":
				return "state-muted"
			default:
				return "state-warn"
			}
		},
		"statusClass": func(status string) string {
			switch strings.ToUpper(status) {
			case "ERROR":
				return "status-error"
			case "QUEUED", "PENDING":
				return "status-warn"
			case "SENT", "TRANSFORMED":
				return "status-ok"
			case "FILTERED":
				return "status-muted"
			default:
				return ""
			}
		},
		"operators": func() []string {
			return []string{"=", "!=", "CONTAINS", "STARTS WITH", "ENDS WITH", "DOES NOT CONTAIN", "<", "<=", ">", ">="}
		},
		"statuses": func() []string {
			return []string{"", "ERROR", "SENT", "QUEUED", "FILTERED", "RECEIVED", "TRANSFORMED", "PENDING"}
		},
		"watchedCount": func(opts []watchChannelOption) int {
			n := 0
			for _, o := range opts {
				if o.Watched {
					n++
				}
			}
			return n
		},
		"add": func(a, b int) int { return a + b },
		"min": func(a, b int) int {
			if a < b {
				return a
			}
			return b
		},
	}).ParseFS(tmplFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &App{cfgStore: cfgStore, client: client, store: store, notifier: notifier, tmpl: tmpl, staticFS: staticFS}, nil
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(a.staticFS))))
	mux.HandleFunc("GET /", a.handleDashboard)
	mux.HandleFunc("GET /search", a.handleSearch)
	mux.HandleFunc("GET /messages/{channelId}/{messageId}", a.handleMessageDetail)
	mux.HandleFunc("GET /alarms", a.handleAlarms)
	mux.HandleFunc("POST /alarms/watch", a.handleAlarmsWatch)
	mux.HandleFunc("GET /settings", a.handleSettingsGet)
	mux.HandleFunc("POST /settings", a.handleSettingsPost)
	mux.HandleFunc("GET /api/dashboard", a.handleAPIDashboard)
	mux.HandleFunc("GET /api/channels/{channelId}/metadata", a.handleAPIMetadata)
	mux.HandleFunc("GET /alarms/feed", a.handleAlarmsFeed)
	mux.HandleFunc("POST /alarms/ack", a.handleAlarmsAck)
	return mux
}

func (a *App) connectionMode() mirth.ConnectionMode {
	if !a.client.Configured() {
		return mirth.ModeUnreachable
	}
	mode, err := a.client.ServerStatus()
	if err != nil {
		return mirth.ModeUnreachable
	}
	return mode
}

func (a *App) modeLabel(mode mirth.ConnectionMode) string {
	if !a.cfgStore.Configured() {
		return "Not configured"
	}
	switch mode {
	case mirth.ModeConnected:
		return "Connected"
	default:
		return "Unreachable"
	}
}

func (a *App) basePage(active, title string) pageData {
	cfg := a.cfgStore.Get()
	mode := a.connectionMode()
	unacked, _ := a.store.UnackedCount()
	return pageData{
		Title:      title,
		Active:     active,
		Mode:       mode,
		ModeLabel:  a.modeLabel(mode),
		Unacked:    unacked,
		PollMS:     cfg.PollIntervalMS,
		Now:        time.Now(),
		Configured: a.cfgStore.Configured(),
	}
}

func (a *App) render(w http.ResponseWriter, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template %s: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (a *App) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *App) requireConfigured(w http.ResponseWriter, r *http.Request) bool {
	if a.cfgStore.Configured() && a.client.Configured() {
		return true
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
	return false
}

func (a *App) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if !a.requireConfigured(w, r) {
		return
	}
	data := a.basePage("dashboard", "Dashboard")
	dash, err := a.client.GetDashboard()
	if err != nil {
		data.Error = err.Error()
		dash.Mode = mirth.ModeUnreachable
		dash.ErrorMessage = err.Error()
		dash.RefreshedAt = time.Now()
	}
	data.Dashboard = &dash
	data.Mode = dash.Mode
	data.ModeLabel = a.modeLabel(dash.Mode)
	a.render(w, "dashboard.html", data)
}

func (a *App) handleSearch(w http.ResponseWriter, r *http.Request) {
	if !a.requireConfigured(w, r) {
		return
	}
	data := a.basePage("search", "Message search")
	channels, err := a.client.ListChannels()
	if err != nil {
		data.Error = err.Error()
	}
	data.Channels = channels
	form := searchForm{
		ChannelID:  r.URL.Query().Get("channelId"),
		Column:     r.URL.Query().Get("column"),
		Operator:   r.URL.Query().Get("operator"),
		Value:      r.URL.Query().Get("value"),
		Status:     r.URL.Query().Get("status"),
		StartDate:  toDateTimeLocal(r.URL.Query().Get("startDate")),
		EndDate:    toDateTimeLocal(r.URL.Query().Get("endDate")),
		IgnoreCase: r.URL.Query().Get("ignoreCase") == "1",
	}
	if form.Operator == "" {
		form.Operator = "CONTAINS"
	}
	data.Form = form

	page := parsePositiveInt(r.URL.Query().Get("page"), 1)
	pageSize := parsePositiveInt(r.URL.Query().Get("pageSize"), 5)
	if pageSize > 100 {
		pageSize = 100
	}

	if form.ChannelID != "" {
		cols, err := a.client.GetMetaDataColumns(form.ChannelID)
		if err != nil {
			data.Error = err.Error()
		} else {
			data.Columns = cols
		}

		// Load channel messages whenever a channel is selected (browse + filtered search).
		filter := mirth.MessageFilter{}
		if form.Status != "" {
			filter.Statuses = []string{form.Status}
		}
		if form.StartDate != "" {
			filter.StartDate = toMirthDate(form.StartDate, false)
		}
		if form.EndDate != "" {
			filter.EndDate = toMirthDate(form.EndDate, true)
		}
		if form.Column != "" && form.Value != "" {
			filter.MetaDataSearch = []mirth.MetaDataSearchElement{{
				ColumnName: form.Column,
				Operator:   form.Operator,
				Value:      form.Value,
				IgnoreCase: form.IgnoreCase,
			}}
		}

		total, err := a.client.CountMessages(form.ChannelID, filter)
		if err != nil {
			// Count can fail on some filters; fall back to unknown total.
			log.Printf("message count: %v", err)
			total = -1
		}
		totalPages := 1
		if total > 0 {
			totalPages = int((total + int64(pageSize) - 1) / int64(pageSize))
		}
		if totalPages < 1 {
			totalPages = 1
		}
		if page > totalPages && total >= 0 {
			page = totalPages
		}
		offset := (page - 1) * pageSize

		results, err := a.client.SearchMessages(form.ChannelID, filter, offset, pageSize, false)
		totalInt := int(total)
		if err != nil {
			data.Error = err.Error()
		} else {
			data.Results = results
			if total < 0 {
				totalInt = offset + len(results)
				totalPages = page
				if len(results) == pageSize {
					totalPages = page + 1
					totalInt++
				}
			}
		}

		q := r.URL.Query()
		q.Del("page")
		data.Pager = &pagerData{
			Page:       page,
			PageSize:   pageSize,
			Total:      totalInt,
			TotalPages: totalPages,
			Offset:     offset,
			HasPrev:    page > 1,
			HasNext:    page < totalPages,
			PrevPage:   page - 1,
			NextPage:   page + 1,
			Query:      template.URL(q.Encode()),
		}
	}
	a.render(w, "search.html", data)
}

func parsePositiveInt(v string, fallback int) int {
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func (a *App) handleMessageDetail(w http.ResponseWriter, r *http.Request) {
	if !a.requireConfigured(w, r) {
		return
	}
	data := a.basePage("search", "Message detail")
	channelID := r.PathValue("channelId")
	messageID, err := strconv.ParseInt(r.PathValue("messageId"), 10, 64)
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}
	detail, err := a.client.GetMessage(channelID, messageID)
	if err != nil {
		data.Error = err.Error()
	} else {
		data.Detail = &detail
	}
	a.render(w, "message.html", data)
}

func (a *App) handleAlarms(w http.ResponseWriter, r *http.Request) {
	if !a.requireConfigured(w, r) {
		return
	}
	data := a.basePage("alarms", "Alarms")
	if msg := r.URL.Query().Get("ok"); msg == "1" {
		data.Success = "Alarm channel selection saved."
	}
	list, err := a.store.List(true, 200)
	if err != nil {
		data.Error = err.Error()
	} else {
		data.Alarms = list
	}
	data.WatchChannels, data.WatchConfigured = a.loadWatchOptions()
	a.render(w, "alarms.html", data)
}

func (a *App) loadWatchOptions() ([]watchChannelOption, bool) {
	channels, err := a.client.ListChannels()
	if err != nil {
		dash, dashErr := a.client.GetDashboard()
		if dashErr != nil {
			return nil, false
		}
		channels = make([]mirth.ChannelInfo, 0, len(dash.Channels))
		for _, ch := range dash.Channels {
			channels = append(channels, mirth.ChannelInfo{ID: ch.ChannelID, Name: ch.Name})
		}
	}
	watched, configured, err := a.store.WatchedSet()
	if err != nil {
		log.Printf("watch config: %v", err)
	}
	out := make([]watchChannelOption, 0, len(channels))
	for _, ch := range channels {
		watchedFlag := true
		if configured {
			watchedFlag = watched[ch.ID]
		}
		out = append(out, watchChannelOption{
			ID:      ch.ID,
			Name:    ch.Name,
			Watched: watchedFlag,
		})
	}
	return out, configured
}

func (a *App) handleAlarmsWatch(w http.ResponseWriter, r *http.Request) {
	if !a.requireConfigured(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ids := r.Form["channelId"]
	names := map[string]string{}
	channels, _ := a.client.ListChannels()
	for _, ch := range channels {
		names[ch.ID] = ch.Name
	}
	if len(names) == 0 {
		if dash, err := a.client.GetDashboard(); err == nil {
			for _, ch := range dash.Channels {
				names[ch.ChannelID] = ch.Name
			}
		}
	}
	selected := make([]alarms.WatchedChannel, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		name := names[id]
		if name == "" {
			name = id
		}
		selected = append(selected, alarms.WatchedChannel{ChannelID: id, ChannelName: name})
	}
	if err := a.store.SetWatchConfig(selected); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/alarms?ok=1", http.StatusSeeOther)
}

func (a *App) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	data := a.basePage("settings", "Settings")
	cfg := a.cfgStore.Get()
	data.Settings = settingsForm{
		URL:                 cfg.MirthURL,
		Username:            cfg.MirthUsername,
		PasswordSet:         cfg.MirthPassword != "",
		TLSInsecure:         cfg.TLSInsecure,
		NotifyEnabled:       cfg.TelegramEnabled(),
		NotifyDestination:   cfg.NotifyDestination,
		NotifyTokenSet:      cfg.NotifyTelegramBotToken != "",
		WhatsAppEnabled:     cfg.WhatsAppEnabled,
		WhatsAppBaseURL:     cfg.WhatsAppBaseURL,
		WhatsAppAPIKeySet:   cfg.WhatsAppAPIKey != "",
		WhatsAppInstance:    cfg.WhatsAppInstance,
		WhatsAppDestination: cfg.WhatsAppDestination,
	}
	if msg := r.URL.Query().Get("ok"); msg == "1" {
		data.Success = "Settings saved."
	}
	if errMsg := r.URL.Query().Get("error"); errMsg != "" {
		data.Error = errMsg
	}
	a.render(w, "settings.html", data)
}

func (a *App) handleSettingsPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	url := strings.TrimSpace(r.FormValue("url"))
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	tlsInsecure := r.FormValue("tlsInsecure") == "1"
	notifyEnabled := r.FormValue("notifyEnabled") == "1"
	notifyToken := strings.TrimSpace(r.FormValue("notifyTelegramBotToken"))
	notifyDest := strings.TrimSpace(r.FormValue("notifyDestination"))
	waEnabled := r.FormValue("whatsappEnabled") == "1"
	waBase := strings.TrimSpace(r.FormValue("whatsappBaseUrl"))
	waKey := strings.TrimSpace(r.FormValue("whatsappApiKey"))
	waInstance := strings.TrimSpace(r.FormValue("whatsappInstance"))
	waDest := strings.TrimSpace(r.FormValue("whatsappDestination"))

	cfg := a.cfgStore.Get()
	if password == "" {
		password = cfg.MirthPassword
	}
	if url == "" || username == "" || password == "" {
		http.Redirect(w, r, "/settings?error="+urlQuery("URL, username, and password are required"), http.StatusSeeOther)
		return
	}
	if notifyEnabled {
		token := notifyToken
		if token == "" {
			token = cfg.NotifyTelegramBotToken
		}
		if token == "" || notifyDest == "" {
			http.Redirect(w, r, "/settings?error="+urlQuery("Telegram requires a bot token and chat id"), http.StatusSeeOther)
			return
		}
	}
	if waEnabled {
		key := waKey
		if key == "" {
			key = cfg.WhatsAppAPIKey
		}
		if waBase == "" || key == "" || waInstance == "" || waDest == "" {
			http.Redirect(w, r, "/settings?error="+urlQuery("WhatsApp requires gateway base URL, API key, instance, and destination (phone or group JID)"), http.StatusSeeOther)
			return
		}
	}

	if err := a.cfgStore.Save(config.SaveInput{
		URL:                    url,
		Username:               username,
		Password:               password,
		TLSInsecure:            tlsInsecure,
		NotifyEnabled:          notifyEnabled,
		NotifyTelegramBotToken: notifyToken,
		NotifyDestination:      notifyDest,
		WhatsAppEnabled:        waEnabled,
		WhatsAppBaseURL:        waBase,
		WhatsAppAPIKey:         waKey,
		WhatsAppInstance:       waInstance,
		WhatsAppDestination:    waDest,
	}); err != nil {
		http.Redirect(w, r, "/settings?error="+urlQuery(err.Error()), http.StatusSeeOther)
		return
	}

	saved := a.cfgStore.Get()
	if a.notifier != nil {
		a.notifier.Replace(notifyConfigFrom(saved))
	}

	if err := a.client.Configure(saved.MirthURL, saved.MirthUsername, saved.MirthPassword, saved.TLSInsecure); err != nil {
		http.Redirect(w, r, "/settings?error="+urlQuery(err.Error()), http.StatusSeeOther)
		return
	}
	if err := a.client.Ping(); err != nil {
		http.Redirect(w, r, "/settings?error="+urlQuery("Saved, but Mirth login failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings?ok=1", http.StatusSeeOther)
}

func notifyConfigFrom(cfg config.Config) notify.Config {
	return notify.Config{
		Provider:            cfg.NotifyProvider,
		TelegramEnabled:     cfg.TelegramEnabled(),
		TelegramBotToken:    cfg.NotifyTelegramBotToken,
		TelegramChatID:      cfg.NotifyDestination,
		WhatsAppEnabled:     cfg.WhatsAppEnabled,
		WhatsAppBaseURL:     cfg.WhatsAppBaseURL,
		WhatsAppAPIKey:      cfg.WhatsAppAPIKey,
		WhatsAppInstance:    cfg.WhatsAppInstance,
		WhatsAppDestination: cfg.WhatsAppDestination,
	}
}

func urlQuery(s string) string {
	return url.QueryEscape(s)
}

// toDateTimeLocal keeps values suitable for <input type="datetime-local">.
func toDateTimeLocal(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", v, time.Local); err == nil {
		return t.Format("2006-01-02T15:04")
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", v, time.Local); err == nil {
		return t.Format("2006-01-02T15:04")
	}
	layouts := []string{
		"2006-01-02T15:04:05.000-0700",
		"2006-01-02T15:04:05.000Z0700",
		time.RFC3339,
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, v); err == nil {
			return t.In(time.Local).Format("2006-01-02T15:04")
		}
	}
	if len(v) >= 16 && v[10] == 'T' {
		return v[:16]
	}
	return v
}

// toMirthDate converts HTML datetime-local values (YYYY-MM-DDTHH:MM[:SS])
// into Mirth's expected form, e.g. 2015-10-21T07:28:00.000+0700.
func toMirthDate(v string, endOfMinute bool) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	layouts := []string{
		"2006-01-02T15:04:05.000",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02",
		time.RFC3339,
		"2006-01-02T15:04:05.000-0700",
		"2006-01-02T15:04:05.000Z0700",
	}
	var t time.Time
	var err error
	for _, layout := range layouts {
		t, err = time.ParseInLocation(layout, v, time.Local)
		if err == nil {
			break
		}
	}
	if err != nil {
		return v
	}
	if endOfMinute && len(v) <= len("2006-01-02T15:04") {
		t = t.Add(59*time.Second + 999*time.Millisecond)
	}
	_, offset := t.Zone()
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	hh := offset / 3600
	mm := (offset % 3600) / 60
	return fmt.Sprintf("%s%s%02d%02d", t.Format("2006-01-02T15:04:05.000"), sign, hh, mm)
}

func (a *App) handleAPIDashboard(w http.ResponseWriter, r *http.Request) {
	dash, err := a.client.GetDashboard()
	if err != nil {
		a.writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "mode": mirth.ModeUnreachable})
		return
	}
	a.writeJSON(w, http.StatusOK, dash)
}

func (a *App) handleAPIMetadata(w http.ResponseWriter, r *http.Request) {
	channelID := r.PathValue("channelId")
	cols, err := a.client.GetMetaDataColumns(channelID)
	if err != nil {
		a.writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	a.writeJSON(w, http.StatusOK, cols)
}

func (a *App) handleAlarmsFeed(w http.ResponseWriter, r *http.Request) {
	sinceStr := r.URL.Query().Get("since")
	since := time.Now().Add(-24 * time.Hour)
	if sinceStr != "" {
		if t, err := time.Parse(time.RFC3339Nano, sinceStr); err == nil {
			since = t
		} else if t, err := time.Parse(time.RFC3339, sinceStr); err == nil {
			since = t
		}
	}
	feed, err := a.store.Feed(since)
	if err != nil {
		a.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	count, _ := a.store.UnackedCount()
	a.writeJSON(w, http.StatusOK, map[string]any{
		"alarms":  feed,
		"unacked": count,
		"now":     time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (a *App) handleAlarmsAck(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	all := r.FormValue("all") == "1"
	if all {
		if err := a.store.AckAll(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "id required", http.StatusBadRequest)
			return
		}
		if err := a.store.Ack(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") || r.FormValue("json") == "1" {
		count, _ := a.store.UnackedCount()
		a.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "unacked": count})
		return
	}
	http.Redirect(w, r, "/alarms", http.StatusSeeOther)
}

func ListenAndServe(addr string, h http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	fmt.Printf("Mirth monitor listening on http://localhost%s\n", addr)
	return srv.ListenAndServe()
}
