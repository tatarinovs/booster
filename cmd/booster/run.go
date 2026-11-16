package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// runOptions — параметры одного запуска загрузки.
type runOptions struct {
	messages      bool
	messageSource bool
	author        string
	token         *AuthToken
	outputDir     string
	isFlat        bool
	workers       int
	noGalleries   bool
	fullCheck     bool
	cancel        *atomic.Bool
	abort         *atomic.Bool
	stats         *Stats
}

func run(ctx context.Context, opts runOptions) error {
	postsErr := runContent(ctx, opts)
	if !opts.messages || opts.cancel.Load() || ctx.Err() != nil {
		return postsErr
	}
	opts.messageSource = true
	messagesErr := runContent(ctx, opts)
	return errors.Join(postsErr, messagesErr)
}

func runContent(ctx context.Context, opts runOptions) error {
	initialFailures := opts.stats.errorCount()
	authorDir := filepath.Join(opts.outputDir, safeFilename(opts.author))
	if opts.messageSource {
		authorDir = filepath.Join(authorDir, "messages")
	}
	if err := os.MkdirAll(authorDir, 0o755); err != nil {
		return fmt.Errorf("не удалось создать папку %s: %w", authorDir, err)
	}
	failedPath := filepath.Join(authorDir, failedFilename)
	syncingPath := filepath.Join(authorDir, ".syncing")
	latestPath := filepath.Join(authorDir, ".latest_post")
	if opts.messageSource {
		latestPath = filepath.Join(authorDir, ".latest_message")
		logInfo("Личная переписка с автором: %s", opts.author)
	}

	logInfo("==================================================")
	logInfo("Автор: %s", opts.author)
	logInfo("Папка: %s", authorDir)
	logInfo("==================================================")

	targetPostID := ""
	if opts.fullCheck {
		logInfo("Полная проверка: просматриваются все посты автора.")
	} else if _, err := os.Stat(syncingPath); err == nil {
		logWarn("Предыдущая загрузка была прервана. Выполняется полная проверка.")
	} else {
		if data, err := os.ReadFile(latestPath); err == nil {
			targetPostID = strings.TrimSpace(string(data))
			if targetPostID != "" {
				logInfo("Быстрая синхронизация: ищем новые посты до ID %s", targetPostID)
			}
		}
	}
	_ = os.WriteFile(syncingPath, []byte(""), 0o644)
	// failed.txt относится только к текущему запуску: без очистки предупреждение
	// в конце срабатывало на файле, оставшемся от прошлых прогонов, — навсегда.
	_ = os.Remove(failedPath)

	externals := newExternalLog(filepath.Join(authorDir, externalFilename))

	client := newBoostyClient(opts.token)
	dlClient := newDownloadHTTPClient(opts.workers)
	gallery := newGalleryClient()

	total := 0
	var err error
	var dialogID, dialogSignature string
	if opts.messageSource {
		dialogID, dialogSignature, err = client.findDialog(ctx, opts.author)
		var herr *httpError
		if errors.As(err, &herr) && herr.status == 404 {
			logInfo("Переписка с автором не найдена.")
			_ = os.Remove(syncingPath)
			return nil
		}
	} else {
		total, err = client.blogPostCount(ctx, opts.author)
	}
	if err != nil {
		return fmt.Errorf("не удалось получить источник загрузки: %w", err)
	}
	if !opts.messageSource {
		logInfo("Постов у автора: %d", total)
	}

	progressInit(total, opts.stats, opts.workers)
	stopTicker := make(chan struct{})
	go progressStartTicker(stopTicker)
	defer close(stopTicker)

	// Очередь с backpressure: не даём пагинации убегать далеко вперёд воркеров.
	// Это критично для boosty — ссылки на видео протухают.
	queue := make(chan DownloadTask, opts.workers*3)

	dl := &downloader{
		author:     opts.author,
		client:     client,
		http:       dlClient,
		stats:      opts.stats,
		cancel:     opts.cancel,
		abort:      opts.abort,
		failedPath: failedPath,
	}

	var wg sync.WaitGroup
	for i := 0; i < opts.workers; i++ {
		wg.Add(1)
		go dl.worker(ctx, i, queue, &wg)
	}

	// Отдельный контекст для пагинации: при мягкой остановке цикл ниже
	// прерывается, и без отмены продюсер навсегда завис бы на отправке в канал.
	postsCtx, postsCancel := context.WithCancel(ctx)
	defer postsCancel()
	var posts <-chan Post
	var errCh <-chan error
	if opts.messageSource {
		posts, errCh = client.iterMessages(postsCtx, dialogID, dialogSignature)
	} else {
		posts, errCh = client.iterPosts(postsCtx, opts.author)
	}

	newestPostID := ""
	processedCount := 0
	var postsErr error
	postsClosed := false

postsLoop:
	for {
		select {
		case post, ok := <-posts:
			if !ok {
				postsClosed = true
				break postsLoop
			}
			if opts.cancel.Load() {
				break postsLoop
			}

			if newestPostID == "" {
				newestPostID = post.ID
			}

			if targetPostID != "" && post.ID == targetPostID {
				logInfo("Достигнут ранее скачанный пост. Остановка поиска.")
				progressSetTotal(processedCount)
				break postsLoop
			}

			processedCount++

			if !post.HasAccess {
				opts.stats.incNoAccess()
				progressPostDone()
				continue
			}
			if opts.messageSource && len(post.Media) == 0 {
				progressPostDone()
				continue
			}

			var postDir string
			if opts.isFlat {
				postDir = authorDir
			} else {
				postDir = postDirName(authorDir, &post)
				if err := os.MkdirAll(postDir, 0o755); err != nil {
					logError("Не удалось создать папку поста %s: %v", postDir, err)
					opts.stats.record("", false, true)
					appendFailed(failedPath, postDir, "https://boosty.to/posts/"+post.ID)
					progressPostDone()
					continue
				}
			}

			var tf string
			if opts.isFlat {
				date := postDate(&post)
				slug := truncateRunes(safeFilename(orDefault(post.Title, "post")), 50)
				tf = filepath.Join(postDir, fmt.Sprintf("%s_%s_%s", date, slug, contentFilename))
			} else {
				tf = filepath.Join(postDir, contentFilename)
			}
			if err := savePostText(tf, &post); err != nil {
				logError("Ошибка сохранения текста поста %s: %v", post.ID, err)
				opts.stats.record("", false, true)
				appendFailed(failedPath, tf, "https://boosty.to/posts/"+post.ID)
			}

			// Внешние видео скачать нельзя — сохраняем ссылки, чтобы пост
			// не выглядел сохранённым целиком, когда часть контента потеряна.
			for _, m := range post.Media {
				if m.Kind == MediaExternal && externals.add(post.ID, m.Title, m.URL) {
					opts.stats.incExternal()
				}
			}

			tasks := makeTasks(&post, postDir, opts.isFlat)
			if opts.messageSource {
				for i := range tasks {
					messageID, mediaID := post.ID, tasks[i].MediaID
					tasks[i].Referer = "https://boosty.to/app/messages/" + dialogID
					tasks[i].PostID = ""
					tasks[i].Resolve = func(ctx context.Context) (string, error) {
						return client.refreshMessageMediaURL(ctx, dialogID, messageID, mediaID)
					}
				}
			}
			if !opts.noGalleries && !opts.messageSource {
				tasks = append(tasks, galleryTasks(ctx, gallery, &post, postDir, opts.stats)...)
			}

			for _, task := range tasks {
				if opts.cancel.Load() {
					break
				}
				select {
				case queue <- task:
				case <-ctx.Done():
					break postsLoop
				}
			}

			progressPostDone()

		case err := <-errCh:
			if err != nil {
				logError("Ошибка получения постов: %v", err)
				postsErr = err
			}
			break postsLoop

		case <-ctx.Done():
			break postsLoop
		}
	}

	// Продюсер кладёт ошибку в errCh до закрытия posts, поэтому, если select
	// выше выбрал закрытый posts, ошибка уже лежит в буфере. Без этой проверки
	// оборванная пагинация засчитывалась как успех: .latest_post сдвигался на
	// самый новый пост, и непросмотренные старые посты терялись навсегда.
	if postsClosed {
		select {
		case err := <-errCh:
			if err != nil {
				logError("Ошибка получения постов: %v", err)
				postsErr = err
			}
		default:
		}
	}

	postsCancel()
	close(queue)
	wg.Wait()
	progressFinish()

	interrupted := opts.abort.Load() || opts.cancel.Load()
	failures := opts.stats.errorCount() - initialFailures

	// Отметку «всё скачано» ставим, только если запуск прошёл полностью и без
	// ошибок. Иначе быстрая синхронизация упёрлась бы в этот ID и до неудавшихся
	// файлов уже никогда не дошла — ошибки становились бы вечными.
	if !interrupted && postsErr == nil && failures == 0 {
		if newestPostID != "" {
			_ = os.WriteFile(latestPath, []byte(newestPostID), 0o644)
		}
		_ = os.Remove(syncingPath)
	}

	if externals.n > 0 {
		logWarn("Внешних видео (YouTube/Vimeo), их нельзя скачать: %d. Ссылки: %s",
			externals.n, externals.path)
	}

	if failures > 0 {
		logWarn("Внимание: не удалось скачать файлов: %d. См. лог: %s", failures, failedPath)
		logWarn("Следующий запуск повторит эти посты — отметка синхронизации не обновлена.")
	}
	return postsErr
}
