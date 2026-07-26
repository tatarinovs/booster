package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// postJSON собирает ответ API для одного поста с единственным видео.
func postJSON(postID, mediaID, videoURL string) string {
	return fmt.Sprintf(`{
		"id": %q,
		"hasAccess": true,
		"title": "Пост",
		"signedQuery": "?sign=fresh",
		"publishTime": 1700000000,
		"data": [{"type": "ok_video", "id": %q, "title": "Видео",
		          "playerUrls": [{"type": "full_hd", "url": %q}]}]
	}`, postID, mediaID, videoURL)
}

// withAPIBase временно направляет клиента на тестовый сервер.
func withAPIBase(t *testing.T, base string) {
	t.Helper()
	prev := apiBase
	apiBase = base
	t.Cleanup(func() { apiBase = prev })
}

// withFastRetries убирает реальные паузы между попытками загрузки.
func withFastRetries(t *testing.T) {
	t.Helper()
	prev := downloadRetryDelay
	downloadRetryDelay = time.Millisecond
	t.Cleanup(func() { downloadRetryDelay = prev })
}

func TestRefreshMediaURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/blog/author/post/post-1" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, postJSON("post-1", "vid-1", "https://cdn/fresh.mp4"))
	}))
	defer srv.Close()
	withAPIBase(t, srv.URL)

	c := newBoostyClient(nil)

	got, err := c.refreshMediaURL(context.Background(), "author", "post-1", "vid-1")
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if got != "https://cdn/fresh.mp4" {
		t.Errorf("ссылка = %q", got)
	}

	// Вложение исчезло из поста — это ошибка, а не пустая ссылка.
	if _, err := c.refreshMediaURL(context.Background(), "author", "post-1", "нет-такого"); err == nil {
		t.Error("ожидалась ошибка для отсутствующего вложения")
	}
	// Пост удалён.
	if _, err := c.refreshMediaURL(context.Background(), "author", "нет-поста", "vid-1"); err == nil {
		t.Error("ожидалась ошибка для отсутствующего поста")
	}
}

func TestRefreshMediaURLSignsAudio(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"p1","hasAccess":true,"signedQuery":"?sign=fresh",
			"data":[{"type":"audio_file","id":"a1","title":"Трек","url":"https://cdn/a.mp3"}]}`)
	}))
	defer srv.Close()
	withAPIBase(t, srv.URL)

	got, err := newBoostyClient(nil).refreshMediaURL(context.Background(), "author", "p1", "a1")
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	// У аудио подпись обязана подставиться из свежего signedQuery.
	if got != "https://cdn/a.mp3?sign=fresh" {
		t.Errorf("ссылка = %q, ожидалась подписанная", got)
	}
}

// TestDownloadRefreshesExpiredURL — сквозная проверка: протухшая ссылка отдаёт
// 403, клиент перезапрашивает пост и докачивает файл по свежей ссылке.
// Раньше такой файл безусловно уходил в failed.txt.
func TestDownloadRefreshesExpiredURL(t *testing.T) {
	const body = "содержимое видео"
	var staleHits, freshHits atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/stale.mp4", func(w http.ResponseWriter, r *http.Request) {
		staleHits.Add(1)
		w.WriteHeader(http.StatusForbidden)
	})
	mux.HandleFunc("/fresh.mp4", func(w http.ResponseWriter, r *http.Request) {
		freshHits.Add(1)
		fmt.Fprint(w, body)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/v1/blog/author/post/post-1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, postJSON("post-1", "vid-1", srv.URL+"/fresh.mp4"))
	})
	withAPIBase(t, srv.URL)

	dir := t.TempDir()
	dest := filepath.Join(dir, "video.mp4")
	var abort atomic.Bool

	d := &downloader{
		author:     "author",
		client:     newBoostyClient(nil),
		http:       srv.Client(),
		stats:      newStats(),
		cancel:     &atomic.Bool{},
		abort:      &abort,
		failedPath: filepath.Join(dir, failedFilename),
	}

	d.downloadOne(context.Background(), DownloadTask{
		URL:       srv.URL + "/stale.mp4",
		Dest:      dest,
		MediaType: "video",
		IsVideo:   true,
		PostID:    "post-1",
		MediaID:   "vid-1",
	})

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("файл не скачан: %v", err)
	}
	if string(got) != body {
		t.Errorf("содержимое = %q, ожидалось %q", got, body)
	}
	if staleHits.Load() != 1 {
		t.Errorf("обращений к протухшей ссылке: %d, ожидалось 1", staleHits.Load())
	}
	if freshHits.Load() != 1 {
		t.Errorf("обращений к свежей ссылке: %d, ожидалось 1", freshHits.Load())
	}
	if d.stats.errorCount() != 0 {
		t.Errorf("ошибок: %d, ожидалось 0", d.stats.errorCount())
	}
	if _, err := os.Stat(d.failedPath); err == nil {
		t.Error("создан failed.txt, хотя файл в итоге скачался")
	}
}

// Обновление пробуется один раз: если и свежая ссылка отдаёт 403, повторять
// перезапрос бессмысленно — иначе получился бы цикл запросов к API.
func TestDownloadRefreshesOnlyOnce(t *testing.T) {
	var apiHits atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/file.mp4", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/v1/blog/author/post/post-1", func(w http.ResponseWriter, r *http.Request) {
		apiHits.Add(1)
		fmt.Fprint(w, postJSON("post-1", "vid-1", srv.URL+"/other.mp4"))
	})
	withAPIBase(t, srv.URL)

	dir := t.TempDir()
	var abort atomic.Bool
	d := &downloader{
		author:     "author",
		client:     newBoostyClient(nil),
		http:       srv.Client(),
		stats:      newStats(),
		cancel:     &atomic.Bool{},
		abort:      &abort,
		failedPath: filepath.Join(dir, failedFilename),
	}

	d.downloadOne(context.Background(), DownloadTask{
		URL:     srv.URL + "/file.mp4",
		Dest:    filepath.Join(dir, "video.mp4"),
		PostID:  "post-1",
		MediaID: "vid-1",
	})

	if n := apiHits.Load(); n != 1 {
		t.Errorf("обращений к API: %d, ожидалось ровно 1", n)
	}
	if d.stats.errorCount() != 1 {
		t.Errorf("ошибок: %d, ожидалась 1", d.stats.errorCount())
	}
	// Файл должен попасть в failed.txt.
	if _, err := os.Stat(d.failedPath); err != nil {
		t.Error("failed.txt не создан, хотя загрузка не удалась")
	}
}

// Задача без PostID/MediaID (например, из старой версии) не должна ходить в API.
func TestDownloadWithoutIdentifiersDoesNotRefresh(t *testing.T) {
	var apiHits atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/file.mp4", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		apiHits.Add(1)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withAPIBase(t, srv.URL)

	dir := t.TempDir()
	var abort atomic.Bool
	d := &downloader{
		author: "author", client: newBoostyClient(nil), http: srv.Client(),
		stats: newStats(), cancel: &atomic.Bool{}, abort: &abort,
		failedPath: filepath.Join(dir, failedFilename),
	}

	d.downloadOne(context.Background(), DownloadTask{
		URL:  srv.URL + "/file.mp4",
		Dest: filepath.Join(dir, "video.mp4"),
	})

	if n := apiHits.Load(); n != 0 {
		t.Errorf("обращений к API: %d, ожидалось 0", n)
	}
}

func TestDownloadSkipsExistingFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "уже-есть.mp4")
	if err := os.WriteFile(dest, []byte("данные"), 0o644); err != nil {
		t.Fatal(err)
	}

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	var abort atomic.Bool
	d := &downloader{
		author: "author", client: newBoostyClient(nil), http: srv.Client(),
		stats: newStats(), cancel: &atomic.Bool{}, abort: &abort,
		failedPath: filepath.Join(dir, failedFilename),
	}
	d.downloadOne(context.Background(), DownloadTask{URL: srv.URL + "/x", Dest: dest, MediaType: "video"})

	if hits.Load() != 0 {
		t.Errorf("сеть дёрнулась %d раз для уже скачанного файла", hits.Load())
	}
	if d.stats.skipped != 1 {
		t.Errorf("skipped = %d, ожидалось 1", d.stats.skipped)
	}
}

// Обрыв соединения не должен приводить к переименованию неполного файла:
// раньше проверка размера работала только для видео.
func TestDownloadRejectsTruncatedNonVideo(t *testing.T) {
	withFastRetries(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "короче обещанного")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "картинка.jpg")
	var abort atomic.Bool
	d := &downloader{
		author: "author", client: newBoostyClient(nil), http: srv.Client(),
		stats: newStats(), cancel: &atomic.Bool{}, abort: &abort,
		failedPath: filepath.Join(dir, failedFilename),
	}

	d.downloadOne(context.Background(), DownloadTask{
		URL: srv.URL + "/i.jpg", Dest: dest, MediaType: "photo",
	})

	if _, err := os.Stat(dest); err == nil {
		t.Error("неполный файл переименован в финальный")
	}
	if d.stats.errorCount() != 1 {
		t.Errorf("ошибок: %d, ожидалась 1", d.stats.errorCount())
	}
}
