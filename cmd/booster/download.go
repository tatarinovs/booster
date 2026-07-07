package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	workersCount       = 5
	maxDownloadRetries = 5
	downloadRetryDelay = 3 * time.Second
)

var bufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 256*1024)
		return &buf
	},
}

func newDownloadHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
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

// retryDelay логирует предупреждение и засыпает перед следующей попыткой.
func retryDelay(attempt int, name string, err error) {
	warnRetry(attempt, name, err)
	if attempt < maxDownloadRetries {
		time.Sleep(downloadRetryDelay)
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

// downloadWorker забирает задачи из канала и скачивает их последовательно.
func downloadWorker(id int, tasks <-chan DownloadTask, client *BoostyClient, dlClient *http.Client,
	stats *Stats, cancelFlag *atomic.Bool, abortFlag *atomic.Bool, failedPath string, wg *sync.WaitGroup) {
	defer wg.Done()
	for task := range tasks {
		if cancelFlag.Load() {
			continue // задача отброшена (мягкая остановка)
		}
		progressWorkerSet(id, filepath.Base(task.Dest))
		func() {
			defer func() {
				if r := recover(); r != nil {
					logError("Воркер %d — неожиданная ошибка для %s: %v", id, task.Dest, r)
					stats.record(task.MediaType, false, true)
				}
			}()
			downloadOne(task, client, dlClient, stats, abortFlag, failedPath)
		}()
		progressWorkerSet(id, "")
	}
}

// downloadOne скачивает один файл с поддержкой докачки и повторных попыток.
func downloadOne(task DownloadTask, client *BoostyClient, dlClient *http.Client,
	stats *Stats, abortFlag *atomic.Bool, failedPath string) {

	if _, err := os.Stat(task.Dest); err == nil {
		stats.record(task.MediaType, true, false)
		return
	}

	if err := os.MkdirAll(filepath.Dir(task.Dest), 0o755); err != nil {
		logError("Не удалось создать папку для %s: %v", task.Dest, err)
	}

	part := task.Dest + ".part"
	// partSize — текущий размер .part файла (используется как offset для Range и счётчик).
	var partSize int64
	if fi, err := os.Stat(part); err == nil {
		partSize = fi.Size()
	}

	parsedURL, _ := url.Parse(task.URL)
	isCDN := parsedURL == nil || !strings.Contains(parsedURL.Hostname(), "boosty.to")

	baseHeaders := client.headers
	if isCDN {
		baseHeaders = map[string]string{"User-Agent": client.headers["User-Agent"]}
	}

	deletedPart := false

	for attempt := 1; attempt <= maxDownloadRetries; attempt++ {
		if abortFlag.Load() {
			return // .part остаётся — при следующем запуске докачается
		}

		req, err := http.NewRequest(http.MethodGet, task.URL, nil)
		if err != nil {
			retryDelay(attempt, filepath.Base(task.Dest), err)
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
			retryDelay(attempt, filepath.Base(task.Dest), err)
			continue
		}

		if resp.StatusCode == 400 || resp.StatusCode == 403 {
			resp.Body.Close()
			if partSize > 0 && !deletedPart {
				logInfo("CDN отклонил Range для %s, качаем заново", filepath.Base(task.Dest))
				_ = safeUnlink(part)
				partSize = 0
				deletedPart = true
				continue // сразу повторяем без sleep
			}
			logError("HTTP %d (протухший URL?): %s", resp.StatusCode, task.URL)
			break
		}
		if resp.StatusCode == 404 {
			resp.Body.Close()
			logError("Файл не найден (404): %s", task.URL)
			break
		}
		if resp.StatusCode >= 400 {
			resp.Body.Close()
			retryDelay(attempt, filepath.Base(task.Dest), fmt.Errorf("HTTP %d", resp.StatusCode))
			continue
		}

		var expected int64 = -1
		var flags int
		if resp.StatusCode == 206 {
			if resp.ContentLength > 0 {
				expected = partSize + resp.ContentLength
			}
			flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
		} else {
			if task.IsVideo {
				expected = resp.ContentLength
			}
			partSize = 0
			flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		}

		f, ferr := os.OpenFile(part, flags, 0o644)
		if ferr != nil {
			resp.Body.Close()
			retryDelay(attempt, filepath.Base(task.Dest), ferr)
			continue
		}

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
				if _, werr := f.Write(buf[:n]); werr != nil {
					writeErr = werr
					break
				}
				partSize += int64(n)
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
		f.Close()
		resp.Body.Close()
		bufferPool.Put(bufPtr)

		if aborted {
			return // .part остаётся для докачки при следующем запуске
		}

		if writeErr != nil {
			retryDelay(attempt, filepath.Base(task.Dest), writeErr)
			continue
		}

		if task.IsVideo && expected > 0 && partSize < expected {
			retryDelay(attempt, filepath.Base(task.Dest),
				fmt.Errorf("неполный файл: ожидалось %d байт, получено %d", expected, partSize))
			continue
		}

		if err := safeReplace(part, task.Dest); err != nil {
			logError("Ошибка переименования %s: %v", task.Dest, err)
		}
		stats.record(task.MediaType, false, false)
		return // успех
	}

	// Все попытки исчерпаны (или произошёл ранний break)
	logError("Не удалось скачать после %d попыток: %s", maxDownloadRetries, task.URL)
	appendFailed(failedPath, task.Dest, task.URL)
	stats.record(task.MediaType, false, true)
}
