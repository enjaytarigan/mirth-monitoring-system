package alarms

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type Alarm struct {
	ID            int64      `json:"id"`
	ChannelID     string     `json:"channelId"`
	ChannelName   string     `json:"channelName"`
	MessageID     int64      `json:"messageId"`
	ConnectorName string     `json:"connectorName"`
	ErrorText     string     `json:"errorText"`
	CreatedAt     time.Time  `json:"createdAt"`
	AckedAt       *time.Time `json:"ackedAt,omitempty"`
}

type Store struct {
	db *sql.DB
}

func Open(databaseURL string) (*Store, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Hour)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS alarms (
  id BIGSERIAL PRIMARY KEY,
  channel_id TEXT NOT NULL,
  channel_name TEXT NOT NULL,
  message_id BIGINT NOT NULL,
  connector_name TEXT NOT NULL,
  error_text TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL,
  acknowledged_at TIMESTAMPTZ,
  CONSTRAINT alarms_dedupe UNIQUE (channel_id, message_id, connector_name)
);
CREATE TABLE IF NOT EXISTS watermarks (
  channel_id TEXT PRIMARY KEY,
  last_message_id BIGINT NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS watched_channels (
  channel_id TEXT PRIMARY KEY,
  channel_name TEXT NOT NULL DEFAULT ''
);
`)
	return err
}

func (s *Store) Insert(channelID, channelName string, messageID int64, connectorName, errorText string) (Alarm, bool, error) {
	now := time.Now().UTC()
	var id int64
	err := s.db.QueryRow(`
INSERT INTO alarms(channel_id, channel_name, message_id, connector_name, error_text, created_at)
VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT (channel_id, message_id, connector_name) DO NOTHING
RETURNING id
`, channelID, channelName, messageID, connectorName, errorText, now).Scan(&id)
	if err == sql.ErrNoRows {
		return Alarm{}, false, nil
	}
	if err != nil {
		// Unique index may not be registered as ON CONFLICT target if created as UNIQUE INDEX
		// rather than CONSTRAINT — fall back to conflict check via rows affected pattern.
		if isUniqueViolation(err) {
			return Alarm{}, false, nil
		}
		return Alarm{}, false, err
	}
	return Alarm{
		ID:            id,
		ChannelID:     channelID,
		ChannelName:   channelName,
		MessageID:     messageID,
		ConnectorName: connectorName,
		ErrorText:     errorText,
		CreatedAt:     now,
	}, true, nil
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate key")
}

func (s *Store) List(includeAcked bool, limit int) ([]Alarm, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT id, channel_id, channel_name, message_id, connector_name, error_text, created_at, acknowledged_at
FROM alarms`
	if !includeAcked {
		q += ` WHERE acknowledged_at IS NULL`
	}
	q += ` ORDER BY created_at DESC LIMIT $1`
	rows, err := s.db.Query(q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAlarms(rows)
}

func (s *Store) Feed(since time.Time) ([]Alarm, error) {
	rows, err := s.db.Query(`
SELECT id, channel_id, channel_name, message_id, connector_name, error_text, created_at, acknowledged_at
FROM alarms
WHERE created_at > $1 AND acknowledged_at IS NULL
ORDER BY created_at ASC
`, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAlarms(rows)
}

func (s *Store) UnackedCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM alarms WHERE acknowledged_at IS NULL`).Scan(&n)
	return n, err
}

func (s *Store) Ack(id int64) error {
	_, err := s.db.Exec(`UPDATE alarms SET acknowledged_at=$1 WHERE id=$2 AND acknowledged_at IS NULL`,
		time.Now().UTC(), id)
	return err
}

func (s *Store) AckAll() error {
	_, err := s.db.Exec(`UPDATE alarms SET acknowledged_at=$1 WHERE acknowledged_at IS NULL`,
		time.Now().UTC())
	return err
}

func (s *Store) GetWatermark(channelID string) (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT last_message_id FROM watermarks WHERE channel_id=$1`, channelID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

func (s *Store) SetWatermark(channelID string, messageID int64) error {
	_, err := s.db.Exec(`
INSERT INTO watermarks(channel_id, last_message_id) VALUES($1,$2)
ON CONFLICT(channel_id) DO UPDATE SET last_message_id=EXCLUDED.last_message_id
WHERE EXCLUDED.last_message_id > watermarks.last_message_id
`, channelID, messageID)
	return err
}

type WatchedChannel struct {
	ChannelID   string `json:"channelId"`
	ChannelName string `json:"channelName"`
}

// WatchConfig returns selected channel IDs for error listening.
// configured=false means the user has never saved a selection (poller watches all).
func (s *Store) WatchConfig() (ids []string, configured bool, err error) {
	var flag string
	err = s.db.QueryRow(`SELECT value FROM meta WHERE key='watch_configured'`).Scan(&flag)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	configured = flag == "1"
	rows, err := s.db.Query(`SELECT channel_id FROM watched_channels ORDER BY channel_name, channel_id`)
	if err != nil {
		return nil, configured, err
	}
	defer rows.Close()
	ids = make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, configured, err
		}
		ids = append(ids, id)
	}
	return ids, configured, rows.Err()
}

func (s *Store) WatchedSet() (map[string]bool, bool, error) {
	ids, configured, err := s.WatchConfig()
	if err != nil {
		return nil, false, err
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set, configured, nil
}

func (s *Store) SetWatchConfig(channels []WatchedChannel) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM watched_channels`); err != nil {
		return err
	}
	for _, ch := range channels {
		if strings.TrimSpace(ch.ChannelID) == "" {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO watched_channels(channel_id, channel_name) VALUES($1,$2)`,
			ch.ChannelID, ch.ChannelName,
		); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO meta(key, value) VALUES('watch_configured','1')
ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func scanAlarms(rows *sql.Rows) ([]Alarm, error) {
	out := make([]Alarm, 0)
	for rows.Next() {
		var a Alarm
		var acked sql.NullTime
		if err := rows.Scan(&a.ID, &a.ChannelID, &a.ChannelName, &a.MessageID, &a.ConnectorName, &a.ErrorText, &a.CreatedAt, &acked); err != nil {
			return nil, err
		}
		if acked.Valid {
			t := acked.Time.UTC()
			a.AckedAt = &t
		}
		a.CreatedAt = a.CreatedAt.UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}
