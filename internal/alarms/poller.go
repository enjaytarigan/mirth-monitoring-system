package alarms

import (
	"log"
	"strings"
	"time"

	"github.com/enjaytarigan/mirth_monitoring_system/internal/mirth"
	"github.com/enjaytarigan/mirth_monitoring_system/internal/notify"
)

type Poller struct {
	client   *mirth.Client
	store    *Store
	notifier notify.Notifier
	interval time.Duration
	stop     chan struct{}
}

func NewPoller(client *mirth.Client, store *Store, notifier notify.Notifier, intervalMS int) *Poller {
	if intervalMS < 1000 {
		intervalMS = 15000
	}
	if notifier == nil {
		notifier = notify.New(notify.Config{Provider: "none"})
	}
	return &Poller{
		client:   client,
		store:    store,
		notifier: notifier,
		interval: time.Duration(intervalMS) * time.Millisecond,
		stop:     make(chan struct{}),
	}
}

func (p *Poller) Start() {
	go func() {
		p.tick()
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.tick()
			case <-p.stop:
				return
			}
		}
	}()
}

func (p *Poller) Stop() {
	close(p.stop)
}

func (p *Poller) tick() {
	if !p.client.Configured() {
		return
	}
	watched, configured, err := p.store.WatchedSet()
	if err != nil {
		log.Printf("alarm poller: watch config: %v", err)
		return
	}
	if configured && len(watched) == 0 {
		return
	}
	dash, err := p.client.GetDashboard()
	if err != nil {
		log.Printf("alarm poller: dashboard: %v", err)
		return
	}
	for _, ch := range dash.Channels {
		if configured && !watched[ch.ChannelID] {
			continue
		}
		wm, err := p.store.GetWatermark(ch.ChannelID)
		if err != nil {
			log.Printf("alarm poller: watermark %s: %v", ch.ChannelID, err)
			continue
		}
		// First sight: advance watermark to current max so only new errors alarm.
		if wm == 0 {
			maxID, err := p.client.MaxMessageID(ch.ChannelID)
			if err != nil {
				log.Printf("alarm poller: maxMessageId %s: %v", ch.ChannelID, err)
				continue
			}
			if maxID > 0 {
				_ = p.store.SetWatermark(ch.ChannelID, maxID)
			}
			continue
		}
		errs, err := p.client.FetchErrorMessages(ch.ChannelID, ch.Name, wm, 50)
		if err != nil {
			log.Printf("alarm poller: errors %s: %v", ch.ChannelID, err)
			continue
		}
		maxSeen := wm
		for _, e := range errs {
			alarm, inserted, err := p.store.Insert(e.ChannelID, e.ChannelName, e.MessageID, e.ConnectorName, e.ErrorText)
			if err != nil {
				log.Printf("alarm poller: insert: %v", err)
				continue
			}
			if inserted && p.notifier.Enabled() {
				ev := notify.AlarmEvent{
					ChannelID:     alarm.ChannelID,
					ChannelName:   alarm.ChannelName,
					MessageID:     alarm.MessageID,
					ConnectorName: alarm.ConnectorName,
					ErrorText:     firstNonEmpty(alarm.ErrorText, e.ErrorText),
					ReceivedDate:  e.ReceivedDate,
					MetaData:      e.MetaData,
				}
				// Enrich from full message when list response lacks metadata/error text.
				if len(ev.MetaData) == 0 || strings.TrimSpace(ev.ErrorText) == "" {
					if detail, err := p.client.GetMessage(e.ChannelID, e.MessageID); err == nil {
						if len(ev.MetaData) == 0 && len(detail.MetaData) > 0 {
							ev.MetaData = detail.MetaData
						}
						if strings.TrimSpace(ev.ErrorText) == "" {
							for _, c := range detail.Connectors {
								if !strings.EqualFold(c.ConnectorName, e.ConnectorName) && e.ConnectorName != "" {
									continue
								}
								if t := firstNonEmpty(c.ProcessingError, c.ResponseError); t != "" {
									ev.ErrorText = t
									break
								}
							}
						}
						if ev.ReceivedDate == "" {
							ev.ReceivedDate = detail.ReceivedDate
						}
					}
				}
				if err := p.notifier.Notify(ev); err != nil {
					log.Printf("alarm notify: %v", err)
				}
			}
			if e.MessageID > maxSeen {
				maxSeen = e.MessageID
			}
		}
		if maxSeen > wm {
			_ = p.store.SetWatermark(ch.ChannelID, maxSeen)
		}
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
