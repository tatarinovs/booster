package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestGalleryClient — клиент без пауз между запросами, чтобы тесты
// не ждали реальный интервал вежливости.
func newTestGalleryClient() *galleryClient {
	c := newGalleryClient()
	c.interval = 0
	c.allowPrivate = true // httptest слушает 127.0.0.1
	return c
}

func TestParseGalleryURL(t *testing.T) {
	ok := []struct{ in, base, slug string }{
		{"https://photos.example.com/disk/album-x1y2z3", "https://photos.example.com", "album-x1y2z3"},
		{"https://photos.example.com/disk/album-x1y2z3/", "https://photos.example.com", "album-x1y2z3"},
		{"http://example.com/disk/abc123", "http://example.com", "abc123"},
		{"  https://example.com/disk/a.b_c-d  ", "https://example.com", "a.b_c-d"},
	}
	for _, tt := range ok {
		ref, good := parseGalleryURL(tt.in)
		if !good {
			t.Errorf("parseGalleryURL(%q) не распознан", tt.in)
			continue
		}
		if ref.Base != tt.base || ref.Slug != tt.slug {
			t.Errorf("parseGalleryURL(%q) = {%q, %q}, ожидалось {%q, %q}",
				tt.in, ref.Base, ref.Slug, tt.base, tt.slug)
		}
	}

	bad := []string{
		"https://boosty.to/pvlkrsnv",
		"https://example.com/disk",        // без слага
		"https://example.com/disk/a/b",    // вложенный путь — не галерея
		"https://example.com/other/album", // другой раздел
		"ftp://example.com/disk/album",    // не http
		"не ссылка",
		"",
	}
	for _, in := range bad {
		if _, good := parseGalleryURL(in); good {
			t.Errorf("parseGalleryURL(%q) ошибочно распознан как галерея", in)
		}
	}
}

func TestFindGalleryLinks(t *testing.T) {
	blocks := []map[string]any{
		{"type": "link", "url": "https://photos.example.com/disk/album-x1y2z3"},
		{"type": "text", "content": `["Смотрите тут https://example.com/disk/second, а также","unstyled",[]]`},
		{"type": "link", "url": "https://photos.example.com/disk/album-x1y2z3"}, // дубль
		{"type": "text", "content": `["Просто текст без ссылок","unstyled",[]]`},
		{"type": "link", "url": "https://youtube.com/watch?v=x"}, // не галерея
	}

	refs := findGalleryLinks(blocks)
	if len(refs) != 2 {
		t.Fatalf("найдено %d галерей, ожидалось 2: %+v", len(refs), refs)
	}
	if refs[0].Slug != "album-x1y2z3" {
		t.Errorf("первая галерея: %q", refs[0].Slug)
	}
	if refs[1].Slug != "second" {
		t.Errorf("вторая галерея: %q, ожидалось %q", refs[1].Slug, "second")
	}
}

func TestFindGalleryLinksTrimsPunctuation(t *testing.T) {
	// Ссылка, вписанная в предложение, тянет за собой знаки препинания.
	for _, raw := range []string{
		`["Тут https://example.com/disk/abc.","unstyled",[]]`,
		`["Тут (https://example.com/disk/abc)","unstyled",[]]`,
		`["Тут https://example.com/disk/abc!","unstyled",[]]`,
		`["Тут https://example.com/disk/abc, и ещё","unstyled",[]]`,
	} {
		refs := findGalleryLinks([]map[string]any{{"type": "text", "content": raw}})
		if len(refs) != 1 {
			t.Errorf("для %s найдено %d галерей, ожидалась 1", raw, len(refs))
			continue
		}
		if refs[0].Slug != "abc" {
			t.Errorf("для %s слаг = %q, ожидалось %q", raw, refs[0].Slug, "abc")
		}
	}
}

// fakeWfolio повторяет поведение настоящей галереи: сессия, форма архива
// с CSRF-токеном и turbo-redirect на zip.
type fakeWfolio struct {
	slug     string
	files    map[string]string // путь внутри архива → содержимое
	paid     bool              // скачивание закрыто: формы архива нет
	reject   bool              // сайт отвечает 403 на всё
	requests int32
	posts    int32
}

func (f *fakeWfolio) start(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	base := "/disk/" + f.slug

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.requests, 1)
		if f.reject {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch {
		case r.URL.Path == base:
			fmt.Fprint(w, `<html><body data-controller="wfolio">галерея</body></html>`)

		case r.URL.Path == base+"/downloads/project" && r.Method == http.MethodGet:
			if f.paid {
				fmt.Fprint(w, `<div class="modal">Pay for your order to download photos</div>`)
				return
			}
			fmt.Fprintf(w, `<form action=%q method="post">`+
				`<input type="hidden" name="authenticity_token" value="TESTTOKEN123" />`+
				`<input type="hidden" name="scope" value="project" />`+
				`<button name="storage" value="local">My computer</button></form>`,
				base+"/downloads/project")

		case r.URL.Path == base+"/downloads/project" && r.Method == http.MethodPost:
			atomic.AddInt32(&f.posts, 1)
			if err := r.ParseForm(); err != nil ||
				r.PostForm.Get("authenticity_token") != "TESTTOKEN123" ||
				r.PostForm.Get("size") != "original" ||
				r.PostForm.Get("storage") != "local" {
				w.WriteHeader(http.StatusUnprocessableEntity)
				return
			}
			w.Header().Set("Content-Type", "text/vnd.turbo-stream.html")
			fmt.Fprintf(w, `<turbo-stream action="redirect" target="%s/zip"></turbo-stream>`, srv.URL)

		case r.URL.Path == "/zip":
			w.Header().Set("Content-Type", "application/zip")
			w.Write(f.zipBytes(t))

		default:
			http.NotFound(w, r)
		}
	})

	return srv
}

func (f *fakeWfolio) zipBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range f.files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestGalleryArchiveURL(t *testing.T) {
	f := &fakeWfolio{slug: "album", files: map[string]string{"Album/Photos/a.jpg": "x"}}
	srv := f.start(t)

	ref, _ := parseGalleryURL(srv.URL + "/disk/album")
	u, err := newTestGalleryClient().archiveURL(context.Background(), ref)
	if err != nil {
		t.Fatalf("archiveURL: %v", err)
	}
	if !strings.HasSuffix(u, "/zip") {
		t.Errorf("ссылка = %q", u)
	}
	// Три запроса на галерею — ради этого архив и выбран вместо поштучной качки:
	// поштучно на сотню фото уходило больше двухсот обращений и ловился бан.
	if n := atomic.LoadInt32(&f.requests); n != 3 {
		t.Errorf("запросов к сайту: %d, ожидалось 3", n)
	}
}

// Платную галерею обходить нельзя.
func TestGalleryPaidIsNotBypassed(t *testing.T) {
	f := &fakeWfolio{slug: "paid", paid: true}
	srv := f.start(t)

	ref, _ := parseGalleryURL(srv.URL + "/disk/paid")
	_, err := newTestGalleryClient().archiveURL(context.Background(), ref)
	if !errors.Is(err, errGalleryNoDownload) {
		t.Fatalf("ошибка = %v, ожидалась errGalleryNoDownload", err)
	}
	if n := atomic.LoadInt32(&f.posts); n != 0 {
		t.Errorf("POST-запросов: %d, ожидалось 0", n)
	}
}

func TestGalleryRejectsNonWfolioPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><body>Обычный сайт</body></html>")
	}))
	defer srv.Close()

	ref, _ := parseGalleryURL(srv.URL + "/disk/whatever")
	if _, err := newTestGalleryClient().archiveURL(context.Background(), ref); !errors.Is(err, errNotGallery) {
		t.Errorf("ошибка = %v, ожидалась errNotGallery", err)
	}
}

func TestExtractGallery(t *testing.T) {
	dir := t.TempDir()
	f := &fakeWfolio{files: map[string]string{
		"Album/Photos/DSCF1.jpg": "первое фото",
		"Album/Photos/DSCF2.jpg": "второе фото",
		"Album/Video/clip.mp4":   "видео",
	}}
	zipPath := filepath.Join(dir, "g.zip")
	if err := os.WriteFile(zipPath, f.zipBytes(t), 0o644); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(dir, "gallery_album")
	n, err := extractGallery(zipPath, dest)
	if err != nil {
		t.Fatalf("extractGallery: %v", err)
	}
	if n != 3 {
		t.Errorf("извлечено %d файлов, ожидалось 3", n)
	}

	// Общий верхний каталог Album/ убирается: галерея уже лежит в своей папке.
	got, err := os.ReadFile(filepath.Join(dest, "Photos", "DSCF1.jpg"))
	if err != nil {
		t.Fatalf("файл не распакован: %v", err)
	}
	if string(got) != "первое фото" {
		t.Errorf("содержимое = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "Video", "clip.mp4")); err != nil {
		t.Errorf("вложенная папка не распакована: %v", err)
	}

	if !galleryDone(dest) {
		t.Error("маркер .complete не создан")
	}
	if _, err := os.Stat(zipPath); err == nil {
		t.Error("архив не удалён после распаковки")
	}
	leftovers, _ := filepath.Glob(filepath.Join(dest, "*", "*.part"))
	if len(leftovers) > 0 {
		t.Errorf("остались временные файлы: %v", leftovers)
	}
}

// Архив не должен уметь писать за пределы своей папки (zip slip).
func TestExtractGalleryRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{
		"../../захват.txt",
		`..\..\захват2.txt`,
		"/абсолютный.txt",
		"ok.txt",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("данные")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	zipPath := filepath.Join(dir, "evil.zip")
	if err := os.WriteFile(zipPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out")

	if _, err := extractGallery(zipPath, dest); err != nil {
		t.Fatalf("extractGallery: %v", err)
	}

	for _, escaped := range []string{"захват.txt", "захват2.txt", "абсолютный.txt"} {
		if _, err := os.Stat(filepath.Join(dir, escaped)); err == nil {
			t.Errorf("файл %q записан за пределы папки назначения", escaped)
		}
	}
	err := filepath.Walk(dest, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !strings.HasPrefix(p, dest) {
			t.Errorf("файл вне папки: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStripCommonRoot(t *testing.T) {
	tests := []struct {
		names []string
		want  string
	}{
		{[]string{"Album/a.jpg", "Album/b/c.jpg"}, "Album"},
		{[]string{"Album/a.jpg", "Другая/b.jpg"}, ""}, // разные корни
		{[]string{"a.jpg", "Album/b.jpg"}, ""},        // файл в корне архива
		{[]string{}, ""},
	}
	for _, tt := range tests {
		if got := stripCommonRoot(tt.names); got != tt.want {
			t.Errorf("stripCommonRoot(%v) = %q, ожидалось %q", tt.names, got, tt.want)
		}
	}
}

func TestGalleryTasks(t *testing.T) {
	f := &fakeWfolio{slug: "album", files: map[string]string{"Album/a.jpg": "фото"}}
	srv := f.start(t)

	dir := t.TempDir()
	post := &Post{ID: "post-1", TextBlocks: []map[string]any{
		{"type": "link", "url": srv.URL + "/disk/album"},
	}}
	stats := newStats()

	tasks := galleryTasks(context.Background(), newTestGalleryClient(), post, dir, stats)
	if len(tasks) != 1 {
		t.Fatalf("задач: %d, ожидалась 1 (одна на галерею, а не на каждое фото)", len(tasks))
	}
	// Построение задач не должно ходить в сеть, иначе обход постов встаёт колом.
	if n := atomic.LoadInt32(&f.requests); n != 0 {
		t.Errorf("запросов при построении задач: %d, ожидалось 0", n)
	}

	task := tasks[0]
	if task.URL != "" || task.Resolve == nil || task.AfterDownload == nil {
		t.Fatalf("задача заполнена неверно: %+v", task)
	}
	if filepath.Base(task.Dest) != "gallery_album.zip" {
		t.Errorf("Dest = %q", task.Dest)
	}
	if stats.galleries != 1 {
		t.Errorf("счётчик галерей = %d, ожидался 1", stats.galleries)
	}
}

// Уже распакованная галерея пропускается без единого обращения к сайту.
func TestGalleryTasksSkipsCompleted(t *testing.T) {
	f := &fakeWfolio{slug: "album", files: map[string]string{"a.jpg": "x"}}
	srv := f.start(t)

	dir := t.TempDir()
	done := filepath.Join(dir, "gallery_album")
	if err := os.MkdirAll(done, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(done, galleryMarker), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	post := &Post{ID: "p", TextBlocks: []map[string]any{
		{"type": "link", "url": srv.URL + "/disk/album"},
	}}
	tasks := galleryTasks(context.Background(), newTestGalleryClient(), post, dir, newStats())
	if len(tasks) != 0 {
		t.Errorf("задач: %d, ожидалось 0 для готовой галереи", len(tasks))
	}
	if n := atomic.LoadInt32(&f.requests); n != 0 {
		t.Errorf("запросов: %d, ожидалось 0", n)
	}
}

// Сайт отвечает 403 на всё: после нескольких отказов клиент прекращает запросы.
func TestGalleryStopsAfterRepeatedRejections(t *testing.T) {
	f := &fakeWfolio{slug: "album", reject: true}
	srv := f.start(t)

	c := newTestGalleryClient()
	ref, _ := parseGalleryURL(srv.URL + "/disk/album")

	var lastErr error
	for i := 0; i < 10; i++ {
		_, lastErr = c.archiveURL(context.Background(), ref)
	}

	if !errors.Is(lastErr, errGalleryBlocked) {
		t.Errorf("последняя ошибка = %v, ожидалась errGalleryBlocked", lastErr)
	}
	if n := atomic.LoadInt32(&f.requests); n > galleryBlockThreshold {
		t.Errorf("запросов к сайту: %d, ожидалось не больше %d", n, galleryBlockThreshold)
	}
}

// Успешный ответ сбрасывает счётчик: единичный отказ не должен копиться.
func TestGalleryFailureCounterResets(t *testing.T) {
	c := newTestGalleryClient()
	c.noteResult(true)
	c.noteResult(true)
	c.noteResult(false)
	c.noteResult(true)
	c.noteResult(true)

	if c.blocked {
		t.Error("клиент заблокировался, хотя отказы шли не подряд")
	}
	c.noteResult(true)
	if !c.blocked {
		t.Error("три отказа подряд должны привести к блокировке")
	}
}

// Запросы должны расходиться во времени: без этого nginx банит по IP.
func TestGalleryRateLimitSpacesRequests(t *testing.T) {
	c := newGalleryClient()
	c.interval = 40 * time.Millisecond

	start := time.Now()
	for i := 0; i < 4; i++ {
		if _, err := c.reserve(); err != nil {
			t.Fatalf("reserve: %v", err)
		}
	}
	last, _ := c.reserve()
	if gap := last.Sub(start); gap < 3*c.interval {
		t.Errorf("слоты выданы с зазором %v, ожидалось не меньше %v", gap, 3*c.interval)
	}
}

func TestGalleryTasksNoLinks(t *testing.T) {
	post := &Post{ID: "p", TextBlocks: []map[string]any{
		{"type": "text", "content": `["Текст без галерей","unstyled",[]]`},
	}}
	if tasks := galleryTasks(context.Background(), newTestGalleryClient(), post, "d", newStats()); tasks != nil {
		t.Errorf("задачи: %+v, ожидался nil", tasks)
	}
}
