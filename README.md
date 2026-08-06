# important-notifications

A Go service that monitors US Embassy websites for new security alerts and forwards them to Telegram channels.

## How It Works

Every hour the service scrapes the alerts page of each configured US Embassy website. If a new alert is detected — by comparing the title against the last delivered one — it fetches the full alert content and sends it to the corresponding Telegram channel.

Long alerts are split across several messages. The split happens on paragraph, line or word boundaries, never in the middle of a character, and the 4096 limit is measured in UTF-16 code units, which is how Telegram counts it.

Each country is checked in its own goroutine. A site that is unreachable, or an alert that fails to send, is logged and retried on the next pass; it never affects the other countries and never stops the service.

## Monitored Countries

| Country | Embassy URL      | Telegram Channel |
| ------- | ---------------- | ---------------- |
| Ukraine | ua.usembassy.gov | `CHANNEL_ID_UA`  |
| Turkey  | tr.usembassy.gov | `CHANNEL_ID_TR`  |
| Israel  | il.usembassy.gov | `CHANNEL_ID_IL`  |
| Russia  | ru.usembassy.gov | `CHANNEL_ID_RU`  |
| Iran    | ir.usembassy.gov | `CHANNEL_ID_IR`  |

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
CHANNEL_ID_IR=-100123456789
```

In production, set environment variables directly (e.g. via Docker Compose). `.env` is listed in `.dockerignore`, so the bot token is never baked into the image.

| Variable        | Description                                      |
| --------------- | ------------------------------------------------ |
| `BOT_TOKEN`     | Telegram bot token                               |
| `APP_ENV`       | Set to `development` to load `.env` file locally |
| `CHANNEL_ID_UA` | Telegram channel ID for Ukraine alerts           |
| `CHANNEL_ID_TR` | Telegram channel ID for Turkey alerts            |
| `CHANNEL_ID_IL` | Telegram channel ID for Israel alerts            |
| `CHANNEL_ID_RU` | Telegram channel ID for Russia alerts            |
| `CHANNEL_ID_IR` | Telegram channel ID for Iran alerts              |

The `titles/` directory must be writable. It is checked at startup and the service refuses to start otherwise, rather than discovering the problem after an alert has already been broadcast.

## Delivery State

`titles/<country>` records what has been delivered, as JSON:

```json
{
  "title": "Security Alert: U.S. Embassy Kyiv, Ukraine",
  "url": "https://ua.usembassy.gov/security-alert-june-6-2025/"
}
```

`title` and `url` identify the last alert delivered **in full**. They are written only after every message of that alert has reached Telegram, so a failed send is retried on the next pass instead of being silently dropped.

The permalink is part of the identity because embassies reuse headlines: `Security Alert: U.S. Embassy Kyiv, Ukraine` has covered several unrelated alerts months apart. Comparing titles alone made the second one look like the first and dropped it entirely.

While a multi-part alert is being delivered, the file also carries a checkpoint:

```json
{
  "title": "the previous alert",
  "url": "its permalink",
  "pending": "the alert being delivered",
  "digest": "sha256 of its content",
  "sent": 2
}
```

The next pass resumes at part 3 rather than re-sending the two parts subscribers have already read. The digest guards the offset: if the embassy edits the page between passes the text no longer matches, so delivery restarts from the beginning instead of skipping the wrong section.

An empty or missing file means "nothing seen yet": the current alert is recorded without being sent, so a fresh deployment does not replay old alerts. Files written by earlier versions hold a bare title rather than JSON and are still read correctly, so upgrading does not re-send the current alert.

Two gaps a file cannot close leave delivery at-least-once, and both repeat only the messages sent since the last checkpoint that reached disk: a crash between a successful send and its checkpoint, and a checkpoint write that itself fails after the send succeeded. Every other interruption resumes exactly once.

**Rolling back to a build older than this one re-sends the current alert once per channel**, because that build reads the whole file as a bare title and the JSON does not match. Upgrading is safe in the other direction — bare-title files are still read correctly.

## Running Locally

```bash
go run main.go
```

## Tests

```bash
go test ./...
```

The suite runs offline. It drives the scraper against a fake embassy site and the real Telegram client against a fake Bot API, covering the splitter, the delivery state machine, resume-after-failure and the retry behaviour.

## Docker

Build and run with Docker Compose:

```bash
docker-compose up -d
```

The `titles/` directory is mounted as a volume so delivery state persists across container restarts.

## Project Structure

```
.
├── main.go            # Application entry point
├── main_test.go       # Tests
├── useragent          # User-agent string used for HTTP requests
├── titles/            # Delivery state per country
│   ├── il
│   ├── ir
│   ├── ru
│   ├── tr
│   └── ua
├── Dockerfile
├── .dockerignore      # Keeps .env and .git out of the image
├── docker-compose.yml
└── .env               # Local environment variables (not committed)
```

## License

See [LICENSE](LICENSE).
