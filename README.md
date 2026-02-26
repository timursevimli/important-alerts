# important-notifications

A Go service that monitors US Embassy websites for new security alerts and forwards them to Telegram channels.

## How It Works

Every hour the service scrapes the alerts page of each configured US Embassy website. If a new alert is detected (by comparing the title against the last stored one), it fetches the full alert content and sends it to the corresponding Telegram channel. Long messages are automatically split to respect Telegram's 4096-character limit.

## Monitored Countries

| Country | Embassy URL      | Telegram Channel   |
| ------- | ---------------- | ------------------ |
| Ukraine | ua.usembassy.gov | `CHANNEL_ID_UA`    |
| Turkey  | tr.usembassy.gov | `CHANNEL_ID_TR`    |
| Israel  | il.usembassy.gov | `CHANNEL_ID_IL`    |
| Russia  | ru.usembassy.gov | `CHANNEL_ID_RU`    |

## Requirements

- Go 1.22+
- A Telegram bot token

## Configuration

Create a `.env` file (used only in `development` mode):

```env
BOT_TOKEN=your_telegram_bot_token
APP_ENV=development
CHANNEL_ID_UA=-100123456789
CHANNEL_ID_TR=-100123456789
CHANNEL_ID_IL=-100123456789
CHANNEL_ID_RU=-100123456789
```

In production, set environment variables directly (e.g. via Docker Compose).

| Variable        | Description                                      |
| --------------- | ------------------------------------------------ |
| `BOT_TOKEN`     | Telegram bot token                               |
| `APP_ENV`       | Set to `development` to load `.env` file locally |
| `CHANNEL_ID_UA` | Telegram channel ID for Ukraine alerts           |
| `CHANNEL_ID_TR` | Telegram channel ID for Turkey alerts            |
| `CHANNEL_ID_IL` | Telegram channel ID for Israel alerts            |
| `CHANNEL_ID_RU` | Telegram channel ID for Russia alerts            |

## Running Locally

```bash
go run main.go
```

## Docker

Build and run with Docker Compose:

```bash
docker-compose up -d
```

The `titles/` directory is mounted as a volume so alert state persists across container restarts.

## Project Structure

```
.
├── main.go            # Application entry point
├── useragent          # User-agent string used for HTTP requests
├── titles/            # Stores the last seen alert title per country
│   ├── il
│   ├── ru
│   ├── tr
│   └── ua
├── Dockerfile
├── docker-compose.yml
└── .env               # Local environment variables (not committed)
```

## License

See [LICENSE](LICENSE).
