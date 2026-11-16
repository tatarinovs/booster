package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	failedFilename   = "failed.txt"
	contentFilename  = "content.md"
	externalFilename = "external_links.txt"
)

// externalLog накапливает ссылки на внешние видео (YouTube/Vimeo), которые
// скачать нельзя. Раньше такие блоки отбрасывались молча, и пользователь
// считал пост сохранённым целиком.
//
// Файл дополняется, а не перезаписывается: быстрая синхронизация видит только
// новые посты, поэтому очистка стёрла бы всю ранее собранную историю. От
// повторов защищает набор уже записанных ссылок.
type externalLog struct {
	path string
	seen map[string]bool
	n    int
}

func newExternalLog(path string) *externalLog {
	e := &externalLog{path: path, seen: map[string]bool{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return e
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) > 0 && fields[len(fields)-1] != "" {
			e.seen[fields[len(fields)-1]] = true
		}
	}
	return e
}

// add записывает ссылку, если её ещё нет. Возвращает true, если запись новая.
func (e *externalLog) add(postID, title, link string) bool {
	if link == "" || e.seen[link] {
		return false
	}
	e.seen[link] = true

	f, err := os.OpenFile(e.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		logError("Не удалось записать в %s: %v", e.path, err)
		return false
	}
	defer f.Close()
	// Таб-разделённые поля: ссылка последняя, чтобы её было удобно вырезать
	// для yt-dlp (например: cut -f3 external_links.txt | yt-dlp -a -).
	fmt.Fprintf(f, "https://boosty.to/posts/%s\t%s\t%s\n",
		postID, strings.ReplaceAll(title, "\t", " "), link)
	e.n++
	return true
}

// DownloadTask описывает одну задачу скачивания файла.
type DownloadTask struct {
	URL       string
	Dest      string
	MediaType string // "photo" | "video" | "audio" | "file"
	Referer   string

	// PostID и MediaID позволяют перезапросить пост и получить свежую ссылку,
	// если подписанная ссылка протухла, пока задача ждала в очереди.
	PostID  string
	MediaID string

	// Resolve вычисляет ссылку в момент скачивания. Нужен для внешних галерей,
	// где прямая ссылка выдаётся отдельным запросом и быстро протухает: считать
	// её заранее для всей очереди бессмысленно. Он же служит способом обновить
	// ссылку, если сервер ответил 403.
	Resolve func(context.Context) (string, error)

	// AfterDownload вызывается после успешного сохранения файла — например,
	// чтобы распаковать скачанный архив галереи.
	AfterDownload func(path string) error
}

func postDate(p *Post) string {
	return time.Unix(p.PublishTime, 0).UTC().Format("2006-01-02")
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// postDirName возвращает имя папки поста (не-плоский режим).
func postDirName(base string, post *Post) string {
	title := truncateRunes(safeFilename(orDefault(post.Title, "post")), 100)
	return filepath.Join(base, fmt.Sprintf("%s_%s_%s", postDate(post), title, post.ID))
}

// flatPrefix возвращает префикс имени файла для плоского режима.
func flatPrefix(post *Post, index int) string {
	title := truncateRunes(safeFilename(orDefault(post.Title, "post")), 100)
	return fmt.Sprintf("%s_%s_%03d", postDate(post), title, index)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// knownImageExts — расширения, которым можно доверять в URL картинки.
var knownImageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true,
	".gif": true, ".webp": true, ".bmp": true,
}

// imageExt определяет расширение картинки по URL. Раньше всё сохранялось как
// .jpg, из-за чего PNG и GIF получали неверное расширение.
func imageExt(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ".jpg"
	}
	ext := strings.ToLower(filepath.Ext(parsed.Path))
	if knownImageExts[ext] {
		return ext
	}
	return ".jpg"
}

// stripKnownExt обрезает расширение из имени файла, если оно совпадает.
func stripKnownExt(name, ext string) string {
	if strings.HasSuffix(strings.ToLower(name), ext) {
		return name[:len(name)-len(ext)]
	}
	return name
}

// makeTasks строит список задач загрузки для поста.
func makeTasks(post *Post, destDir string, isFlat bool) []DownloadTask {
	var tasks []DownloadTask
	for i, m := range post.Media {
		var ext, stem, mediaType, referer string
		switch m.Kind {
		case MediaImage:
			ext, stem, mediaType = imageExt(m.URL), m.ID, "photo"
		case MediaVideo:
			ext, mediaType = ".mp4", "video"
			stem = truncateRunes(safeFilename(stripKnownExt(orDefault(m.Title, m.ID), ext)), 100)
			referer = "https://boosty.to/posts/" + post.ID
		case MediaAudio:
			ext, mediaType = ".mp3", "audio"
			stem = truncateRunes(stripKnownExt(safeFilename(orDefault(m.Title, m.ID)), ext), 100)
		case MediaFile:
			title := safeFilename(orDefault(m.Title, m.ID))
			ext, mediaType = filepath.Ext(title), "file"
			stem = truncateRunes(strings.TrimSuffix(title, ext), 100)
		default:
			continue
		}
		dlURL := mediaURL(post, &m)
		if dlURL == "" {
			logWarn("Нет доступной ссылки для вложения %s в посте %s", m.ID, post.ID)
			continue
		}
		fname := fmt.Sprintf("%03d_%s%s", i+1, stem, ext)
		if isFlat {
			fname = flatPrefix(post, i+1) + ext
		}
		tasks = append(tasks, DownloadTask{
			URL: dlURL, Dest: filepath.Join(destDir, fname), MediaType: mediaType,
			Referer: referer, PostID: post.ID, MediaID: m.ID,
		})
	}
	return tasks
}

// savePostText сохраняет текст через временный файл: неудачная запись
// не должна оставлять обрезанный content.md, который пропустит следующий запуск.
func savePostText(path string, post *Post) error {
	info, err := os.Stat(path)
	if err == nil {
		if info.IsDir() {
			return fmt.Errorf("путь текста занят каталогом: %s", path)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	text := postToMarkdown(post.TextBlocks)
	if text == "" {
		return nil
	}
	pubDate := time.Unix(post.PublishTime, 0).UTC().Format("02.01.2006 15:04 UTC")
	tmp := path + ".part"
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("Published %s\n\n%s", pubDate, text)), 0o644); err != nil {
		return err
	}
	return safeReplace(tmp, path)
}
