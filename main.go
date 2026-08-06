package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-telegram/bot"
	"github.com/gocolly/colly/v2"
	"github.com/joho/godotenv"
)

const (
	DIR                   = "titles"
	MAX_MESSAGE_LENGTH    = 4096
	REPEAT_DELAY_IN_HOURS = 1
	REQUEST_TIMEOUT       = 120 * time.Second
	SEND_TIMEOUT          = 30 * time.Second
	SEND_ATTEMPTS         = 3
	MAX_RETRY_WAIT        = 60 * time.Second
	DEFAULT_USER_AGENT    = "Mozilla/5.0 (compatible; important-notifications/1.0)"
)

var chatIDs map[string]int64

var userAgent string

// sendFunc delivers one already-split message to a channel.
type sendFunc func(chatID int64, text string) error

// Seams for tests: the real implementations reach the network and the titles
// directory, tests swap in fakes.
var (
	titlesDir    = DIR
	fetchAlert   = getLastAlert
	fetchContent = getHtmlContent
)

func main() {
	loadEnv()
	chatIDs = loadChatIDs()
	userAgent = loadUserAgent()
	ensureTitlesDir()
	b := getBot()

	send := func(chatID int64, text string) error {
		return sendMessage(b, chatID, text)
	}

	for {
		log.Println("Checking...")
		var wg sync.WaitGroup
		for country, chatID := range chatIDs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// A single misbehaving embassy site must not take down the
				// checks for the other countries.
				defer func() {
					if r := recover(); r != nil {
						log.Printf("%s: panic: %v", country, r)
					}
				}()
				if err := checkCountry(send, country, chatID); err != nil {
					log.Printf("%s: %v", country, err)
				}
			}()
		}
		wg.Wait()
		log.Println("Checking completed and sleeping...")
		time.Sleep(time.Hour * REPEAT_DELAY_IN_HOURS)
	}
}

func checkCountry(send sendFunc, country string, chatID int64) error {
	baseURL := "https://" + country + ".usembassy.gov"
	fileName := titlesDir + "/" + country
	log.Print("Checking: " + baseURL)

	lastTitle, err := readFile(fileName)
	if err != nil {
		return fmt.Errorf("read last title: %w", err)
	}
	log.Print("Last alert: " + lastTitle)

	alert, err := fetchAlert(baseURL + "/category/alert/")
	if err != nil {
		return fmt.Errorf("fetch alert list: %w", err)
	}

	if lastTitle == "" {
		if err := saveFile(fileName, alert.Title); err != nil {
			return fmt.Errorf("save initial title: %w", err)
		}
		log.Print("Initial alert created: " + baseURL)
		return nil
	}

	if alert.Title == lastTitle {
		log.Print("No new alerts: " + baseURL)
		return nil
	}

	log.Print("New alert found : " + alert.Title + " (" + alert.URL + ")")
	content, err := fetchContent(alert.URL)
	if err != nil {
		return fmt.Errorf("fetch alert content: %w", err)
	}
	if content == "" {
		return errors.New("no content: " + alert.URL)
	}
	if !strings.Contains(content, alert.Title) {
		content = alert.Title + "\n\n" + content
	}

	// The title is recorded only once every part has reached Telegram, so a
	// failed send is retried on the next pass instead of being lost. The cost
	// is at-least-once delivery: a failure part way through a multi-part alert
	// re-sends the parts that already arrived.
	if err := sendAlert(send, chatID, content); err != nil {
		return fmt.Errorf("send alert: %w", err)
	}
	return saveFile(fileName, alert.Title)
}

func loadEnv() {
	if os.Getenv("APP_ENV") == "development" {
		if err := godotenv.Load(); err != nil {
			log.Fatalf("cannot load .env: %v", err)
		}
	}
}

func loadChatIDs() map[string]int64 {
	envKeys := map[string]string{
		"ua": "CHANNEL_ID_UA",
		"tr": "CHANNEL_ID_TR",
		"il": "CHANNEL_ID_IL",
		"ru": "CHANNEL_ID_RU",
		"ir": "CHANNEL_ID_IR",
	}
	result := make(map[string]int64, len(envKeys))
	for country, key := range envKeys {
		raw := os.Getenv(key)
		if raw == "" {
			log.Fatalf("missing environment variable: %s", key)
		}
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			log.Fatalf("invalid %s: %v", key, err)
		}
		result[country] = id
	}
	return result
}

// ensureTitlesDir fails fast when the titles directory cannot be written to.
// Without it an unwritable bind mount would only surface after an alert had
// already been broadcast, and the same alert would then be re-broadcast every
// hour because the title could never be recorded.
func ensureTitlesDir() {
	if err := os.MkdirAll(titlesDir, 0755); err != nil {
		log.Fatalf("cannot create %s: %v", titlesDir, err)
	}
	probe := titlesDir + "/.write-probe"
	if err := os.WriteFile(probe, []byte("ok"), 0644); err != nil {
		log.Fatalf("%s is not writable: %v", titlesDir, err)
	}
	if err := os.Remove(probe); err != nil {
		log.Printf("cannot remove %s: %v", probe, err)
	}
}

// utf16Len reports the length of s in UTF-16 code units, which is the unit
// Telegram counts a message against.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xFFFF {
			n++
		}
	}
	return n
}

// cutIndex returns the byte index at which s must be cut so the first piece
// fits within limit UTF-16 code units. It prefers a paragraph break, then a
// line break, then a space, and otherwise falls back to the last whole rune
// that fits. The returned index is always on a rune boundary and greater than
// zero, so callers always make progress.
func cutIndex(s string, limit int) int {
	units, hardEnd := 0, len(s)
	for i, r := range s {
		width := 1
		if r > 0xFFFF {
			width = 2
		}
		if units+width > limit {
			hardEnd = i
			break
		}
		units += width
	}
	if hardEnd == 0 {
		// limit is smaller than the first rune; emit that rune alone rather
		// than spinning forever on a zero-width cut.
		_, size := utf8.DecodeRuneInString(s)
		return size
	}

	// Only accept a separator that leaves the piece at least half full,
	// otherwise a stray early newline would produce a flood of tiny messages.
	minFill := hardEnd / 2
	for _, sep := range []string{"\n\n", "\n", " "} {
		if i := strings.LastIndex(s[:hardEnd], sep); i >= minFill {
			return i + len(sep)
		}
	}
	return hardEnd
}

// split breaks content into pieces that each fit within limit UTF-16 code
// units, cutting on paragraph, line or word boundaries and never in the middle
// of a rune.
func split(content string, limit int) []string {
	var parts []string
	for {
		content = strings.TrimLeft(content, " \t\n\r")
		if content == "" {
			return parts
		}
		if utf16Len(content) <= limit {
			return append(parts, strings.TrimRight(content, " \t\n\r"))
		}
		cut := cutIndex(content, limit)
		if part := strings.TrimRight(content[:cut], " \t\n\r"); part != "" {
			parts = append(parts, part)
		}
		content = content[cut:]
	}
}

func getBot() *bot.Bot {
	token := os.Getenv("BOT_TOKEN")
	b, err := bot.New(token)
	if err != nil {
		log.Fatalf("cannot create bot: %v", err)
	}
	return b
}

func sendAlert(send sendFunc, chatID int64, content string) error {
	for _, part := range split(content, MAX_MESSAGE_LENGTH) {
		if err := send(chatID, part); err != nil {
			return err
		}
	}
	return nil
}

// sendMessage retries within the pass so that a transient failure does not
// abort a multi-part alert and force the whole thing to be re-sent an hour
// later.
func sendMessage(b *bot.Bot, chatID int64, text string) error {
	var err error
	for attempt := 1; attempt <= SEND_ATTEMPTS; attempt++ {
		if attempt > 1 {
			time.Sleep(retryDelay(err, attempt))
		}

		ctx, cancel := context.WithTimeout(context.Background(), SEND_TIMEOUT)
		_, err = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   text,
		})
		cancel()

		if err == nil {
			return nil
		}
		log.Printf("send attempt %d/%d to %d failed: %v", attempt, SEND_ATTEMPTS, chatID, err)
	}
	return err
}

// retryBackoff is the base unit of the linear backoff; tests shorten it.
var retryBackoff = time.Second

// retryDelay honours Telegram's retry_after when the wait is short enough to
// sit through, and otherwise backs off linearly.
func retryDelay(err error, attempt int) time.Duration {
	var tooMany *bot.TooManyRequestsError
	if errors.As(err, &tooMany) {
		wait := time.Duration(tooMany.RetryAfter) * time.Second
		if wait > 0 && wait <= MAX_RETRY_WAIT {
			return wait
		}
	}
	return time.Duration(attempt) * retryBackoff
}

// saveFile writes atomically, so an interrupted write cannot leave a truncated
// title behind that would read back as a different alert.
func saveFile(fileName string, content string) error {
	tmp := fileName + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, fileName)
}

func readFile(fileName string) (string, error) {
	file, err := os.ReadFile(fileName)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(file)), nil
}

func loadUserAgent() string {
	ua, err := readFile("useragent")
	if err != nil {
		log.Printf("cannot read useragent file, using default: %v", err)
		return DEFAULT_USER_AGENT
	}
	if ua == "" {
		log.Print("useragent file is empty, using default")
		return DEFAULT_USER_AGENT
	}
	return ua
}

type Alert struct {
	Title string
	URL   string
}

func getCollector() *colly.Collector {
	c := colly.NewCollector()
	c.SetRequestTimeout(REQUEST_TIMEOUT)

	c.OnRequest(func(r *colly.Request) {
		r.Headers.Set("User-Agent", userAgent)
	})

	return c
}

func getLastAlert(url string) (Alert, error) {
	c := getCollector()
	var alert Alert

	c.OnHTML("main > article", func(e *colly.HTMLElement) {
		if alert != (Alert{}) {
			return
		}
		alert = Alert{
			Title: e.ChildText("h2.entry-title a"),
			URL:   e.ChildAttr("h2.entry-title a", "href"),
		}
	})

	if err := c.Visit(url); err != nil {
		return Alert{}, err
	}
	if alert.Title == "" || alert.URL == "" {
		return Alert{}, errors.New("no alert found: " + url)
	}
	return alert, nil
}

func getHtmlContent(url string) (string, error) {
	c := getCollector()
	var sb strings.Builder

	previous := ""

	c.OnHTML(`.paragraph.alignwide.container .row`, func(e *colly.HTMLElement) {
		e.ForEach(`p, ul li`, func(_ int, el *colly.HTMLElement) {
			text := strings.TrimSpace(el.Text)
			if text == "" {
				return
			}

			if sb.Len() > 0 {
				// Consecutive list items go on adjacent lines, everything else
				// is separated by a blank line. Without this the items of a
				// list ran together into one unreadable word.
				if previous == "li" && el.Name == "li" {
					sb.WriteString("\n")
				} else {
					sb.WriteString("\n\n")
				}
			}
			sb.WriteString(text)
			previous = el.Name
		})
	})

	if err := c.Visit(url); err != nil {
		return "", err
	}
	return sb.String(), nil
}
