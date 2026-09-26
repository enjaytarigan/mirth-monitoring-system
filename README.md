# Mirth Monitor

Full-stack Go operations console for **NextGen Connect (Mirth) 4.5.2**. Talks only to the live Client API — no demo data.

## Features

- **Dashboard** — channel state and current statistics from `GET /api/channels/statuses`
- **Search** — metadata message search via `GET /api/channels/{id}/messages` (with `metaDataSearch` query params)
- **Alarms** — PostgreSQL-backed ERROR alarms; optional Telegram and/or WhatsApp alerts
- **Settings** — Mirth URL / credentials and notification channels; saved to `data/connection.json`

## Requirements

- Go 1.25+
- PostgreSQL 14+ (or use Docker Compose below)

## Run (local)

```bash
cp .env.example .env
# set DATABASE_URL, and optionally MIRTH_* seed values
go run ./cmd/server
```

Open http://localhost:8080

If credentials are missing, you are redirected to **Settings**. Prefer configuring Mirth and notifications there.

Every Mirth API call sends `X-Requested-With: OpenAPI` as required by your server.

## Docker (recommended)

Multi-stage **distroless** image + Postgres:

```bash
docker compose up -d --build
```

- App: http://localhost:8080  
- Postgres: internal only (`postgres://mirth:mirth@postgres:5432/mirth_monitor`)  
- App data volume: `app_data` → `/app/data` (`connection.json`)

Build image only:

```bash
docker build -t mirth-monitor:latest .
```

## Telegram notifications (free)

Configure under **Settings** (not env):

1. Talk to [@BotFather](https://t.me/BotFather) → `/newbot` → copy the bot token  
2. Add the bot to your support group and allow it to post  
3. Get the group `chat_id` via `https://api.telegram.org/bot<token>/getUpdates`  
4. Open **Settings** → enable Telegram → paste bot token and chat id → Save  

## WhatsApp notifications

Requires a self-hosted WhatsApp HTTP gateway (Evolution-compatible `sendText`).

1. Run and connect your gateway instance; note its name and API key  
2. Open **Settings** → enable WhatsApp  
3. Set gateway base URL, instance name, API key, and destination — phone (e.g. `62857…`) **or** group JID (e.g. `1203630…@g.us`)  
4. Save  

List groups (after the gateway session is connected):

```bash
curl -s "http://GATEWAY/group/fetchAllGroups/INSTANCE?getParticipants=false" -H "apikey: KEY"
```

Telegram and WhatsApp can both be enabled; each new ERROR alarm is fanned out to every enabled channel.

## Build (binary)

```bash
go build -o bin/mirth-monitor ./cmd/server
./bin/mirth-monitor
```
