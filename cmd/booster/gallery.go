package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Поддержка внешних галерей wfolio: авторы выкладывают на них полные съёмки
// и дают ссылку в посте. Внутри поста лежат уменьшенные превью, оригиналы —
// только в галерее.
//
// Галерея умеет отдавать всё одним архивом, и берём мы именно его:
//  1. GET  /disk/{slug}                   — сессионная cookie и проверка, что это wfolio;
//  2. GET  /disk/{slug}/downloads/project — форма архива с Rails-токеном CSRF;
//  3. POST /disk/{slug}/downloads/project — turbo-redirect на zip.
//
// Поштучное скачивание тоже работает, но требует двух запросов на каждое фото:
// на съёмку в сотню кадров это больше двухсот обращений к сайту автора, после
// чего nginx отвечает 403 на весь IP. Архив — три запроса на галерею, отдаётся
// потоком через mod_zip (сервер ничего не готовит заранее) и поддерживает Range,
// то есть докачивается штатным механизмом .part.
//
// Разметка машинная и стабильная, поэтому разбирается регулярками, без
// внешних зависимостей на HTML-парсер.

const (
	galleryTimeout   = 60 * time.Second
	galleryMaxBody   = 32 << 20 // потолок на размер HTML-страницы
	galleryUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36"

	// galleryInterval — минимальный промежуток между запросами к сайту автора.
	// Ограничение общее для всех воркеров: на галерею из сотни фото приходится
	// столько же запросов, и без паузы nginx на стороне сайта отвечает 403 на
	// весь IP. Сами файлы качаются с CDN обычным клиентом и сюда не попадают.
	galleryInterval = 500 * time.Millisecond

	// galleryBlockThreshold — сколько отказов подряд считать блокировкой.
	// После этого работа с галереями прекращается до конца запуска: долбиться
	// в закрытую дверь тысячами запросов только продлевает бан.
	galleryBlockThreshold = 3
)

var (
	// errUnavailable — материал скачать нельзя в принципе (платная галерея,
	// ссылка не на галерею). Это не сбой: такие задачи пропускаются и не
	// мешают отметке синхронизации.
	errUnavailable = errors.New("недоступно")

	// errGalleryBlocked означает, что сайт перестал нас обслуживать.
	errGalleryBlocked = errors.New("сайт галереи отклоняет запросы (вероятно, сработало ограничение частоты)")
	// errNotGallery — ссылка ведёт на посторонний сайт, а не на wfolio.
	errNotGallery = fmt.Errorf("%w: не похоже на галерею wfolio", errUnavailable)
	// errGalleryNoDownload — скачивание закрыто владельцем либо галерея платная.
	errGalleryNoDownload = fmt.Errorf("%w: скачивание архива закрыто (возможно, галерея платная)", errUnavailable)
	// errPrivateHost — ссылка ведёт во внутреннюю сеть пользователя.
	errPrivateHost = fmt.Errorf("%w: ссылка ведёт на локальный или внутренний адрес", errUnavailable)
	// errBadArchive — скачанный архив повреждён; он удаляется, чтобы
	// следующая попытка скачала его заново.
	errBadArchive = errors.New("архив повреждён")
)

var (
	// diskPathRe вычленяет слаг галереи из пути вида /disk/album-x1y2z3.
	diskPathRe = regexp.MustCompile(`^/disk/([A-Za-z0-9][A-Za-z0-9._-]*)/?$`)
	// tokenRe достаёт Rails-токен CSRF из формы скачивания.
	tokenRe = regexp.MustCompile(`name="authenticity_token"[^>]*?value="([^"]+)"`)
	// redirectRe достаёт итоговую ссылку на архив из turbo-stream ответа.
	redirectRe = regexp.MustCompile(`<turbo-stream[^>]*action="redirect"[^>]*target="([^"]+)"`)
	// httpURLRe вылавливает ссылки из текста поста.
	httpURLRe = regexp.MustCompile(`https?://[^\s"'<>)\]]+`)
)

// galleryClient ходит по wfolio-галереям. Пригоден для конкурентного
// использования: http.Client с cookiejar потокобезопасен, а остальное
// закрыто мьютексом.
type galleryClient struct {
	http     *http.Client
	interval time.Duration
	// allowPrivate разрешает адреса внутренней сети — только для тестов
	// с локальным httptest-сервером.
	allowPrivate bool

	mu       sync.Mutex
	tokens   map[string]string // страница галереи → CSRF-токен
	nextSlot time.Time         // не раньше этого момента можно слать запрос
	failures int               // отказов подряд
	blocked  bool
}

func newGalleryClient() *galleryClient {
	jar, _ := cookiejar.New(nil)
	c := &galleryClient{
		interval: galleryInterval,
		tokens:   map[string]string{},
	}
	c.http = &http.Client{
		Timeout: galleryTimeout,
		Jar:     jar,
		// Переадресация проверяется так же, как исходная ссылка: иначе
		// внешний сайт мог бы увести запрос во внутреннюю сеть.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("слишком много переадресаций")
			}
			return c.checkHost(req.Context(), req.URL)
		},
	}
	return c
}

// checkHost не пускает запросы на локальные и внутренние адреса. Ссылки на
// галереи берутся из текста постов, то есть их пишет автор, и без проверки
// любая ссылка вида http://192.168.1.1/disk/x заставила бы программу
// обращаться к устройствам в домашней сети пользователя.
func (c *galleryClient) checkHost(ctx context.Context, u *url.URL) error {
	if c.allowPrivate {
		return nil
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: недопустимая схема %q", errUnavailable, u.Scheme)
	}
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil {
		if !isPublicAddr(ip) {
			return errPrivateHost
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return err
	}
	for _, ip := range addrs {
		if !isPublicAddr(ip) {
			return errPrivateHost
		}
	}
	return nil
}

func isPublicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsValid() && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsUnspecified() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast() &&
		!netip.MustParsePrefix("100.64.0.0/10").Contains(ip) // CGNAT
}

// reserve выдаёт следующий разрешённый момент запроса. Слоты раздаются под
// мьютексом, поэтому общий темп соблюдается независимо от числа воркеров.
func (c *galleryClient) reserve() (time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blocked {
		return time.Time{}, errGalleryBlocked
	}
	now := time.Now()
	slot := c.nextSlot
	if slot.Before(now) {
		slot = now
	}
	c.nextSlot = slot.Add(c.interval)
	return slot, nil
}

// noteResult копит отказы: несколько подряд означают, что нас перестали
// обслуживать, и дальше ходить бессмысленно.
func (c *galleryClient) noteResult(rejected bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !rejected {
		c.failures = 0
		return
	}
	c.failures++
	if c.failures >= galleryBlockThreshold && !c.blocked {
		c.blocked = true
		logError("Галереи отключены до конца запуска: %v", errGalleryBlocked)
		logError("Подождите и запустите позже — уже скачанные фото пропускаются.")
	}
}

// galleryRef — разобранная ссылка на галерею.
type galleryRef struct {
	Base string // https://photos.example.com
	Slug string // album-x1y2z3
}

func (g galleryRef) pageURL() string { return g.Base + "/disk/" + g.Slug }

// parseGalleryURL распознаёт ссылку вида https://host/disk/{slug}.
// Хост не проверяется: wfolio работает на собственных доменах авторов,
// поэтому принадлежность платформе выясняется уже при загрузке страницы.
func parseGalleryURL(raw string) (galleryRef, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return galleryRef{}, false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return galleryRef{}, false
	}
	m := diskPathRe.FindStringSubmatch(u.Path)
	if m == nil {
		return galleryRef{}, false
	}
	return galleryRef{Base: u.Scheme + "://" + u.Host, Slug: m[1]}, true
}

// findGalleryLinks собирает ссылки на галереи из текстовых блоков поста.
func findGalleryLinks(blocks []map[string]any) []galleryRef {
	var out []galleryRef
	seen := map[string]bool{}

	add := func(raw string) {
		// Хвостовая пунктуация из текста в ссылку не входит.
		raw = strings.TrimRight(raw, ".,;:!?)")
		ref, ok := parseGalleryURL(raw)
		if !ok || seen[ref.pageURL()] {
			return
		}
		seen[ref.pageURL()] = true
		out = append(out, ref)
	}

	for _, b := range blocks {
		if u := asString(b["url"]); u != "" {
			add(u)
		}
		// Ссылка может быть просто вписана в текст блока.
		for _, u := range httpURLRe.FindAllString(asString(b["content"]), -1) {
			add(strings.TrimSuffix(u, `\"`))
		}
	}
	return out
}

// do выполняет запрос к сайту галереи и возвращает тело вместе с конечным
// адресом (после возможных переадресаций).
func (c *galleryClient) do(ctx context.Context, method, rawURL, referer string, body io.Reader) (string, string, error) {
	slot, err := c.reserve()
	if err != nil {
		return "", "", err
	}
	if wait := time.Until(slot); wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return "", "", ctx.Err()
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return "", "", err
	}
	if err := c.checkHost(ctx, req.URL); err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", galleryUserAgent)
	// Accept строго */*. Заманчиво прислать «как браузер»
	// (text/html,application/xhtml+xml,...), но тогда Rails выбирает другую
	// ветку respond_to и отвечает переадресацией на саму галерею вместо формы.
	// Ровно так же ломает дело и text/vnd.turbo-stream.html на POST.
	req.Header.Set("Accept", "*/*")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Отказ проверки переадресации приходит обёрнутым в *url.Error;
		// errors.Is доберётся до errUnavailable сквозь обёртку.
		return "", "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, galleryMaxBody))
	if err != nil {
		return "", "", err
	}

	// 403 и 429 — признаки того, что нас ограничивают, а не что материала нет.
	rejected := resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests
	c.noteResult(rejected)

	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return string(data), resp.Request.URL.String(), nil
}

// archiveURL получает прямую ссылку на zip со всей галереей.
// Три запроса: страница (сессия), форма архива (CSRF-токен), отправка формы.
func (c *galleryClient) archiveURL(ctx context.Context, ref galleryRef) (string, error) {
	// Первый заход заводит сессионную cookie и подтверждает, что это wfolio.
	page, _, err := c.do(ctx, http.MethodGet, ref.pageURL(), "", nil)
	if err != nil {
		return "", err
	}
	if !strings.Contains(page, "wfolio") && !strings.Contains(page, "/pieces?") {
		return "", errNotGallery
	}

	formURL := ref.pageURL() + "/downloads/project"
	form, finalURL, err := c.do(ctx, http.MethodGet, formURL, ref.pageURL(), nil)
	if err != nil {
		return "", err
	}
	// Переадресация обратно на галерею означает, что сервер не отдал форму;
	// без этой проверки мы бы разбирали страницу галереи и объявляли её платной.
	if finalURL != formURL {
		return "", fmt.Errorf("форма архива недоступна: сервер увёл на %s", finalURL)
	}
	m := tokenRe.FindStringSubmatch(form)
	if m == nil {
		// Формы нет — скачивание закрыто или галерея платная. Обходить оплату
		// мы не пытаемся: сообщаем и идём дальше.
		return "", errGalleryNoDownload
	}

	body := url.Values{}
	body.Set("authenticity_token", m[1])
	body.Set("scope", "project") // весь проект, а не текущая папка
	body.Set("folder_path", "")
	body.Set("size", "original")
	body.Set("cart_secure_id", "")
	body.Set("person_id", "")
	body.Set("storage", "local") // «My computer», а не Яндекс.Диск

	resp, _, err := c.do(ctx, http.MethodPost, formURL, ref.pageURL(), strings.NewReader(body.Encode()))
	if err != nil {
		return "", err
	}
	r := redirectRe.FindStringSubmatch(resp)
	if r == nil {
		return "", fmt.Errorf("ссылка на архив не найдена в ответе")
	}
	archive := html.UnescapeString(r[1])
	// Архив качает общий загрузчик, поэтому его адрес проверяется здесь же.
	u, err := url.Parse(archive)
	if err != nil {
		return "", fmt.Errorf("некорректная ссылка на архив: %w", err)
	}
	if err := c.checkHost(ctx, u); err != nil {
		return "", err
	}
	return archive, nil
}

// galleryTasks находит в посте ссылки на галереи и строит задачи загрузки.
// Сетевых запросов здесь нет: ссылка на архив вычисляется уже в воркере, иначе
// перебор галерей затормозил бы обход постов. Готовые галереи пропускаются
// по файлу-маркеру, то есть повторный запуск не трогает сайт вовсе.
func galleryTasks(ctx context.Context, c *galleryClient, post *Post, destDir string, stats *Stats) []DownloadTask {
	refs := findGalleryLinks(post.TextBlocks)
	if len(refs) == 0 {
		return nil
	}

	var tasks []DownloadTask
	for _, ref := range refs {
		dir := filepath.Join(destDir, "gallery_"+safeFilename(ref.Slug))
		if galleryDone(dir) {
			continue
		}
		stats.incGallery()

		tasks = append(tasks, DownloadTask{
			Dest:      dir + ".zip",
			MediaType: "gallery",
			Referer:   ref.pageURL(),
			Resolve: func(ctx context.Context) (string, error) {
				return c.archiveURL(ctx, ref)
			},
			AfterDownload: func(zipPath string) error {
				n, err := extractGallery(zipPath, dir)
				if err != nil {
					return err
				}
				logInfo("Галерея %s: распаковано файлов — %d", ref.Slug, n)
				stats.addGalleryFiles(n)
				return nil
			},
		})
	}
	return tasks
}

// galleryMarker — файл-отметка о том, что галерея полностью распакована.
const galleryMarker = ".complete"

func galleryDone(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, galleryMarker))
	return err == nil
}

// safeZipPath приводит имя внутри архива к безопасному относительному пути.
// Без этого запись вида ../../autorun поместила бы файл мимо папки назначения
// (классический zip slip).
func safeZipPath(name string) (string, bool) {
	name = strings.ReplaceAll(name, `\`, "/")
	var parts []string
	for _, p := range strings.Split(name, "/") {
		switch p {
		case "", ".", "..":
			continue
		}
		parts = append(parts, safeFilename(p))
	}
	if len(parts) == 0 {
		return "", false
	}
	return filepath.Join(parts...), true
}

// stripCommonRoot убирает общий верхний каталог архива: галерея уже лежит
// в своей папке, и лишний уровень вида Album/ только удлиняет путь.
func stripCommonRoot(names []string) string {
	root := ""
	for _, n := range names {
		n = strings.ReplaceAll(n, `\`, "/")
		i := strings.Index(n, "/")
		if i <= 0 {
			return "" // файл в корне архива — общего каталога нет
		}
		if root == "" {
			root = n[:i]
		} else if root != n[:i] {
			return ""
		}
	}
	return root
}

// extractGallery распаковывает архив в destDir и удаляет его.
// Возвращает число извлечённых файлов.
func extractGallery(zipPath, destDir string) (int, error) {
	// Архив закрывается до удаления: Windows не даёт удалить открытый файл.
	count, err := unzipTo(zipPath, destDir)
	if err != nil {
		if errors.Is(err, errBadArchive) {
			// Битый архив не исправится сам: удаляем, чтобы его скачали заново,
			// а не спотыкались об него при каждом запуске.
			if uerr := safeUnlink(zipPath); uerr != nil {
				logWarn("Не удалось удалить повреждённый архив %s: %v", zipPath, uerr)
			}
		}
		return count, err
	}

	// Маркер ставится только после полной распаковки: при обрыве галерея
	// считается незавершённой и будет обработана заново.
	if err := os.WriteFile(filepath.Join(destDir, galleryMarker), nil, 0o644); err != nil {
		return count, err
	}
	if err := safeUnlink(zipPath); err != nil {
		logWarn("Не удалось удалить архив %s: %v", zipPath, err)
	}
	return count, nil
}

func unzipTo(zipPath, destDir string) (int, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", errBadArchive, err)
	}
	defer r.Close()

	names := make([]string, 0, len(r.File))
	for _, f := range r.File {
		if !f.FileInfo().IsDir() {
			names = append(names, f.Name)
		}
	}
	root := stripCommonRoot(names)

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return 0, err
	}

	count := 0
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := f.Name
		if root != "" {
			name = strings.TrimPrefix(strings.ReplaceAll(name, `\`, "/"), root+"/")
		}
		rel, ok := safeZipPath(name)
		if !ok {
			logWarn("Пропущена запись архива с недопустимым именем: %q", f.Name)
			continue
		}
		target := filepath.Join(destDir, rel)

		if _, err := os.Stat(target); err == nil {
			count++
			continue // уже распаковано ранее
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return count, err
		}
		if err := writeZipEntry(f, target); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// zipCorruption помечает ошибки чтения, означающие повреждённый архив.
func zipCorruption(err error) error {
	if errors.Is(err, zip.ErrFormat) || errors.Is(err, zip.ErrChecksum) ||
		errors.Is(err, zip.ErrAlgorithm) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: %v", errBadArchive, err)
	}
	return err
}

func writeZipEntry(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return zipCorruption(err)
	}
	defer rc.Close()

	// Пишем во временный файл и переименовываем: оборванная распаковка
	// не должна оставить обрезанный файл под правильным именем.
	tmp := target + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		_ = safeUnlink(tmp)
		return zipCorruption(err)
	}
	if err := out.Close(); err != nil {
		_ = safeUnlink(tmp)
		return err
	}
	return safeReplace(tmp, target)
}
