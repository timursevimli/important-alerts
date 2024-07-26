package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/gocolly/colly/v2"
	"github.com/joho/godotenv"
)

const (
	DIR                   = "titles"
	MAX_MESSAGE_LENGTH    = 4096
	REPEAT_DELAY_IN_HOURS = 1
)

var chatIDs = map[string]int64{
	"ua": -1002127502421,
	"tr": -1002050212638,
	"il": -1002073228695,
	"ru": -1002116092146,
}

func main() {
	err := godotenv.Load()
	check(err)
	b := getBot()
	for {
		log.Println("Checking...")
		countries := getFilesInDirectory(DIR)
		for _, country := range countries {
			baseURL := "https://" + country + ".usembassy.gov"
			url := baseURL + "/category/alert/"
			log.Print("Checking: " + baseURL)
			alert := getLastAlert(url)
			fileName := DIR + "/" + country
			lastTitle := readFile(fileName)
			if alert.Title == lastTitle {
				log.Print("No new alerts: " + baseURL)
				continue
			}
			log.Print("New alert found : " + baseURL)
			content := getHtmlContent(alert.URL)
			chatID := chatIDs[country]
			if chatID == 0 {
				continue
			}
			if len(content) > MAX_MESSAGE_LENGTH {
				contentParts := split(content, MAX_MESSAGE_LENGTH)
				for _, contentPart := range contentParts {
					sendMessage(b, chatID, contentPart)
				}
			} else {
				sendMessage(b, chatID, content)
			}
			saveFile(fileName, alert.Title)
		}
		log.Println("Checking completed and sleeping...")
		time.Sleep(time.Hour * REPEAT_DELAY_IN_HOURS)
	}
}

func split(content string, size int) []string {
	var parts []string
	for len(content) > 0 {
		if len(content) <= size {
			parts = append(parts, content)
			break
		}
		parts = append(parts, content[:size])
		content = content[size:]
	}
	return parts
}

func check(e error) {
	if e != nil {
		panic(e)
	}
}

func getBot() *bot.Bot {
	token := os.Getenv("BOT_TOKEN")
	b, err := bot.New(token)
	check(err)
	return b
}

func sendMessage(b *bot.Bot, chatID int64, text string) {
	b.SendMessage(context.Background(), &bot.SendMessageParams{
		ChatID: chatID,
		Text:   strings.Trim(text, "\n\r"),
	})
}

func getFilesInDirectory(dirname string) []string {
	fileNames := []string{}
	files, err := filepath.Glob(filepath.Join(dirname, "*"))
	check(err)
	for _, filePath := range files {
		fileName := strings.ReplaceAll(filePath, dirname+"/", "")
		fileNames = append(fileNames, fileName)
	}
	return fileNames
}

func saveFile(fileName string, content string) {
	err := os.WriteFile(fileName, []byte(content), 0644)
	check(err)
}

func readFile(fileName string) string {
	file, err := os.ReadFile(fileName)
	check(err)
	return strings.TrimSpace(string(file))
}

func getUserAgent() string {
	return readFile("useragent")
}

type Alert struct {
	Title string
	URL   string
}

var userAgent = getUserAgent()

func getCollector() *colly.Collector {
	c := colly.NewCollector()
	c.SetRequestTimeout(120 * time.Second)

	c.OnRequest(func(r *colly.Request) {
		r.Headers.Set("User-Agent", userAgent)
	})

	c.OnError(func(_ *colly.Response, err error) {
		panic(err)
	})

	return c
}

func getLastAlert(url string) (alert Alert) {
	c := getCollector()

	var wg sync.WaitGroup

	c.OnHTML("#content article", func(e *colly.HTMLElement) {
		wg.Add(1)
		defer wg.Done()
		if alert != (Alert{}) {
			return
		}
		title := e.ChildText("h2.entry-title a")
		url := e.ChildAttr("h2.entry-title a", "href")
		alert = Alert{
			Title: title,
			URL:   url,
		}
	})

	c.Visit(url)
	wg.Wait()

	return alert
}

func getHtmlContent(url string) string {
	c := getCollector()

	var content string

	c.OnHTML(".entry-content", func(e *colly.HTMLElement) {
		content = e.Text
	})

	c.Visit(url)
	return content
}
