package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-telegram/bot"
)

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

func TestUtf16Len(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"привет", 6},
		{"שלום", 4},
		{"Türkiye", 7},
		{"😀", 2},
		{"a😀b", 4},
	}
	for _, c := range cases {
		if got := utf16Len(c.in); got != c.want {
			t.Errorf("utf16Len(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestSplitKeepsRunesIntact(t *testing.T) {
	// Cyrillic is two bytes per rune, so a byte-based cut at an odd limit
	// would land inside a rune.
	content := strings.Repeat("привет ", 3000)

	for _, limit := range []int{1, 2, 3, 7, 100, 4096} {
		parts := split(content, limit)
		for i, part := range parts {
			if !utf8.ValidString(part) {
				t.Fatalf("limit %d: part %d is not valid UTF-8: %q", limit, i, part)
			}
			if got := utf16Len(part); got > limit {
				t.Fatalf("limit %d: part %d is %d units long", limit, i, got)
			}
		}
	}
}

func TestSplitPreservesContent(t *testing.T) {
	cases := []string{
		"",
		"short",
		strings.Repeat("привет ", 3000),
		strings.Repeat("שלום עולם\n\n", 800),
		strings.Repeat("Ağrı Dağı\nİstanbul\n", 700),
		strings.Repeat("😀", 5000),
		strings.Repeat("x", 10000),
	}
	// split trims whitespace at every cut, so the invariant is that no
	// non-whitespace character is lost, reordered or duplicated.
	dropSpace := func(s string) string {
		return strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return r
		}, s)
	}

	for _, content := range cases {
		parts := split(content, MAX_MESSAGE_LENGTH)
		want := dropSpace(content)
		got := dropSpace(strings.Join(parts, ""))
		if got != want {
			t.Errorf("content %.20q: round trip lost data (%d vs %d chars)", content, len(got), len(want))
		}
	}
}

func TestSplitRespectsSurrogatePairs(t *testing.T) {
	// Each emoji is one rune but two UTF-16 units, so a rune-count based
	// splitter would emit parts twice as long as Telegram allows.
	parts := split(strings.Repeat("😀", 5000), MAX_MESSAGE_LENGTH)
	if len(parts) < 3 {
		t.Fatalf("expected at least 3 parts, got %d", len(parts))
	}
	for i, part := range parts {
		if got := utf16Len(part); got > MAX_MESSAGE_LENGTH {
			t.Errorf("part %d is %d UTF-16 units, limit is %d", i, got, MAX_MESSAGE_LENGTH)
		}
	}
}

func TestSplitPrefersParagraphBoundaries(t *testing.T) {
	paragraph := strings.Repeat("a", 1000)
	content := strings.Repeat(paragraph+"\n\n", 10)

	parts := split(content, MAX_MESSAGE_LENGTH)
	if len(parts) < 2 {
		t.Fatalf("expected the content to be split, got %d part(s)", len(parts))
	}
	for i, part := range parts {
		if strings.HasPrefix(part, "a") && !strings.HasSuffix(part, "a") {
			t.Errorf("part %d does not end on a paragraph boundary: %.30q...", i, part)
		}
		for _, line := range strings.Split(part, "\n\n") {
			if line != "" && line != paragraph {
				t.Errorf("part %d contains a truncated paragraph of %d chars", i, len(line))
			}
		}
	}
}

func TestSplitDoesNotEmitTinyParts(t *testing.T) {
	// A stray newline near the start must not cause a one-character message.
	content := "a\n" + strings.Repeat("b", 8000)

	parts := split(content, MAX_MESSAGE_LENGTH)
	for i, part := range parts[:len(parts)-1] {
		if utf16Len(part) < MAX_MESSAGE_LENGTH/2 {
			t.Errorf("part %d is only %d units long", i, utf16Len(part))
		}
	}
}

func TestSplitEmptyAndWhitespace(t *testing.T) {
	for _, content := range []string{"", "   ", "\n\n\n", " \t\r\n "} {
		if parts := split(content, MAX_MESSAGE_LENGTH); len(parts) != 0 {
			t.Errorf("split(%q) = %q, want no parts", content, parts)
		}
	}
}

func TestSplitTerminatesOnLongUnbrokenText(t *testing.T) {
	// No separators at all: the splitter must still make progress.
	parts := split(strings.Repeat("ñ", 20000), 10)
	if len(parts) != 2000 {
		t.Fatalf("expected 2000 parts, got %d", len(parts))
	}
}

// stub replaces the network and filesystem seams so checkCountry can be driven
// without touching either.
type stub struct {
	dir        string
	alert      Alert
	alertErr   error
	content    string
	contentErr error
	sent       []string
	failOn     int // 1-based index of the part whose send fails; 0 never fails
	sendErr    error
}

func newStub(t *testing.T) *stub {
	t.Helper()
	s := &stub{dir: t.TempDir(), sendErr: errors.New("telegram unavailable")}

	oldDir, oldAlert, oldContent := titlesDir, fetchAlert, fetchContent
	titlesDir = s.dir
	fetchAlert = func(string) (Alert, error) { return s.alert, s.alertErr }
	fetchContent = func(string) (string, error) { return s.content, s.contentErr }
	t.Cleanup(func() {
		titlesDir, fetchAlert, fetchContent = oldDir, oldAlert, oldContent
	})

	return s
}

func (s *stub) send(_ int64, text string) error {
	s.sent = append(s.sent, text)
	if s.failOn != 0 && len(s.sent) == s.failOn {
		return s.sendErr
	}
	return nil
}

func (s *stub) setTitle(t *testing.T, country, title string) {
	t.Helper()
	if err := saveFile(s.dir+"/"+country, title); err != nil {
		t.Fatalf("saveFile: %v", err)
	}
}

func (s *stub) title(t *testing.T, country string) string {
	t.Helper()
	title, err := readFile(s.dir + "/" + country)
	if err != nil {
		t.Fatalf("readFile: %v", err)
	}
	return title
}

// The regression test for the original bug: the title used to be recorded
// before the send, so a failed send discarded the alert for good.
func TestCheckCountryKeepsOldTitleWhenSendFails(t *testing.T) {
	s := newStub(t)
	s.setTitle(t, "ua", "Old alert")
	s.alert = Alert{Title: "New alert", URL: "https://ua.usembassy.gov/new"}
	s.content = "New alert\n\nbody"
	s.failOn = 1

	if err := checkCountry(s.send, "ua", 1); err == nil {
		t.Fatal("expected an error when the send fails")
	}
	if got := s.title(t, "ua"); got != "Old alert" {
		t.Fatalf("stored title = %q, want %q so the alert is retried next pass", got, "Old alert")
	}
}

func TestCheckCountrySavesTitleAfterSuccessfulSend(t *testing.T) {
	s := newStub(t)
	s.setTitle(t, "tr", "Old alert")
	s.alert = Alert{Title: "New alert", URL: "https://tr.usembassy.gov/new"}
	s.content = "New alert\n\nbody"

	if err := checkCountry(s.send, "tr", 1); err != nil {
		t.Fatalf("checkCountry: %v", err)
	}
	if len(s.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(s.sent))
	}
	if got := s.title(t, "tr"); got != "New alert" {
		t.Fatalf("stored title = %q, want %q", got, "New alert")
	}
}

func TestSendAlertStopsAtTheFailingPart(t *testing.T) {
	s := newStub(t)
	s.failOn = 2

	content := strings.Repeat("абв ", 4000) // long enough to need several parts
	if err := sendAlert(s.send, 1, content); err == nil {
		t.Fatal("expected sendAlert to report the failure")
	}
	if len(s.sent) != 2 {
		t.Fatalf("attempted %d parts, want it to stop at the failing one", len(s.sent))
	}
}

func TestCheckCountryBootstrapDoesNotSend(t *testing.T) {
	s := newStub(t)
	s.alert = Alert{Title: "First alert", URL: "https://il.usembassy.gov/first"}
	s.content = "body"

	if err := checkCountry(s.send, "il", 1); err != nil {
		t.Fatalf("checkCountry: %v", err)
	}
	if len(s.sent) != 0 {
		t.Fatalf("sent %d messages on bootstrap, want none", len(s.sent))
	}
	if got := s.title(t, "il"); got != "First alert" {
		t.Fatalf("stored title = %q, want %q", got, "First alert")
	}
}

func TestCheckCountrySkipsUnchangedTitle(t *testing.T) {
	s := newStub(t)
	s.setTitle(t, "ru", "Same alert")
	s.alert = Alert{Title: "Same alert", URL: "https://ru.usembassy.gov/same"}
	s.contentErr = errors.New("the alert page must not be fetched")

	if err := checkCountry(s.send, "ru", 1); err != nil {
		t.Fatalf("checkCountry: %v", err)
	}
	if len(s.sent) != 0 {
		t.Fatalf("sent %d messages for an unchanged title, want none", len(s.sent))
	}
}

func TestCheckCountryDoesNotConsumeAlertWithoutContent(t *testing.T) {
	s := newStub(t)
	s.setTitle(t, "ir", "Old alert")
	s.alert = Alert{Title: "New alert", URL: "https://ir.usembassy.gov/new"}
	s.content = ""

	if err := checkCountry(s.send, "ir", 1); err == nil {
		t.Fatal("expected an error for an alert page with no content")
	}
	if got := s.title(t, "ir"); got != "Old alert" {
		t.Fatalf("stored title = %q, want the alert to stay unconsumed", got)
	}
}

// The scraper used to panic from colly's OnError handler, taking the whole
// process down with it.
func TestCheckCountryReportsFetchErrorInsteadOfPanicking(t *testing.T) {
	s := newStub(t)
	s.setTitle(t, "ua", "Old alert")
	s.alertErr = errors.New("dial tcp: connection refused")

	err := checkCountry(s.send, "ua", 1)
	if err == nil {
		t.Fatal("expected an error for an unreachable site")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error %q does not mention the underlying cause", err)
	}
	if got := s.title(t, "ua"); got != "Old alert" {
		t.Errorf("stored title = %q, want it untouched", got)
	}
}

func TestRetryDelay(t *testing.T) {
	cases := []struct {
		name string
		err  error
		try  int
		want time.Duration
	}{
		{"honours retry_after", &bot.TooManyRequestsError{RetryAfter: 7}, 2, 7 * time.Second},
		{"sees through wrapping", fmt.Errorf("send: %w", &bot.TooManyRequestsError{RetryAfter: 3}), 2, 3 * time.Second},
		{"ignores an absurd retry_after", &bot.TooManyRequestsError{RetryAfter: 3600}, 2, 2 * time.Second},
		{"backs off on other errors", errors.New("connection reset"), 3, 3 * time.Second},
	}
	for _, c := range cases {
		if got := retryDelay(c.err, c.try); got != c.want {
			t.Errorf("%s: retryDelay = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSaveFileLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()

	if err := saveFile(dir+"/ua", "Some alert"); err != nil {
		t.Fatalf("saveFile: %v", err)
	}
	if got, err := readFile(dir + "/ua"); err != nil || got != "Some alert" {
		t.Fatalf("readFile = %q, %v", got, err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Errorf("temporary file %s was left behind", entry.Name())
		}
	}
}

// fakeTelegram stands in for the Bot API. It fails the first failures requests
// and records the text of every message it is asked to deliver.
type fakeTelegram struct {
	mu       sync.Mutex
	received []string
	failures int
	response string
}

func newFakeTelegram(t *testing.T, failures int, response string) (*bot.Bot, *fakeTelegram) {
	t.Helper()

	f := &fakeTelegram{failures: failures, response: response}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		n := len(f.received)
		f.received = append(f.received, telegramText(t, r))
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if n < f.failures {
			fmt.Fprint(w, f.response)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"date":0,"chat":{"id":1,"type":"channel"}}}`)
	}))
	t.Cleanup(srv.Close)

	b, err := bot.New("123:fake-token", bot.WithServerURL(srv.URL), bot.WithSkipGetMe())
	if err != nil {
		t.Fatalf("bot.New: %v", err)
	}

	old := retryBackoff
	retryBackoff = time.Millisecond
	t.Cleanup(func() { retryBackoff = old })

	return b, f
}

func telegramText(t *testing.T, r *http.Request) string {
	t.Helper()

	mediaType, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if !strings.HasPrefix(mediaType, "multipart/") {
		body, _ := io.ReadAll(r.Body)
		return string(body)
	}

	reader := multipart.NewReader(r.Body, params["boundary"])
	for {
		part, err := reader.NextPart()
		if err != nil {
			return ""
		}
		if part.FormName() == "text" {
			body, _ := io.ReadAll(part)
			return string(body)
		}
	}
}

func (f *fakeTelegram) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.received...)
}

const serverError = `{"ok":false,"error_code":500,"description":"Internal Server Error"}`

// Retrying inside the pass keeps a hiccup on one part from forcing the whole
// alert to be re-sent an hour later.
func TestSendMessageRetriesTransientFailures(t *testing.T) {
	b, tg := newFakeTelegram(t, SEND_ATTEMPTS-1, serverError)

	if err := sendMessage(b, 1, "hello"); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	if got := len(tg.texts()); got != SEND_ATTEMPTS {
		t.Errorf("made %d attempts, want %d", got, SEND_ATTEMPTS)
	}
}

func TestSendMessageRetriesAfterTooManyRequests(t *testing.T) {
	b, tg := newFakeTelegram(t, 1,
		`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":0}}`)

	if err := sendMessage(b, 1, "hello"); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	if got := len(tg.texts()); got != 2 {
		t.Errorf("made %d attempts, want 2", got)
	}
}

func TestSendMessageGivesUpAndReportsTheError(t *testing.T) {
	b, tg := newFakeTelegram(t, 99, serverError)

	err := sendMessage(b, 1, "hello")
	if err == nil {
		t.Fatal("expected an error after every attempt failed")
	}
	if got := len(tg.texts()); got != SEND_ATTEMPTS {
		t.Errorf("made %d attempts, want exactly %d", got, SEND_ATTEMPTS)
	}
}

func TestSendMessageDeliversTextUnchanged(t *testing.T) {
	b, tg := newFakeTelegram(t, 0, serverError)

	text := "Тревога: Ağrı — שלום\n\nвторой абзац"
	if err := sendMessage(b, 1, text); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}

	got := tg.texts()
	if len(got) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(got))
	}
	if got[0] != text {
		t.Errorf("delivered %q, want %q", got[0], text)
	}
}

// A title has to survive the write/read round trip unchanged, otherwise the
// same alert looks new on every pass and is re-sent every hour.
func TestTitleRoundTripsThroughTheFile(t *testing.T) {
	dir := t.TempDir()
	titles := []string{
		"Security Alert:  Iran – July 24, 2026",
		"Weather Alert – Wildfires – U.S. Embassy Ankara, Türkiye (July 31, 2026)",
		"Тривога — Київ",
		"התרעת ביטחון",
	}

	for _, title := range titles {
		if err := saveFile(dir+"/x", title); err != nil {
			t.Fatalf("saveFile: %v", err)
		}
		got, err := readFile(dir + "/x")
		if err != nil {
			t.Fatalf("readFile: %v", err)
		}
		if got != title {
			t.Errorf("round trip changed %q into %q, which would re-send the alert hourly", title, got)
		}
	}
}

func servePage(t *testing.T, html string) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, html)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// List items used to be concatenated with no separator at all, producing
// "Have a plan.Monitor local media." in the delivered message.
func TestGetHtmlContentSeparatesListItems(t *testing.T) {
	userAgent = DEFAULT_USER_AGENT

	url := servePage(t, `<html><body>
		<div class="paragraph alignwide container"><div class="row">
			<p>Event: Wildfires are burning across the region.</p>
			<ul>
				<li>Have a plan.</li>
				<li>Monitor local media.</li>
				<li>Follow the instructions of local authorities.</li>
			</ul>
			<p>Actions to Take: stay alert.</p>
		</div></div>
	</body></html>`)

	got, err := getHtmlContent(url)
	if err != nil {
		t.Fatalf("getHtmlContent: %v", err)
	}

	want := "Event: Wildfires are burning across the region.\n\n" +
		"Have a plan.\nMonitor local media.\nFollow the instructions of local authorities.\n\n" +
		"Actions to Take: stay alert."
	if got != want {
		t.Errorf("content =\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(got, "plan.Monitor") {
		t.Error("list items are still glued together")
	}
}

func TestGetHtmlContentSkipsEmptyElements(t *testing.T) {
	userAgent = DEFAULT_USER_AGENT

	url := servePage(t, `<html><body>
		<div class="paragraph alignwide container"><div class="row">
			<p>   </p><p>Тривога у Києві.</p><p></p><p>Другий абзац.</p>
		</div></div>
	</body></html>`)

	got, err := getHtmlContent(url)
	if err != nil {
		t.Fatalf("getHtmlContent: %v", err)
	}
	if want := "Тривога у Києві.\n\nДругий абзац."; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}

// An alert page that does not match the content selector must report empty, so
// checkCountry retries instead of recording an alert it never delivered.
func TestGetHtmlContentReportsEmptyForUnknownLayout(t *testing.T) {
	userAgent = DEFAULT_USER_AGENT

	url := servePage(t, `<html><body><div class="something-else"><p>text</p></div></body></html>`)

	got, err := getHtmlContent(url)
	if err != nil {
		t.Fatalf("getHtmlContent: %v", err)
	}
	if got != "" {
		t.Errorf("content = %q, want empty", got)
	}
}

func TestGetLastAlertReadsTitleAndURL(t *testing.T) {
	userAgent = DEFAULT_USER_AGENT

	url := servePage(t, `<html><body><main>
		<article><h2 class="entry-title"><a href="https://x.usembassy.gov/newest/">Newest alert</a></h2></article>
		<article><h2 class="entry-title"><a href="https://x.usembassy.gov/older/">Older alert</a></h2></article>
	</main></body></html>`)

	alert, err := getLastAlert(url)
	if err != nil {
		t.Fatalf("getLastAlert: %v", err)
	}
	if alert.Title != "Newest alert" {
		t.Errorf("title = %q, want the first article's title", alert.Title)
	}
	if alert.URL != "https://x.usembassy.gov/newest/" {
		t.Errorf("url = %q", alert.URL)
	}
}

// The scraper used to return a zero Alert here and the caller would store an
// empty title, which reads back as "never seen anything" forever.
func TestGetLastAlertErrorsWhenNothingMatches(t *testing.T) {
	userAgent = DEFAULT_USER_AGENT

	url := servePage(t, `<html><body><main><p>no articles here</p></main></body></html>`)

	if _, err := getLastAlert(url); err == nil {
		t.Fatal("expected an error when the page has no alerts")
	}
}

// A missing titles file is the bootstrap signal, not an error.
func TestReadFileMissingIsEmpty(t *testing.T) {
	got, err := readFile(t.TempDir() + "/absent")
	if err != nil {
		t.Fatalf("readFile: %v", err)
	}
	if got != "" {
		t.Errorf("readFile = %q, want empty", got)
	}
}
