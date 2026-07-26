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
	IsVideo   bool

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
	postURL := "https://boosty.to/posts/" + post.ID

	for i, m := range post.Media {
		idx := i + 1
		var pfx string
		if isFlat {
			pfx = flatPrefix(post, idx)
		} else {
			pfx = fmt.Sprintf("%03d", idx)
		}

		switch m.Kind {
		case MediaImage:
			ext := imageExt(m.URL)
			var fname string
			if isFlat {
				fname = pfx + ext
			} else {
				fname = fmt.Sprintf("%s_%s%s", pfx, m.ID, ext)
			}
			tasks = append(tasks, DownloadTask{
				URL: m.URL, Dest: filepath.Join(destDir, fname), MediaType: "photo",
				PostID: post.ID, MediaID: m.ID,
			})

		case MediaVideo:
			dlURL := m.bestURL()
			if dlURL == "" {
				logWarn("Нет доступных URL для видео %s в посте %s", m.ID, post.ID)
				continue
			}
			title := truncateRunes(safeFilename(stripKnownExt(orDefault(m.Title, m.ID), ".mp4")), 100)
			var fname string
			if isFlat {
				fname = pfx + ".mp4"
			} else {
				fname = fmt.Sprintf("%s_%s.mp4", pfx, title)
			}
			tasks = append(tasks, DownloadTask{
				URL: dlURL, Dest: filepath.Join(destDir, fname), MediaType: "video",
				Referer: postURL, IsVideo: true,
				PostID: post.ID, MediaID: m.ID,
			})

		case MediaAudio, MediaFile:
			if post.SignedQuery == "" {
				logWarn("Нет signed_query для медиа в посте %s, пропускаем", post.ID)
				continue
			}
			dlURL := signURL(m.URL, post.SignedQuery)
			rawTitle := safeFilename(orDefault(m.Title, m.ID))

			if m.Kind == MediaAudio {
				title := truncateRunes(stripKnownExt(rawTitle, ".mp3"), 100)
				var fname string
				if isFlat {
					fname = pfx + ".mp3"
				} else {
					fname = fmt.Sprintf("%s_%s.mp3", pfx, title)
				}
				tasks = append(tasks, DownloadTask{
					URL: dlURL, Dest: filepath.Join(destDir, fname), MediaType: "audio",
					PostID: post.ID, MediaID: m.ID,
				})
			} else {
				ext := filepath.Ext(rawTitle)
				stem := truncateRunes(strings.TrimSuffix(rawTitle, ext), 100)
				var fname string
				if isFlat {
					fname = pfx + ext
				} else {
					fname = fmt.Sprintf("%s_%s%s", pfx, stem, ext)
				}
				tasks = append(tasks, DownloadTask{
					URL: dlURL, Dest: filepath.Join(destDir, fname), MediaType: "file",
					PostID: post.ID, MediaID: m.ID,
				})
			}
		}
	}

	return tasks
}
