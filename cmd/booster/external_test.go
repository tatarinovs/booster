package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMediaExternalVideo(t *testing.T) {
	m := parseMedia(map[string]any{
		"type":  "external_video",
		"id":    "ext-1",
		"url":   "https://youtube.com/watch?v=abc",
		"title": "Стрим",
	})
	if m == nil {
		t.Fatal("external_video отброшен — раньше это молча теряло контент")
	}
	if m.Kind != MediaExternal {
		t.Errorf("Kind = %v, ожидалось MediaExternal", m.Kind)
	}
	if m.URL != "https://youtube.com/watch?v=abc" {
		t.Errorf("URL = %q", m.URL)
	}

	// Без ссылки записывать нечего.
	if got := parseMedia(map[string]any{"type": "external_video", "id": "x"}); got != nil {
		t.Errorf("external_video без url = %+v, ожидался nil", got)
	}
	// Тизеры по-прежнему пропускаются.
	if got := parseMedia(map[string]any{
		"type": "external_video", "url": "https://youtube.com/x", "isTeaser": true,
	}); got != nil {
		t.Errorf("тизер = %+v, ожидался nil", got)
	}
	// Неизвестный тип по-прежнему пропускается.
	if got := parseMedia(map[string]any{"type": "какой-то_новый_тип", "id": "z"}); got != nil {
		t.Errorf("неизвестный тип = %+v, ожидался nil", got)
	}
}

func TestExternalLogDeduplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), externalFilename)

	e := newExternalLog(path)
	if !e.add("post1", "Первое", "https://youtube.com/a") {
		t.Error("первая ссылка должна записаться")
	}
	if e.add("post1", "Первое", "https://youtube.com/a") {
		t.Error("повтор в том же запуске не должен записываться")
	}
	if !e.add("post2", "Второе", "https://youtube.com/b") {
		t.Error("вторая ссылка должна записаться")
	}
	if e.add("post3", "Пустое", "") {
		t.Error("пустая ссылка не должна записываться")
	}
	if e.n != 2 {
		t.Errorf("n = %d, ожидалось 2", e.n)
	}

	// Новый запуск должен подхватить уже записанное и не продублировать его.
	// Быстрая синхронизация видит только новые посты, поэтому файл дополняется.
	e2 := newExternalLog(path)
	if e2.add("post1", "Первое", "https://youtube.com/a") {
		t.Error("ссылка из прошлого запуска записана повторно")
	}
	if !e2.add("post9", "Новое", "https://youtube.com/c") {
		t.Error("новая ссылка должна записаться")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("файл не прочитан: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("строк в файле: %d, ожидалось 3.\n%s", len(lines), data)
	}
	// Ссылка — последнее поле, чтобы её можно было вырезать через cut для yt-dlp.
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			t.Errorf("строка %q: полей %d, ожидалось 3", line, len(fields))
			continue
		}
		if !strings.HasPrefix(fields[2], "https://youtube.com/") {
			t.Errorf("последнее поле %q не является ссылкой", fields[2])
		}
	}
}

func TestExternalLogHandlesTabsInTitle(t *testing.T) {
	path := filepath.Join(t.TempDir(), externalFilename)
	e := newExternalLog(path)
	e.add("p1", "Заголовок\tс\tтабами", "https://youtube.com/a")

	data, _ := os.ReadFile(path)
	if n := strings.Count(strings.TrimSpace(string(data)), "\t"); n != 2 {
		t.Errorf("табов в строке: %d, ожидалось 2 (табы в заголовке ломают разбор)", n)
	}
}

func TestExternalLogMissingFile(t *testing.T) {
	// Отсутствующий файл — это нормальный первый запуск, а не ошибка.
	e := newExternalLog(filepath.Join(t.TempDir(), "нет-такого.txt"))
	if len(e.seen) != 0 {
		t.Errorf("seen = %v, ожидался пустой набор", e.seen)
	}
}

func TestMediaURL(t *testing.T) {
	post := &Post{SignedQuery: "?sign=abc&ts=1"}

	video := &MediaItem{Kind: MediaVideo, URLs: map[string]string{"full_hd": "https://cdn/v.mp4"}}
	if got := mediaURL(post, video); got != "https://cdn/v.mp4" {
		t.Errorf("видео: %q", got)
	}

	audio := &MediaItem{Kind: MediaAudio, URL: "https://cdn/a.mp3"}
	got := mediaURL(post, audio)
	if !strings.Contains(got, "sign=abc") || !strings.Contains(got, "ts=1") {
		t.Errorf("аудио: %q — подпись не добавлена", got)
	}

	// Без signedQuery ссылку отдавать нельзя: она заведомо не откроется.
	if got := mediaURL(&Post{}, audio); got != "" {
		t.Errorf("аудио без подписи = %q, ожидалась пустая строка", got)
	}

	img := &MediaItem{Kind: MediaImage, URL: "https://cdn/i.png"}
	if got := mediaURL(post, img); got != "https://cdn/i.png" {
		t.Errorf("картинка: %q", got)
	}
}

func TestMakeTasksCarriesRefreshIdentifiers(t *testing.T) {
	post := &Post{
		ID:          "post-42",
		SignedQuery: "?sign=abc",
		Media: []MediaItem{
			{Kind: MediaImage, ID: "img-1", URL: "https://cdn/i.png"},
			{Kind: MediaVideo, ID: "vid-1", URLs: map[string]string{"high": "https://cdn/v.mp4"}},
			{Kind: MediaAudio, ID: "aud-1", URL: "https://cdn/a.mp3"},
			{Kind: MediaFile, ID: "file-1", URL: "https://cdn/f.zip", Title: "f.zip"},
			{Kind: MediaExternal, ID: "ext-1", URL: "https://youtube.com/x"},
		},
	}

	tasks := makeTasks(post, t.TempDir(), false)

	// Внешнее видео не должно попадать в очередь загрузки.
	if len(tasks) != 4 {
		t.Fatalf("задач: %d, ожидалось 4 (внешнее видео качать нельзя)", len(tasks))
	}
	// Без PostID/MediaID протухшую ссылку не обновить.
	for _, task := range tasks {
		if task.PostID != "post-42" {
			t.Errorf("%s: PostID = %q", task.MediaType, task.PostID)
		}
		if task.MediaID == "" {
			t.Errorf("%s: MediaID пуст — обновление ссылки станет невозможным", task.MediaType)
		}
	}
}

func TestMakeTasksSkipsVideoWithoutURL(t *testing.T) {
	post := &Post{
		ID:    "p1",
		Media: []MediaItem{{Kind: MediaVideo, ID: "v1", URLs: map[string]string{}}},
	}
	if tasks := makeTasks(post, t.TempDir(), false); len(tasks) != 0 {
		t.Errorf("задач: %d, ожидалось 0", len(tasks))
	}
}

func TestMakeTasksImageExtensionFromURL(t *testing.T) {
	post := &Post{
		ID: "p1",
		Media: []MediaItem{
			{Kind: MediaImage, ID: "i1", URL: "https://cdn/a.png"},
			{Kind: MediaImage, ID: "i2", URL: "https://cdn/b.gif?x=1"},
		},
	}
	tasks := makeTasks(post, "dir", false)
	if !strings.HasSuffix(tasks[0].Dest, ".png") {
		t.Errorf("Dest = %q, ожидалось .png", tasks[0].Dest)
	}
	if !strings.HasSuffix(tasks[1].Dest, ".gif") {
		t.Errorf("Dest = %q, ожидалось .gif", tasks[1].Dest)
	}
}
