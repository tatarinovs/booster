package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestDownloader(dir string, httpClient *http.Client) *downloader {
	return &downloader{
		author:     "author",
		client:     newBoostyClient(nil),
		http:       httpClient,
		stats:      newStats(),
		cancel:     &atomic.Bool{},
		abort:      &atomic.Bool{},
		failedPath: filepath.Join(dir, failedFilename),
	}
}

// Оборванная пагинация не должна сдвигать отметку быстрой синхронизации:
// раньше гонка в select засчитывала её как успех, и посты за местом сбоя
// терялись навсегда.
//
// Гонка воспроизводится так: у поста больше картинок, чем вмещает очередь,
// а качаются они медленно. Пока главный цикл стоит на отправке в очередь,
// вторая страница успевает упасть, и к возвращению в select готовы оба
// канала сразу. Без исправления тест падает почти в каждом прогоне.
func TestRunPaginationErrorKeepsSyncMarker(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var images []string
	for i := 0; i < 6; i++ {
		images = append(images, fmt.Sprintf(`{"type":"image","id":"i%d","width":1,"url":"%s/img/%d.jpg"}`, i, srv.URL, i))
	}
	mux.HandleFunc("/img/", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(15 * time.Millisecond)
		fmt.Fprint(w, "x")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/blog/author":
			fmt.Fprint(w, `{"count":{"posts":3}}`)
		case r.URL.Path == "/v1/blog/author/post/" && r.URL.Query().Get("offset") == "":
			fmt.Fprintf(w, `{"data":[{"id":"p1","hasAccess":true,"data":[%s]}],
				"extra":{"isLast":false,"offset":"next"}}`, strings.Join(images, ","))
		default:
			// 404 не повторяется — ошибка приходит сразу.
			http.NotFound(w, r)
		}
	})
	withAPIBase(t, srv.URL)

	for i := 0; i < 20; i++ {
		dir := t.TempDir()
		err := run(context.Background(), runOptions{
			author: "author", outputDir: dir, workers: 1, noGalleries: true,
			cancel: &atomic.Bool{}, abort: &atomic.Bool{}, stats: newStats(),
		})
		if err == nil {
			t.Fatalf("прогон %d: ошибка пагинации потеряна", i)
		}
		if _, err := os.Stat(filepath.Join(dir, "author", ".latest_post")); err == nil {
			t.Fatalf("прогон %d: .latest_post записан, хотя посты получены не все", i)
		}
	}
}

// --full обязан игнорировать отметку и просматривать все посты.
func TestRunFullCheckIgnoresMarker(t *testing.T) {
	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/blog/author":
			fmt.Fprint(w, `{"count":{"posts":2}}`)
		case "/v1/blog/author/post/":
			pages.Add(1)
			fmt.Fprint(w, `{"data":[{"id":"new","hasAccess":false},{"id":"old","hasAccess":false}],
				"extra":{"isLast":true}}`)
		}
	}))
	defer srv.Close()
	withAPIBase(t, srv.URL)

	dir := t.TempDir()
	authorDir := filepath.Join(dir, "author")
	if err := os.MkdirAll(authorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authorDir, ".latest_post"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	stats := newStats()
	err := run(context.Background(), runOptions{
		author: "author", outputDir: dir, workers: 1, noGalleries: true, fullCheck: true,
		cancel: &atomic.Bool{}, abort: &atomic.Bool{}, stats: stats,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Без --full обход остановился бы на первом же посте «new».
	if stats.noAccess != 2 {
		t.Errorf("просмотрено постов без доступа: %d, ожидалось 2", stats.noAccess)
	}
}

// .part скачан целиком, но не переименован: сервер отвечает 416, и файл
// должен стать готовым, а не уходить в ошибки при каждом запуске.
func TestDownloadCompletesFullPartOn416(t *testing.T) {
	const body = "полное содержимое"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(body)))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "video.mp4")
	if err := os.WriteFile(dest+".part", []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	d := newTestDownloader(dir, srv.Client())
	d.downloadOne(context.Background(), DownloadTask{URL: srv.URL + "/v", Dest: dest, MediaType: "video"})

	got, err := os.ReadFile(dest)
	if err != nil || string(got) != body {
		t.Fatalf("файл = %q, %v", got, err)
	}
	if d.stats.errorCount() != 0 || d.stats.videos != 1 {
		t.Errorf("ошибок %d, видео %d", d.stats.errorCount(), d.stats.videos)
	}
}

// 416 при .part другого размера — докачка невозможна, качаем заново.
func TestDownload416WithMismatchedPartRestarts(t *testing.T) {
	withFastRetries(t)
	const body = "настоящее содержимое"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(body)))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(dest+".part", []byte(strings.Repeat("x", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	d := newTestDownloader(dir, srv.Client())
	d.downloadOne(context.Background(), DownloadTask{URL: srv.URL + "/f", Dest: dest, MediaType: "file"})

	if got, _ := os.ReadFile(dest); string(got) != body {
		t.Errorf("файл = %q, ожидалось %q", got, body)
	}
}

// Протухшая ссылка при недокачанном файле: сначала обновляется ссылка, и
// докачка продолжается с места обрыва, а не начинается заново.
func TestDownloadRefreshKeepsPartialFile(t *testing.T) {
	const head, tail = "начало-", "конец"
	var freshRange atomic.Value

	mux := http.NewServeMux()
	mux.HandleFunc("/stale.mp4", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	mux.HandleFunc("/fresh.mp4", func(w http.ResponseWriter, r *http.Request) {
		freshRange.Store(r.Header.Get("Range"))
		if r.Header.Get("Range") == fmt.Sprintf("bytes=%d-", len(head)) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", len(head), len(head+tail)-1, len(head+tail)))
			w.WriteHeader(http.StatusPartialContent)
			fmt.Fprint(w, tail)
			return
		}
		fmt.Fprint(w, head+tail)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/v1/blog/author/post/post-1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, postJSON("post-1", "vid-1", srv.URL+"/fresh.mp4"))
	})
	withAPIBase(t, srv.URL)

	dir := t.TempDir()
	dest := filepath.Join(dir, "video.mp4")
	if err := os.WriteFile(dest+".part", []byte(head), 0o644); err != nil {
		t.Fatal(err)
	}
	d := newTestDownloader(dir, srv.Client())
	d.downloadOne(context.Background(), DownloadTask{
		URL: srv.URL + "/stale.mp4", Dest: dest, MediaType: "video", PostID: "post-1", MediaID: "vid-1",
	})

	if got, _ := os.ReadFile(dest); string(got) != head+tail {
		t.Errorf("файл = %q", got)
	}
	if r, _ := freshRange.Load().(string); r == "" {
		t.Error("по свежей ссылке файл качался заново, а не докачивался")
	}
}

// Зависшее соединение обрывается сторожем, и файл докачивается.
func TestDownloadRecoversFromStalledConnection(t *testing.T) {
	withFastRetries(t)
	prev := downloadIdleTimeout
	downloadIdleTimeout = 100 * time.Millisecond
	t.Cleanup(func() { downloadIdleTimeout = prev })

	const head, tail = "первая часть|", "вторая часть"
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", fmt.Sprint(len(head+tail)))
			fmt.Fprint(w, head)
			w.(http.Flusher).Flush()
			// Молчим, пока клиент не оборвёт соединение.
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", len(head), len(head+tail)-1, len(head+tail)))
		w.WriteHeader(http.StatusPartialContent)
		fmt.Fprint(w, tail)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "f.bin")
	d := newTestDownloader(dir, srv.Client())

	done := make(chan struct{})
	go func() {
		d.downloadOne(context.Background(), DownloadTask{URL: srv.URL + "/f", Dest: dest, MediaType: "file"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("загрузка зависла вместе с соединением")
	}
	if got, _ := os.ReadFile(dest); string(got) != head+tail {
		t.Errorf("файл = %q", got)
	}
}

// Платная галерея — не ошибка: иначе отметка синхронизации не обновлялась бы
// никогда, а код возврата всегда был бы 2.
func TestUnavailableGalleryIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	d := newTestDownloader(dir, http.DefaultClient)
	d.downloadOne(context.Background(), DownloadTask{
		Dest: filepath.Join(dir, "gallery_x.zip"), MediaType: "gallery",
		Resolve: func(context.Context) (string, error) { return "", errGalleryNoDownload },
	})
	if d.stats.errorCount() != 0 {
		t.Errorf("ошибок: %d, ожидалось 0", d.stats.errorCount())
	}
	if d.stats.unavailable != 1 {
		t.Errorf("недоступно: %d, ожидалось 1", d.stats.unavailable)
	}
	if _, err := os.Stat(d.failedPath); err == nil {
		t.Error("недоступная галерея попала в failed.txt")
	}
}

// Битый архив, оставшийся с прошлого запуска, удаляется и скачивается заново.
func TestCorruptExistingArchiveIsRedownloaded(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("Album/a.jpg")
	w.Write([]byte("фото"))
	zw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(buf.Bytes())
	}))
	defer srv.Close()

	dir := t.TempDir()
	galleryDir := filepath.Join(dir, "gallery_x")
	zipPath := galleryDir + ".zip"
	if err := os.WriteFile(zipPath, []byte("это не zip"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := newTestDownloader(dir, srv.Client())
	d.downloadOne(context.Background(), DownloadTask{
		URL: srv.URL + "/zip", Dest: zipPath, MediaType: "gallery",
		AfterDownload: func(p string) error {
			_, err := extractGallery(p, galleryDir)
			return err
		},
	})

	if got, err := os.ReadFile(filepath.Join(galleryDir, "a.jpg")); err != nil || string(got) != "фото" {
		t.Fatalf("галерея не распакована: %q, %v", got, err)
	}
	if d.stats.errorCount() != 0 {
		t.Errorf("ошибок: %d, ожидалось 0", d.stats.errorCount())
	}
}

// Ссылка из поста не должна приводить к запросам во внутреннюю сеть.
func TestGalleryRejectsPrivateHost(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	c := newGalleryClient()
	c.interval = 0
	ref, _ := parseGalleryURL(srv.URL + "/disk/x")
	_, err := c.archiveURL(context.Background(), ref)
	if !errors.Is(err, errUnavailable) {
		t.Errorf("ошибка = %v, ожидалась errUnavailable", err)
	}
	if hits.Load() != 0 {
		t.Errorf("запросов к локальному адресу: %d", hits.Load())
	}
}

// Переадресация с внешнего сайта во внутреннюю сеть тоже отсекается.
func TestGalleryRedirectToPrivateHostRejected(t *testing.T) {
	c := newGalleryClient()
	u, _ := url.Parse("http://10.0.0.1/disk/x")
	req := &http.Request{URL: u}
	req = req.WithContext(context.Background())
	if err := c.http.CheckRedirect(req, nil); !errors.Is(err, errUnavailable) {
		t.Errorf("ошибка = %v, ожидалась errUnavailable", err)
	}
}

func TestIsPublicAddr(t *testing.T) {
	cases := map[string]bool{
		"8.8.8.8":         true,
		"2a00:1450::1":    true,
		"127.0.0.1":       false,
		"10.1.2.3":        false,
		"192.168.0.1":     false,
		"172.16.5.5":      false,
		"169.254.169.254": false,
		"100.64.0.1":      false,
		"0.0.0.0":         false,
		"::1":             false,
		"fe80::1":         false,
		"fd00::1":         false,
		"::ffff:10.0.0.1": false,
	}
	for in, want := range cases {
		if got := isPublicAddr(netip.MustParseAddr(in)); got != want {
			t.Errorf("isPublicAddr(%s) = %v, ожидалось %v", in, got, want)
		}
	}
}

func TestContentRangeTotal(t *testing.T) {
	cases := []struct {
		in   string
		n    int64
		isOK bool
	}{
		{"bytes */1234", 1234, true},
		{"bytes 0-99/1234", 1234, true},
		{"bytes 0-99/*", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		n, ok := contentRangeTotal(c.in)
		if n != c.n || ok != c.isOK {
			t.Errorf("contentRangeTotal(%q) = %d, %v", c.in, n, ok)
		}
	}
}

// slowFileServer отдаёт первую половину файла и ждёт release, прежде чем
// отдать остальное. started закрывается, когда первая половина ушла клиенту.
func slowFileServer(t *testing.T, head, tail string) (srv *httptest.Server, started, release chan struct{}) {
	t.Helper()
	started, release = make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(head+tail)))
		fmt.Fprint(w, head)
		w.(http.Flusher).Flush()
		if once.CompareAndSwap(false, true) {
			close(started)
		}
		select {
		case <-release:
			fmt.Fprint(w, tail)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	return srv, started, release
}

// Первый Ctrl+C: текущая загрузка доводится до конца, задачи из очереди
// отбрасываются и учитываются как отменённые.
func TestSoftCancelFinishesCurrentDownload(t *testing.T) {
	const head, tail = "первая половина|", "вторая половина"
	srv, started, release := slowFileServer(t, head, tail)

	dir := t.TempDir()
	d := newTestDownloader(dir, srv.Client())
	queue := make(chan DownloadTask, 2)
	queue <- DownloadTask{URL: srv.URL + "/a", Dest: filepath.Join(dir, "a.bin"), MediaType: "file"}
	queue <- DownloadTask{URL: srv.URL + "/b", Dest: filepath.Join(dir, "b.bin"), MediaType: "file"}
	close(queue)

	var wg sync.WaitGroup
	wg.Add(1)
	go d.worker(context.Background(), 0, queue, &wg)

	<-started
	d.cancel.Store(true) // первый Ctrl+C
	close(release)
	wg.Wait()

	if got, _ := os.ReadFile(filepath.Join(dir, "a.bin")); string(got) != head+tail {
		t.Errorf("текущий файл не докачан: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.bin")); err == nil {
		t.Error("задача из очереди скачана после мягкой остановки")
	}
	if d.stats.cancelled != 1 || d.stats.errorCount() != 0 {
		t.Errorf("отменено %d, ошибок %d", d.stats.cancelled, d.stats.errorCount())
	}
}

// Второй Ctrl+C: загрузка обрывается сразу, .part остаётся для докачки,
// и обрыв не считается ошибкой.
func TestAbortStopsDownloadKeepingPart(t *testing.T) {
	const head, tail = "первая половина|", "вторая половина"
	srv, started, _ := slowFileServer(t, head, tail)

	dir := t.TempDir()
	dest := filepath.Join(dir, "a.bin")
	d := newTestDownloader(dir, srv.Client())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		d.downloadOne(ctx, DownloadTask{URL: srv.URL + "/a", Dest: dest, MediaType: "file"})
		close(done)
	}()

	<-started
	// Сервер отдал первую половину — ждём, пока клиент запишет её на диск.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if fi, err := os.Stat(dest + ".part"); err == nil && fi.Size() == int64(len(head)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("первая половина так и не записана в .part")
		}
		time.Sleep(5 * time.Millisecond)
	}
	d.cancel.Store(true) // первый Ctrl+C
	d.abort.Store(true)  // второй Ctrl+C
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("загрузка не прервалась по второму Ctrl+C")
	}
	if _, err := os.Stat(dest); err == nil {
		t.Error("недокачанный файл переименован в итоговый")
	}
	if got, err := os.ReadFile(dest + ".part"); err != nil || string(got) != head {
		t.Errorf(".part = %q, %v — ожидалась сохранённая первая половина", got, err)
	}
	if d.stats.errorCount() != 0 {
		t.Errorf("ошибок: %d, обрыв пользователем не должен считаться ошибкой", d.stats.errorCount())
	}
}
