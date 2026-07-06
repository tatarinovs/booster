//go:generate goversioninfo -icon=favicon.ico -manifest=booster.exe.manifest

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
	flag.Parse()

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

	done := make(chan struct{})
	go func() {
		defer close(done)
		opts := runOptions{
			author:    nick,
			token:     token,
			outputDir: outputDir,
			isFlat:    *flat,
			cancel:    &cancelFlag,
			abort:     &abortFlag,
			stats:     stats,
		}
		if err := run(ctx, opts); err != nil {
			logError("Критическая ошибка: %v", err)
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
				fmt.Fprint(os.Stderr, "\r\033[K\033[33m[СТОП] Завершаем текущие загрузки... (повторный Ctrl+C — прервать немедленно)\033[0m\n")
				cancelFlag.Store(true)
			case 2:
				fmt.Fprint(os.Stderr, "\r\033[K\033[1;31m[ПРИНУДИТЕЛЬНО] Прерываем активные загрузки...\033[0m\n")
				abortFlag.Store(true)
				ctxCancel()
			default:
				// Третий Ctrl+C — выходим немедленно без ожидания
				os.Exit(1)
			}
		}
	}

	stats.printSummary()

	fmt.Print("\nНажмите Enter для выхода...")
	reader := bufio.NewReader(os.Stdin)
	_, _ = reader.ReadString('\n')
}
