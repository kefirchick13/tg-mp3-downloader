package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kkdai/youtube/v2"
	tgbotapi "gopkg.in/telegram-bot-api.v4"
)

const (
	tempDir         = "tmp"
	maxDownloadSize = 50 * 1024 * 1024 // 50MB
)

var (
	platformChoice = make(map[int]string) // userID -> platform
	mu             sync.Mutex
)

func main() {
	botToken := os.Getenv("TELEGRAM_TOKEN")
	if botToken == "" {
		log.Fatal("TELEGRAM_TOKEN not set")
	}

	// Создаем временную директорию
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		log.Fatalf("Error creating temp dir: %v", err)
	}

	// Инициализация бота
	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Panic(err)
	}

	bot.Debug = true
	log.Printf("Authorized on account %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates, _ := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.Message == nil {
			continue
		}

		msg := tgbotapi.NewMessage(update.Message.Chat.ID, "")
		userID := update.Message.From.ID

		// Обработка команд
		if update.Message.IsCommand() {
			switch update.Message.Command() {
			case "start":
				msg.Text = "Привет! Отправь мне ссылку на трек из YouTube или SoundCloud, и я скачаю его для тебя."
				bot.Send(msg)
				continue
			}
		}

		// Обработка текстовых сообщений
		text := update.Message.Text
		if text == "" {
			continue
		}

		// Определение платформы
		if isYouTubeLink(text) || isSoundCloudLink(text) {
			// Показать клавиатуру выбора
			msg.Text = "Выбери платформу для скачивания:"
			msg.ReplyMarkup = createPlatformKeyboard()
			bot.Send(msg)

			// Сохранить ссылку для будущей обработки
			mu.Lock()
			platformChoice[userID] = text
			mu.Unlock()
			continue
		}

		// Обработка выбора платформы
		if text == "YouTube" || text == "SoundCloud" {
			mu.Lock()
			link, ok := platformChoice[userID]
			mu.Unlock()

			if !ok {
				msg.Text = "Сначала отправь мне ссылку на трек"
				bot.Send(msg)
				continue
			}

			// Удалить состояние
			mu.Lock()
			delete(platformChoice, userID)
			mu.Unlock()

			// Запуск обработки в горутине
			go func(chatID int64, platform, url string) {
				var filePath string
				var err error

				switch platform {
				case "YouTube":
					filePath, err = downloadYouTube(url)
				case "SoundCloud":
					filePath, err = downloadSoundCloud(url)
				default:
					err = fmt.Errorf("неподдерживаемая платформа")
				}

				if err != nil {
					errorMsg := tgbotapi.NewMessage(chatID, "❌ Ошибка: "+err.Error())
					bot.Send(errorMsg)
					return
				}

				// Отправка файла
				if err := sendAudioFile(bot, chatID, filePath); err != nil {
					log.Printf("Error sending file: %v", err)
				}

				// Удаление временного файла
				os.Remove(filePath)
			}(update.Message.Chat.ID, text, link)
		} else {
			msg.Text = "Отправь мне ссылку на YouTube или SoundCloud трек"
			bot.Send(msg)
		}
	}
}

// Создание клавиатуры выбора платформы
func createPlatformKeyboard() tgbotapi.ReplyKeyboardMarkup {
	return tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("YouTube"),
			tgbotapi.NewKeyboardButton("SoundCloud"),
		),
	)
}

// Проверка YouTube ссылки
func isYouTubeLink(text string) bool {
	return strings.Contains(text, "youtube.com/") ||
		strings.Contains(text, "youtu.be/")
}

// Проверка SoundCloud ссылки
func isSoundCloudLink(text string) bool {
	return strings.Contains(text, "soundcloud.com/")
}

// Загрузка с YouTube
func downloadYouTube(url string) (string, error) {
	client := youtube.Client{}
	video, err := client.GetVideo(url)
	if err != nil {
		return "", fmt.Errorf("ошибка получения видео: %v", err)
	}

	// Ищем формат с лучшим аудио
	var format *youtube.Format
	for _, f := range video.Formats {
		if f.AudioChannels > 0 && (format == nil || f.Bitrate > format.Bitrate) {
			format = &f
		}
	}

	if format == nil {
		return "", fmt.Errorf("аудио поток не найден")
	}

	// Создаем временный файл
	filePath := filepath.Join(tempDir, sanitizeFileName(video.Title)+".mp3")

	// Скачиваем и конвертируем
	if err := downloadAndConvertYouTube(client, video, format, filePath); err != nil {
		return "", err
	}

	return filePath, nil
}

// Скачивание и конвертация YouTube аудио
func downloadAndConvertYouTube(client youtube.Client, video *youtube.Video, format *youtube.Format, outputPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	stream, _, err := client.GetStreamContext(ctx, video, format)
	if err != nil {
		return err
	}
	defer stream.Close()

	// Создание временного файла
	tempFile, err := os.CreateTemp(tempDir, "youtube_*.m4a")
	if err != nil {
		return err
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	// Скачивание
	if _, err := io.Copy(tempFile, stream); err != nil {
		return err
	}

	// Конвертация в MP3
	cmd := exec.Command("ffmpeg", "-i", tempFile.Name(), "-codec:a", "libmp3lame", "-q:a", "0", outputPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ошибка конвертации: %v", err)
	}

	return nil
}

// Загрузка с SoundCloud
func downloadSoundCloud(url string) (string, error) {
	// Проверка доступности скачивания
	downloadURL, title, err := getSoundCloudDownloadURL(url)
	if err != nil {
		return "", err
	}

	// Создание файла
	filePath := filepath.Join(tempDir, sanitizeFileName(title)+".mp3")
	file, err := os.Create(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	// Скачивание файла
	resp, err := http.Get(downloadURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Проверка размера файла
	if resp.ContentLength > maxDownloadSize {
		return "", fmt.Errorf("файл слишком большой (%dMB)", resp.ContentLength/1024/1024)
	}

	// Копирование данных
	if _, err := io.Copy(file, resp.Body); err != nil {
		return "", err
	}

	return filePath, nil
}

// Получение ссылки на скачивание SoundCloud
func getSoundCloudDownloadURL(pageURL string) (string, string, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err := client.Get(pageURL)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("страница не найдена (код %d)", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}

	content := string(body)

	// Поиск URL для скачивания
	re := regexp.MustCompile(`"download_url":"([^"]+)"`)
	matches := re.FindStringSubmatch(content)
	if len(matches) < 2 {
		return "", "", fmt.Errorf("скачивание недоступно для этого трека")
	}
	downloadURL := strings.ReplaceAll(matches[1], "\\u0026", "&")

	// Извлечение названия трека
	titleRe := regexp.MustCompile(`"title":"([^"]+)"`)
	titleMatches := titleRe.FindStringSubmatch(content)
	if len(titleMatches) < 2 {
		return downloadURL, "soundcloud_track", nil
	}

	return downloadURL, titleMatches[1], nil
}

// Отправка аудиофайла
func sendAudioFile(bot *tgbotapi.BotAPI, chatID int64, filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	fileInfo, _ := file.Stat()
	if fileInfo.Size() > 50*1024*1024 {
		return fmt.Errorf("файл слишком большой для Telegram (%.2fMB)", float64(fileInfo.Size())/1024/1024)
	}

	audio := tgbotapi.NewAudioUpload(chatID, tgbotapi.FileReader{
		Name:   filepath.Base(filePath),
		Reader: file,
	})

	_, err = bot.Send(audio)
	return err
}

// Санитизация имени файла
func sanitizeFileName(name string) string {
	re := regexp.MustCompile(`[<>:"/\\|?*]`)
	return re.ReplaceAllString(name, "_")
}
