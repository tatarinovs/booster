package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const (
	failedFilename  = "failed.txt"
	contentFilename = "content.txt"
)

// DownloadTask описывает одну задачу скачивания файла.
type DownloadTask struct {
	URL       string
	Dest      string
	MediaType string // "photo" | "video" | "audio" | "file"
	Referer   string
	IsVideo   bool
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
			var fname string
			if isFlat {
				fname = pfx + ".jpg"
			} else {
				fname = fmt.Sprintf("%s_%s.jpg", pfx, m.ID)
			}
			tasks = append(tasks, DownloadTask{
				URL: m.URL, Dest: filepath.Join(destDir, fname), MediaType: "photo",
			})

		case MediaVideo:
			url := m.bestURL()
			if url == "" {
				logWarn("Нет доступных URL для видео %s в посте %s", m.ID, post.ID)
				continue
			}
			cleanTitle := safeFilename(orDefault(m.Title, m.ID))
			if strings.HasSuffix(strings.ToLower(cleanTitle), ".mp4") {
				cleanTitle = cleanTitle[:len(cleanTitle)-4]
			}
			title := truncateRunes(cleanTitle, 100)
			var fname string
			if isFlat {
				fname = pfx + ".mp4"
			} else {
				fname = fmt.Sprintf("%s_%s.mp4", pfx, title)
			}
			tasks = append(tasks, DownloadTask{
				URL: url, Dest: filepath.Join(destDir, fname), MediaType: "video",
				Referer: postURL, IsVideo: true,
			})

		case MediaAudio, MediaFile:
			if post.SignedQuery == "" {
				logWarn("Нет signed_query для медиа в посте %s, пропускаем", post.ID)
				continue
			}
			url := signURL(m.URL, post.SignedQuery)
			cleanTitle := safeFilename(orDefault(m.Title, m.ID))

			if m.Kind == MediaAudio {
				title := cleanTitle
				if strings.HasSuffix(strings.ToLower(cleanTitle), ".mp3") {
					title = truncateRunes(cleanTitle[:len(cleanTitle)-4], 100)
				} else {
					title = truncateRunes(cleanTitle, 100)
				}
				var fname string
				if isFlat {
					fname = pfx + ".mp3"
				} else {
					fname = fmt.Sprintf("%s_%s.mp3", pfx, title)
				}
				tasks = append(tasks, DownloadTask{
					URL: url, Dest: filepath.Join(destDir, fname), MediaType: "audio",
				})
			} else {
				ext := filepath.Ext(cleanTitle)
				stem := truncateRunes(strings.TrimSuffix(cleanTitle, ext), 100)
				var fname string
				if isFlat {
					fname = pfx + ext
				} else {
					fname = fmt.Sprintf("%s_%s%s", pfx, stem, ext)
				}
				tasks = append(tasks, DownloadTask{
					URL: url, Dest: filepath.Join(destDir, fname), MediaType: "file",
				})
			}
		}
	}

	return tasks
}
