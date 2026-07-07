package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// runOptions — параметры одного запуска загрузки.
type runOptions struct {
	author    string
	token     *AuthToken
	outputDir string
	isFlat    bool
	cancel    *atomic.Bool
	abort     *atomic.Bool
	stats     *Stats
}

func run(ctx context.Context, opts runOptions) error {
	authorDir := filepath.Join(opts.outputDir, safeFilename(opts.author))
	if err := os.MkdirAll(authorDir, 0o755); err != nil {
		return fmt.Errorf("не удалось создать папку %s: %w", authorDir, err)
	}
	failedPath := filepath.Join(authorDir, failedFilename)
	syncingPath := filepath.Join(authorDir, ".syncing")
	latestPath := filepath.Join(authorDir, ".latest_post")

	logInfo("==================================================")
	logInfo("Автор: %s", opts.author)
	logInfo("Папка: %s", authorDir)
	logInfo("==================================================")

	targetPostID := ""
	if _, err := os.Stat(syncingPath); err == nil {
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

	client := newBoostyClient(opts.token)
	dlClient := newDownloadHTTPClient()

	total, err := client.blogPostCount(ctx, opts.author)
	if err != nil {
		return fmt.Errorf("не удалось получить число постов: %w", err)
	}
	logInfo("Постов у автора: %d", total)

	progressInit(total, opts.stats)
	stopTicker := make(chan struct{})
	go progressStartTicker(stopTicker)
	defer close(stopTicker)

	// Очередь с backpressure: не даём пагинации убегать далеко вперёд воркеров.
	// Это критично для boosty — ссылки на видео протухают.
	queue := make(chan DownloadTask, workersCount*3)

	var wg sync.WaitGroup
	for i := 0; i < workersCount; i++ {
		wg.Add(1)
		go downloadWorker(i, queue, client, dlClient, opts.stats, opts.cancel, opts.abort, failedPath, &wg)
	}

	posts, errCh := client.iterPosts(ctx, opts.author)

	newestPostID := ""
	processedCount := 0

postsLoop:
	for {
		select {
		case post, ok := <-posts:
			if !ok {
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

			var postDir string
			if opts.isFlat {
				postDir = authorDir
			} else {
				postDir = postDirName(authorDir, &post)
				if err := os.MkdirAll(postDir, 0o755); err != nil {
					logError("Не удалось создать папку поста %s: %v", postDir, err)
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
			if _, err := os.Stat(tf); os.IsNotExist(err) {
				if text := postToMarkdown(post.TextBlocks); text != "" {
					pubDate := time.Unix(post.PublishTime, 0).UTC().Format("02.01.2006 15:04 UTC")
					fullText := fmt.Sprintf("Published %s\n\n%s", pubDate, text)
					if werr := os.WriteFile(tf, []byte(fullText), 0o644); werr != nil {
						logError("Ошибка сохранения текста поста %s: %v", post.ID, werr)
					}
				}
			}

			for _, task := range makeTasks(&post, postDir, opts.isFlat) {
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
			}
			break postsLoop

		case <-ctx.Done():
			break postsLoop
		}
	}

	close(queue)
	wg.Wait()
	progressFinish()

	if !opts.abort.Load() && !opts.cancel.Load() {
		if newestPostID != "" {
			_ = os.WriteFile(latestPath, []byte(newestPostID), 0o644)
		}
		_ = os.Remove(syncingPath)
	}

	if _, err := os.Stat(failedPath); err == nil {
		logWarn("Внимание: часть файлов не удалось скачать. См. лог: %s", failedPath)
	}
	return nil
}
