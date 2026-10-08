package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultWorkers     = 5
	maxWorkers         = 64
	maxDownloadRetries = 5
)

// downloadRetryDelay — пауза между попытками. Переменная, а не константа,
// чтобы тесты не ждали реальные секунды.
var downloadRetryDelay = 3 * time.Second

// downloadIdleTimeout — сколько ждать очередной порции данных, прежде чем
// считать соединение зависшим. Общего таймаута у клиента нет (файлы бывают
// большими), и без этой проверки замолчавший CDN навсегда занимал воркер.
var downloadIdleTimeout = 60 * time.Second

// contentRangeTotal достаёт полный размер файла из Content-Range
// («bytes 0-99/1234» или «bytes */1234»).
func contentRangeTotal(v string) (int64, bool) {
	i := strings.LastIndexByte(v, '/')
	if i < 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v[i+1:]), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

var bufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 256*1024)
		return &buf
	},
}

func newDownloadHTTPClient(workers int) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	// По умолчанию на хост держится всего 2 простаивающих соединения, из-за чего
	// воркеры постоянно переустанавливают TLS-сессию к одному и тому же CDN.
	transport.MaxIdleConnsPerHost = workers
	return &http.Client{
		Timeout:   0, // без общего таймаута — файлы могут быть большими
		Transport: transport,
	}
}

func warnRetry(attempt int, name string, err error) {
	if attempt < maxDownloadRetries {
		logWarn("Попытка %d/%d для %s: %v", attempt, maxDownloadRetries, name, err)
	}
}

// retryDelay логирует предупреждение и засыпает перед следующей попыткой,
// прерываясь по отмене контекста.
func retryDelay(ctx context.Context, attempt int, name string, err error) {
	warnRetry(attempt, name, err)
	if attempt < maxDownloadRetries {
		sleepCtx(ctx, downloadRetryDelay)
	}
}

func appendFailed(failedPath, dest, url string) {
	f, err := os.OpenFile(failedPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		logError("Не удалось записать в %s: %v", failedPath, err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\t%s\n", dest, url)
}

// downloader держит общие для всех воркеров зависимости.
type downloader struct {
	author     string
	client     *BoostyClient
	http       *http.Client
	stats      *Stats
	cancel     *atomic.Bool
	abort      *atomic.Bool
	failedPath string
}

// worker забирает задачи из канала и скачивает их последовательно.
func (d *downloader) worker(ctx context.Context, id int, tasks <-chan DownloadTask, wg *sync.WaitGroup) {
	defer wg.Done()
	for task := range tasks {
		if d.cancel.Load() {
			// Мягкая остановка: задача отброшена. Учитываем её явно, иначе
			// сводка молча недосчитывает файлы, стоявшие в очереди.
			d.stats.incCancelled()
			continue
		}
		progressWorkerSet(id, filepath.Base(task.Dest))
		func() {
			defer func() {
				if r := recover(); r != nil {
					logError("Воркер %d — неожиданная ошибка для %s: %v", id, task.Dest, r)
					d.stats.record(task.MediaType, false, true)
				}
			}()
			d.downloadOne(ctx, task)
		}()
		progressWorkerSet(id, "")
	}
}

// downloadOne скачивает один файл с поддержкой докачки и повторных попыток.
func (d *downloader) downloadOne(ctx context.Context, task DownloadTask) {
	client, dlClient, stats, abortFlag, failedPath := d.client, d.http, d.stats, d.abort, d.failedPath

	if _, err := os.Stat(task.Dest); err == nil {
		// Файл на месте, но обработка могла не доехать: архив галереи,
		// скачанный в прошлый раз, мог остаться нераспакованным. Иначе он
		// пролежал бы мёртвым грузом, а фото так и не появились бы.
		if task.AfterDownload == nil {
			stats.record(task.MediaType, true, false)
			return
		}
		err := task.AfterDownload(task.Dest)
		if err == nil {
			stats.record(task.MediaType, true, false)
			return
		}
		if _, serr := os.Stat(task.Dest); serr == nil {
			logError("Обработка %s не удалась: %v", filepath.Base(task.Dest), err)
			stats.record(task.MediaType, false, true)
			return
		}
		// Обработчик удалил испорченный файл (битый архив) — скачиваем заново,
		// иначе ошибка повторялась бы при каждом запуске.
		logWarn("%s повреждён, скачиваем заново: %v", filepath.Base(task.Dest), err)
	}

	if err := os.MkdirAll(filepath.Dir(task.Dest), 0o755); err != nil {
		logError("Не удалось создать папку для %s: %v", task.Dest, err)
		appendFailed(failedPath, task.Dest, task.URL)
		stats.record(task.MediaType, false, true)
		return
	}

	// Задачи из внешних галерей приходят без готовой ссылки: её выдаёт
	// отдельный запрос, и делать его заранее для всей очереди бессмысленно —
	// ссылка протухнет, пока задача ждёт своей очереди.
	if task.URL == "" {
		if task.Resolve == nil {
			logError("Задача без ссылки: %s", task.Dest)
			stats.record(task.MediaType, false, true)
			return
		}
		u, err := task.Resolve(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Платная галерея или ссылка не на галерею — это свойство материала,
			// а не сбой. Как ошибка она навсегда блокировала бы отметку
			// синхронизации и давала код возврата 2 при каждом запуске.
			if errors.Is(err, errUnavailable) {
				logWarn("Пропущено %s: %v", filepath.Base(task.Dest), err)
				stats.incUnavailable()
				return
			}
			logError("Не удалось получить ссылку для %s: %v", filepath.Base(task.Dest), err)
			appendFailed(failedPath, task.Dest, "(ссылка не получена)")
			stats.record(task.MediaType, false, true)
			return
		}
		task.URL = u
	}

	part := task.Dest + ".part"
	// partSize — текущий размер .part файла (используется как offset для Range и счётчик).
	var partSize int64
	if fi, err := os.Stat(part); err == nil {
		partSize = fi.Size()
	}
	// counted — сколько байт этой задачи уже учтено в общей статистике.
	// Нужно, чтобы при перезаписи .part вычесть отброшенное и не завышать итог.
	var counted int64
	discard := func() {
		if counted > 0 {
			stats.addBytes(-counted)
			counted = 0
		}
	}

	// headersFor выбирает заголовки под конкретную ссылку: токен и cookie
	// отдаём только собственным хостам boosty.to, на CDN — один User-Agent.
	headersFor := func(rawURL string) map[string]string {
		parsed, err := url.Parse(rawURL)
		if err == nil && parsed != nil && isBoostyHost(parsed.Hostname()) {
			return client.headers
		}
		return map[string]string{"User-Agent": client.headers["User-Agent"]}
	}
	baseHeaders := headersFor(task.URL)

	// finish превращает скачанный целиком .part в итоговый файл и запускает
	// постобработку. Ошибки учитывает сам.
	finish := func() {
		if err := safeReplace(part, task.Dest); err != nil {
			logError("Ошибка переименования %s: %v", task.Dest, err)
			appendFailed(failedPath, task.Dest, task.URL)
			stats.record(task.MediaType, false, true)
			return
		}
		if task.AfterDownload != nil {
			if err := task.AfterDownload(task.Dest); err != nil {
				logError("Обработка %s не удалась: %v", filepath.Base(task.Dest), err)
				appendFailed(failedPath, task.Dest, task.URL)
				stats.record(task.MediaType, false, true)
				return
			}
		}
		stats.record(task.MediaType, false, false)
	}

	deletedPart := false
	refreshed := false

	for attempt := 1; attempt <= maxDownloadRetries; attempt++ {
		if abortFlag.Load() {
			return // .part остаётся — при следующем запуске докачается
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, task.URL, nil)
		if err != nil {
			retryDelay(ctx, attempt, filepath.Base(task.Dest), err)
			continue
		}
		for k, v := range baseHeaders {
			req.Header.Set(k, v)
		}
		if partSize > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", partSize))
		}
		if task.Referer != "" {
			req.Header.Set("Referer", task.Referer)
		}

		resp, err := dlClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return // отмена — .part сохраняем для докачки
			}
			retryDelay(ctx, attempt, filepath.Base(task.Dest), err)
			continue
		}

		if resp.StatusCode == 400 || resp.StatusCode == 403 {
			resp.Body.Close()
			// Подпись ссылки протухла, пока задача ждала в очереди. Перезапрашиваем
			// пост и берём свежую ссылку — на большом блоге это основная причина
			// потерь, и без обновления файл ушёл бы в failed.txt.
			//
			// Ссылка обновляется раньше, чем выбрасывается .part: иначе после обрыва
			// посреди запуска протухшая подпись стоила бы всего уже скачанного
			// куска, хотя дело было не в Range.
			if !refreshed && (task.Resolve != nil || (task.PostID != "" && task.MediaID != "")) {
				refreshed = true
				var fresh string
				var rerr error
				if task.Resolve != nil {
					fresh, rerr = task.Resolve(ctx)
				} else {
					fresh, rerr = client.refreshMediaURL(ctx, d.author, task.PostID, task.MediaID)
				}
				switch {
				case rerr != nil:
					logWarn("Не удалось обновить ссылку для %s: %v", filepath.Base(task.Dest), rerr)
				case fresh != task.URL:
					logInfo("Ссылка протухла, обновлена: %s", filepath.Base(task.Dest))
					task.URL = fresh
					baseHeaders = headersFor(fresh)
					continue // повторяем со свежей ссылкой
				}
			}
			if partSize > 0 && !deletedPart {
				logInfo("CDN отклонил Range для %s, качаем заново", filepath.Base(task.Dest))
				_ = safeUnlink(part)
				partSize = 0
				discard()
				deletedPart = true
				continue // сразу повторяем без sleep
			}
			logError("HTTP %d (протухший URL?): %s", resp.StatusCode, task.URL)
			break
		}
		if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && partSize > 0 {
			resp.Body.Close()
			// .part уже скачан целиком, но в прошлый раз не был переименован
			// (процесс прервали или антивирус держал файл). Без этой ветки сервер
			// отвечал бы 416 при каждом запуске и файл не появился бы никогда.
			if total, ok := contentRangeTotal(resp.Header.Get("Content-Range")); ok && total == partSize {
				finish()
				return
			}
			logInfo("Сервер отклонил докачку %s, качаем заново", filepath.Base(task.Dest))
			_ = safeUnlink(part)
			partSize = 0
			discard()
			deletedPart = true
			continue
		}
		if resp.StatusCode == 404 {
			resp.Body.Close()
			logError("Файл не найден (404): %s", task.URL)
			break
		}
		if resp.StatusCode >= 400 {
			resp.Body.Close()
			retryDelay(ctx, attempt, filepath.Base(task.Dest), fmt.Errorf("HTTP %d", resp.StatusCode))
			continue
		}

		// expected — ожидаемый итоговый размер. Считаем его для любого типа,
		// а не только для видео: обрезанная картинка иначе переименовывалась бы
		// в финальный файл и навсегда считалась скачанной.
		var expected int64 = -1
		var flags int
		if resp.StatusCode == 206 {
			if resp.ContentLength > 0 {
				expected = partSize + resp.ContentLength
			}
			flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
		} else {
			expected = resp.ContentLength
			partSize = 0
			discard()
			flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		}

		f, ferr := os.OpenFile(part, flags, 0o644)
		if ferr != nil {
			resp.Body.Close()
			retryDelay(ctx, attempt, filepath.Base(task.Dest), ferr)
			continue
		}

		// Сторож закрывает тело ответа, если данные перестали приходить:
		// закрытие из другой горутины прерывает заблокированный Read.
		var stalled atomic.Bool
		watchdog := time.AfterFunc(downloadIdleTimeout, func() {
			stalled.Store(true)
			resp.Body.Close()
		})

		bufPtr := bufferPool.Get().(*[]byte)
		buf := *bufPtr
		var writeErr error
		aborted := false
		for {
			if abortFlag.Load() {
				aborted = true
				break
			}
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				watchdog.Reset(downloadIdleTimeout)
				if _, werr := f.Write(buf[:n]); werr != nil {
					writeErr = werr
					break
				}
				partSize += int64(n)
				counted += int64(n)
				stats.addBytes(int64(n))
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				writeErr = rerr
				break
			}
		}
		watchdog.Stop()
		if cerr := f.Close(); cerr != nil && writeErr == nil {
			writeErr = cerr
		}
		resp.Body.Close()
		bufferPool.Put(bufPtr)
		if stalled.Load() && writeErr != nil {
			writeErr = fmt.Errorf("нет данных дольше %v, соединение зависло", downloadIdleTimeout)
		}

		if aborted {
			return // .part остаётся для докачки при следующем запуске
		}

		if writeErr != nil {
			if ctx.Err() != nil {
				return
			}
			retryDelay(ctx, attempt, filepath.Base(task.Dest), writeErr)
			continue
		}

		if expected > 0 && partSize < expected {
			retryDelay(ctx, attempt, filepath.Base(task.Dest),
				fmt.Errorf("неполный файл: ожидалось %d байт, получено %d", expected, partSize))
			continue
		}

		finish()
		return
	}

	// Все попытки исчерпаны (или произошёл ранний break)
	logError("Не удалось скачать после %d попыток: %s", maxDownloadRetries, task.URL)
	appendFailed(failedPath, task.Dest, task.URL)
	stats.record(task.MediaType, false, true)
}
