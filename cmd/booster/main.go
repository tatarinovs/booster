//go:generate goversioninfo -icon=favicon.ico -manifest=booster.exe.manifest -64=true

package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
)

// version подставляется при сборке через -ldflags "-X main.version=v1.2.3".
// В обычной сборке остаётся dev — так видно, что бинарник собран не из релиза.
var version = "dev"

func scriptDir() string {
	exe, err := os.Executable()
	if err != nil {
		wd, _ := os.Getwd()
		return wd
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(exe))
	if err != nil {
		return filepath.Dir(exe)
	}
	return dir
}

func main() {
	author := flag.String("author", "", "Ник автора или ссылка на профиль")
	flag.StringVar(author, "a", "", "Ник автора или ссылка на профиль (сокращение)")
	output := flag.String("output", "", "Папка для загрузок (по умолчанию — рядом со скриптом)")
	flag.StringVar(output, "o", "", "Папка для загрузок (сокращение)")
	flat := flag.Bool("flat", false, "Все файлы в одну папку без подпапок по постам")
	flag.BoolVar(flat, "f", false, "Все файлы в одну папку без подпапок по постам (сокращение)")
	noWait := flag.Bool("no-wait", false, "Не ждать Enter перед выходом (для скриптов)")
	workers := flag.Int("workers", defaultWorkers,
		fmt.Sprintf("Число параллельных загрузок (1..%d)", maxWorkers))
	flag.IntVar(workers, "w", defaultWorkers, "Число параллельных загрузок (сокращение)")
	noGalleries := flag.Bool("no-galleries", false,
		"Не скачивать фото из внешних галерей, на которые ссылаются посты")
	showVersion := flag.Bool("version", false, "Показать версию и выйти")
	flag.Parse()

	if *showVersion {
		fmt.Printf("booster %s\n", version)
		return
	}

	if *workers < 1 || *workers > maxWorkers {
		fmt.Printf("Число воркеров должно быть от 1 до %d (указано %d).\n", maxWorkers, *workers)
		os.Exit(1)
	}

	dir := scriptDir()

	nick := extractNickname(*author)
	if nick == "" {
		fmt.Print("Ник автора (boosty.to/...): ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		nick = extractNickname(line)
	}
	if nick == "" {
		fmt.Println("Ник автора не указан.")
		os.Exit(1)
	}

	outputDir := dir
	if *output != "" {
		if abs, err := filepath.Abs(*output); err == nil {
			outputDir = abs
		} else {
			outputDir = *output
		}
	}

	token := loadToken(dir)
	if token != nil && token.IsExpired() {
		logWarn("Токен истёк.")
		token = nil
	}
	if token == nil {
		token = promptToken(dir)
	}
	if token != nil {
		exp := time.Unix(token.ExpiresAt, 0).UTC().Format("2006-01-02 15:04 MST")
		logInfo("Авторизован (токен до %s).", exp)
	}

	var cancelFlag, abortFlag atomic.Bool
	stats := newStats()

	ctx, ctxCancel := context.WithCancel(context.Background())
	defer ctxCancel()

	var runFailed atomic.Bool

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Паника в парсинге ответа API не должна уносить весь процесс без сводки.
		defer func() {
			if r := recover(); r != nil {
				logError("Неожиданная ошибка: %v", r)
				runFailed.Store(true)
			}
		}()
		opts := runOptions{
			author:      nick,
			token:       token,
			outputDir:   outputDir,
			isFlat:      *flat,
			workers:     *workers,
			noGalleries: *noGalleries,
			cancel:      &cancelFlag,
			abort:       &abortFlag,
			stats:       stats,
		}
		if err := run(ctx, opts); err != nil {
			logError("Критическая ошибка: %v", err)
			runFailed.Store(true)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	interrupts := 0
loop:
	for {
		select {
		case <-done:
			break loop
		case <-sigCh:
			interrupts++
			switch interrupts {
			case 1:
				logWarn("[СТОП] Завершаем текущие загрузки... (повторный Ctrl+C — прервать немедленно)")
				cancelFlag.Store(true)
			case 2:
				logError("[ПРИНУДИТЕЛЬНО] Прерываем активные загрузки...")
				abortFlag.Store(true)
				ctxCancel()
			default:
				// Третий Ctrl+C — выходим немедленно без ожидания
				os.Exit(1)
			}
		}
	}

	stats.printSummary()

	// Ждём Enter только в интерактивном режиме: при запуске из скрипта или
	// планировщика процесс иначе висел бы вечно.
	if !*noWait && isTerminal(os.Stdin) {
		fmt.Print("\nНажмите Enter для выхода...")
		reader := bufio.NewReader(os.Stdin)
		_, _ = reader.ReadString('\n')
	}

	os.Exit(exitCode(runFailed.Load(), interrupts > 0, stats))
}

// exitCode переводит итог работы в код возврата, чтобы вызывающий скрипт
// мог отличить успех от частичной или полной неудачи.
func exitCode(failed, interrupted bool, stats *Stats) int {
	switch {
	case failed:
		return 1
	case stats.errorCount() > 0:
		return 2
	case interrupted:
		return 130 // прервано по сигналу, как принято в шелле
	default:
		return 0
	}
}
