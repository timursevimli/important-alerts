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
	"sync/atomic"
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

	for _, limit := range []int{2, 3, 7, 100, 4096} {
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
	return s.state(t, country).Title
}

func (s *stub) state(t *testing.T, country string) deliveryState {
	t.Helper()
	state, err := readState(s.dir + "/" + country)
	if err != nil {
		t.Fatalf("readState: %v", err)
	}
	return state
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

func TestDeliverStopsAtTheFailingPart(t *testing.T) {
	s := newStub(t)
	s.failOn = 2

	content := strings.Repeat("абв ", 4000) // long enough to need several parts
	err := deliver(s.send, 1, s.dir+"/ua", deliveryState{Title: "Old alert"}, Alert{Title: "New alert"}, content)
	if err == nil {
		t.Fatal("expected deliver to report the failure")
	}
	if len(s.sent) != 2 {
		t.Fatalf("attempted %d parts, want it to stop at the failing one", len(s.sent))
	}

	// The one part that did arrive must be checkpointed, not repeated later.
	state := s.state(t, "ua")
	if state.Title != "Old alert" {
		t.Errorf("last delivered title = %q, want it unchanged", state.Title)
	}
	if state.Pending != "New alert" || state.Sent != 1 {
		t.Errorf("checkpoint = %+v, want pending %q with 1 part sent", state, "New alert")
	}
}

// The whole point of the checkpoint: a subscriber must never see a part twice
// because a later part failed.
func TestDeliverResumesWithoutRepeatingParts(t *testing.T) {
	s := newStub(t)
	s.setTitle(t, "ua", "Old alert")
	s.alert = Alert{Title: "New alert", URL: "https://ua.usembassy.gov/new"}
	s.content = "New alert\n\n" + strings.Repeat("Тривога у Києві. ", 1200)

	wantParts := split(s.content, MAX_MESSAGE_LENGTH)
	if len(wantParts) < 3 {
		t.Fatalf("this test needs a multi-part alert, got %d parts", len(wantParts))
	}

	// Pass 1 fails on the second part.
	s.failOn = 2
	if err := checkCountry(s.send, "ua", 1); err == nil {
		t.Fatal("expected the first pass to fail")
	}
	firstPass := append([]string(nil), s.sent...)

	// Pass 2 succeeds and must pick up where the first stopped.
	s.failOn = 0
	s.sent = nil
	if err := checkCountry(s.send, "ua", 1); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	delivered := append(firstPass[:len(firstPass)-1], s.sent...) // the failed part never arrived
	if len(delivered) != len(wantParts) {
		t.Fatalf("delivered %d parts, want %d", len(delivered), len(wantParts))
	}
	for i := range wantParts {
		if delivered[i] != wantParts[i] {
			t.Fatalf("part %d differs from the expected split", i)
		}
	}

	seen := map[string]int{}
	for _, part := range delivered {
		seen[part]++
	}
	for part, n := range seen {
		if n != 1 {
			t.Errorf("part %.40q was delivered %d times", part, n)
		}
	}

	if got := s.state(t, "ua"); got.Title != "New alert" || got.Pending != "" || got.Sent != 0 {
		t.Errorf("final state = %+v, want the alert committed and no pending delivery", got)
	}
}

// A page that is rewritten between passes invalidates the offset, otherwise
// resuming would skip the wrong piece of the new text.
func TestDeliverRestartsWhenTheContentChanged(t *testing.T) {
	s := newStub(t)
	original := strings.Repeat("Тривога у Києві. ", 1200)
	state := deliveryState{
		Title:   "Old alert",
		Pending: "New alert",
		Digest:  contentDigest(original),
		Sent:    2,
	}

	rewritten := strings.Repeat("Оновлена тривога. ", 1200)
	if err := deliver(s.send, 1, s.dir+"/ua", state, Alert{Title: "New alert"}, rewritten); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	want := split(rewritten, MAX_MESSAGE_LENGTH)
	if len(s.sent) != len(want) {
		t.Fatalf("delivered %d parts, want all %d of the rewritten alert", len(s.sent), len(want))
	}
}

func TestDeliverResumesAfterAFailedCheckpointWrite(t *testing.T) {
	s := newStub(t)
	content := "New alert\n\nbody"
	fileName := s.dir + "/ua"

	// The commit write failed last pass, so the checkpoint says everything was
	// already sent. Nothing may be delivered again.
	state := deliveryState{
		Title:   "Old alert",
		Pending: "New alert",
		Digest:  contentDigest(content),
		Sent:    len(split(content, MAX_MESSAGE_LENGTH)),
	}
	if err := deliver(s.send, 1, fileName, state, Alert{Title: "New alert"}, content); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if len(s.sent) != 0 {
		t.Errorf("re-delivered %d parts, want none", len(s.sent))
	}
	if got := s.state(t, "ua"); got.Title != "New alert" || got.Pending != "" {
		t.Errorf("state = %+v, want the alert committed", got)
	}
}

// Files written before delivery tracking existed hold a bare title, and an
// upgrade must not treat the current alert as new.
func TestReadStateAcceptsTheLegacyBareTitle(t *testing.T) {
	dir := t.TempDir()
	if err := saveFile(dir+"/ua", "Security Alert – U.S. Embassy Moscow, Russia (June 18, 2026)"); err != nil {
		t.Fatal(err)
	}

	state, err := readState(dir + "/ua")
	if err != nil {
		t.Fatalf("readState: %v", err)
	}
	if state.Title != "Security Alert – U.S. Embassy Moscow, Russia (June 18, 2026)" {
		t.Errorf("title = %q", state.Title)
	}
	if state.Pending != "" || state.Sent != 0 {
		t.Errorf("legacy file produced a pending delivery: %+v", state)
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := deliveryState{Title: "Старе", Pending: "Нове", Digest: contentDigest("body"), Sent: 3}

	if err := saveState(dir+"/ua", want); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	got, err := readState(dir + "/ua")
	if err != nil {
		t.Fatalf("readState: %v", err)
	}
	if got != want {
		t.Errorf("state = %+v, want %+v", got, want)
	}
}

func TestReadStateReportsCorruption(t *testing.T) {
	dir := t.TempDir()
	if err := saveFile(dir+"/ua", `{"title": "unterminated`); err != nil {
		t.Fatal(err)
	}
	if _, err := readState(dir + "/ua"); err == nil {
		t.Fatal("expected corrupt state to be reported rather than read as a title")
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
	received []string // texts Telegram accepted
	attempts int      // requests made, including the rejected ones
	fail     func(attempt int, text string) bool
	response string
}

func newFakeTelegram(t *testing.T, failures int, response string) (*bot.Bot, *fakeTelegram) {
	t.Helper()
	return newFailingTelegram(t, response, func(attempt int, _ string) bool {
		return attempt < failures
	})
}

func newFailingTelegram(t *testing.T, response string, fail func(attempt int, text string) bool) (*bot.Bot, *fakeTelegram) {
	t.Helper()

	f := &fakeTelegram{fail: fail, response: response}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		text := telegramText(t, r)

		f.mu.Lock()
		rejected := f.fail(f.attempts, text)
		f.attempts++
		if !rejected {
			f.received = append(f.received, text)
		}
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if rejected {
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

func (f *fakeTelegram) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

const serverError = `{"ok":false,"error_code":500,"description":"Internal Server Error"}`

// Retrying inside the pass keeps a hiccup on one part from forcing the whole
// alert to be re-sent an hour later.
func TestSendMessageRetriesTransientFailures(t *testing.T) {
	b, tg := newFakeTelegram(t, SEND_ATTEMPTS-1, serverError)

	if err := sendMessage(b, 1, "hello"); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	if got := tg.attemptCount(); got != SEND_ATTEMPTS {
		t.Errorf("made %d attempts, want %d", got, SEND_ATTEMPTS)
	}
	if got := len(tg.texts()); got != 1 {
		t.Errorf("delivered %d messages, want exactly 1", got)
	}
}

func TestSendMessageRetriesAfterTooManyRequests(t *testing.T) {
	b, tg := newFakeTelegram(t, 1,
		`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":0}}`)

	if err := sendMessage(b, 1, "hello"); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	if got := tg.attemptCount(); got != 2 {
		t.Errorf("made %d attempts, want 2", got)
	}
	if got := len(tg.texts()); got != 1 {
		t.Errorf("delivered %d messages, want exactly 1", got)
	}
}

func TestSendMessageGivesUpAndReportsTheError(t *testing.T) {
	b, tg := newFakeTelegram(t, 99, serverError)

	err := sendMessage(b, 1, "hello")
	if err == nil {
		t.Fatal("expected an error after every attempt failed")
	}
	if got := tg.attemptCount(); got != SEND_ATTEMPTS {
		t.Errorf("made %d attempts, want exactly %d", got, SEND_ATTEMPTS)
	}
	if got := len(tg.texts()); got != 0 {
		t.Errorf("delivered %d messages, want none", got)
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

// The whole pipeline on a realistic multi-part alert: the real scraper against
// a fake embassy site, the real sendMessage against a fake Telegram API, an
// outage part way through the delivery, and a resume that repeats nothing.
func TestMultiPartAlertIsDeliveredExactlyOnceAcrossPasses(t *testing.T) {
	userAgent = DEFAULT_USER_AGENT

	title := "Security Alert – U.S. Embassy Kyiv, Ukraine (August 6, 2026)"
	paragraph := strings.Repeat("Громадянам США рекомендується зберігати пильність та стежити за повідомленнями місцевої влади. ", 6)

	var body strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&body, "<p>%d. %s</p>", i, paragraph)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/category/alert/" {
			fmt.Fprintf(w, `<html><body><main><article><h2 class="entry-title">`+
				`<a href="/alert/1">%s</a></h2></article></main></body></html>`, title)
			return
		}
		fmt.Fprintf(w, `<html><body><div class="paragraph alignwide container">`+
			`<div class="row">%s</div></div></body></html>`, body.String())
	}))
	t.Cleanup(srv.Close)

	oldDir, oldAlert, oldContent := titlesDir, fetchAlert, fetchContent
	t.Cleanup(func() { titlesDir, fetchAlert, fetchContent = oldDir, oldAlert, oldContent })
	titlesDir = t.TempDir()
	fetchAlert = func(string) (Alert, error) {
		alert, err := getLastAlert(srv.URL + "/category/alert/")
		alert.URL = srv.URL + alert.URL // the fake site serves a relative href
		return alert, err
	}
	fetchContent = getHtmlContent

	// A file in the pre-upgrade bare-title format, holding an older alert.
	if err := saveFile(titlesDir+"/ua", "An older alert"); err != nil {
		t.Fatal(err)
	}

	content, err := getHtmlContent(srv.URL + "/alert/1")
	if err != nil {
		t.Fatalf("getHtmlContent: %v", err)
	}
	if !strings.Contains(content, title) {
		content = title + "\n\n" + content
	}
	wantParts := split(content, MAX_MESSAGE_LENGTH)
	if len(wantParts) < 3 {
		t.Fatalf("this test needs a multi-part alert, got %d part(s) from %d units",
			len(wantParts), utf16Len(content))
	}

	var failing atomic.Value
	failing.Store("")
	b, tg := newFailingTelegram(t, serverError, func(_ int, text string) bool {
		return text == failing.Load().(string)
	})
	send := func(chatID int64, text string) error { return sendMessage(b, chatID, text) }

	// Pass 1: Telegram rejects the second part on every attempt.
	failing.Store(wantParts[1])
	if err := checkCountry(send, "ua", 1); err == nil {
		t.Fatal("expected the interrupted pass to report an error")
	}
	if got := len(tg.texts()); got != 1 {
		t.Fatalf("interrupted pass delivered %d parts, want 1", got)
	}
	if state := mustReadState(t, titlesDir+"/ua"); state.Title != "An older alert" || state.Sent != 1 {
		t.Fatalf("checkpoint = %+v, want the older title with 1 part sent", state)
	}

	// Pass 2: the outage is over and delivery resumes.
	failing.Store("")
	if err := checkCountry(send, "ua", 1); err != nil {
		t.Fatalf("resumed pass: %v", err)
	}

	delivered := tg.texts()
	if len(delivered) != len(wantParts) {
		t.Fatalf("delivered %d messages in total, want %d", len(delivered), len(wantParts))
	}
	for i, want := range wantParts {
		if delivered[i] != want {
			t.Errorf("message %d differs from the expected split", i)
		}
		if n := utf16Len(want); n > MAX_MESSAGE_LENGTH {
			t.Errorf("part %d is %d UTF-16 units, Telegram allows %d", i, n, MAX_MESSAGE_LENGTH)
		}
	}
	if tg.attemptCount() <= len(wantParts) {
		t.Errorf("made %d attempts, expected retries of the rejected part", tg.attemptCount())
	}

	state := mustReadState(t, titlesDir+"/ua")
	if state.Title != title || state.Pending != "" || state.Sent != 0 {
		t.Errorf("final state = %+v, want %q committed with nothing pending", state, title)
	}

	t.Logf("%d parts, %d units, %d Telegram calls", len(wantParts), utf16Len(content), tg.attemptCount())
}

// Embassies reuse headlines. "Security Alert: U.S. Embassy Kyiv, Ukraine"
// covered two unrelated alerts a month apart; comparing titles alone dropped
// the second one entirely and silently.
func TestSameTitleAtADifferentURLIsStillDelivered(t *testing.T) {
	s := newStub(t)
	s.alert = Alert{Title: "Security Alert: U.S. Embassy Kyiv, Ukraine", URL: "https://ua.usembassy.gov/may-9-2025/"}
	s.content = "A potentially significant air attack may occur."

	// Bootstrap records the first alert without sending it.
	if err := checkCountry(s.send, "ua", 1); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// A different article published later under the identical headline.
	s.alert.URL = "https://ua.usembassy.gov/june-6-2025/"
	s.content = "A significant missile and drone attack struck sites across Ukraine."
	if err := checkCountry(s.send, "ua", 1); err != nil {
		t.Fatalf("second alert: %v", err)
	}
	if len(s.sent) != 1 {
		t.Fatalf("delivered %d messages for the second alert, want 1", len(s.sent))
	}
	if !strings.Contains(s.sent[0], "missile and drone") {
		t.Errorf("delivered the wrong alert: %.60q", s.sent[0])
	}

	// The very same article must still not be re-sent.
	s.sent = nil
	if err := checkCountry(s.send, "ua", 1); err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if len(s.sent) != 0 {
		t.Errorf("re-sent %d messages for an unchanged alert", len(s.sent))
	}
}

// Upgrading from a bare-title file leaves no URL to compare, and the current
// alert must not be re-broadcast because of that.
func TestLegacyStateWithoutURLDoesNotResend(t *testing.T) {
	s := newStub(t)
	s.setTitle(t, "ru", "Security Alert – U.S. Embassy Moscow, Russia (June 18, 2026)")
	s.alert = Alert{
		Title: "Security Alert – U.S. Embassy Moscow, Russia (June 18, 2026)",
		URL:   "https://ru.usembassy.gov/security-alert-june-18-2026/",
	}
	s.content = "body"

	if err := checkCountry(s.send, "ru", 1); err != nil {
		t.Fatalf("checkCountry: %v", err)
	}
	if len(s.sent) != 0 {
		t.Fatalf("re-sent %d messages after the upgrade, want none", len(s.sent))
	}
}

// A title carrying invalid UTF-8 used to be mangled by json.Marshal, so the
// stored value never matched the scraped one and the alert was re-broadcast
// every single hour.
func TestInvalidUTF8InTitleDoesNotLoopForever(t *testing.T) {
	s := newStub(t)
	s.alert = Alert{
		Title: "Security Alert \x96 U.S. Embassy Ankara", // Windows-1252 en dash
		URL:   "https://tr.usembassy.gov/alert/",
	}
	s.content = "body of the alert"

	if err := checkCountry(s.send, "tr", 1); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	for pass := 2; pass <= 4; pass++ {
		if err := checkCountry(s.send, "tr", 1); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	if len(s.sent) != 0 {
		t.Fatalf("re-delivered the same alert %d times across three passes", len(s.sent))
	}
	if !utf8.ValidString(s.state(t, "tr").Title) {
		t.Error("stored title is not valid UTF-8")
	}
}

func TestInvalidUTF8InContentIsSanitised(t *testing.T) {
	s := newStub(t)
	s.setTitle(t, "il", "Old alert")
	s.alert = Alert{Title: "New alert", URL: "https://il.usembassy.gov/new/"}
	s.content = "New alert\n\nbody with a bad byte \xff here"

	if err := checkCountry(s.send, "il", 1); err != nil {
		t.Fatalf("checkCountry: %v", err)
	}
	if len(s.sent) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(s.sent))
	}
	if !utf8.ValidString(s.sent[0]) {
		t.Error("delivered a message that Telegram would reject as invalid UTF-8")
	}
}

// A hand-edited or corrupted offset must not index outside the parts slice.
func TestDeliverIgnoresAnOutOfRangeOffset(t *testing.T) {
	content := "New alert\n\nbody"
	alert := Alert{Title: "New alert", URL: "https://x/new/"}

	for _, sent := range []int{-1, 99} {
		s := newStub(t)
		state := deliveryState{
			Title:   "Old alert",
			Pending: alert.Title,
			Digest:  contentDigest(content),
			Sent:    sent,
		}
		if err := deliver(s.send, 1, s.dir+"/ua", state, alert, content); err != nil {
			t.Fatalf("sent=%d: %v", sent, err)
		}
		if len(s.sent) != 1 {
			t.Errorf("sent=%d: delivered %d messages, want the alert delivered once", sent, len(s.sent))
		}
	}
}

func mustReadState(t *testing.T, fileName string) deliveryState {
	t.Helper()
	state, err := readState(fileName)
	if err != nil {
		t.Fatalf("readState: %v", err)
	}
	return state
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
