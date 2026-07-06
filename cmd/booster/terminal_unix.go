//go:build !windows

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
)

var cachedWidth atomic.Int32

func init() {
	// Инициализируем кэш ширины один раз при старте
	updateTerminalWidth()

	// Подписываемся на SIGWINCH (изменение размера окна терминала)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	go func() {
		for range sigCh {
			updateTerminalWidth()
		}
	}()
}

func updateTerminalWidth() {
	// 1. Пробуем переменную окружения (часто ставится шеллом)
	if cols := os.Getenv("COLUMNS"); cols != "" {
		if c, err := strconv.Atoi(cols); err == nil && c > 0 {
			cachedWidth.Store(int32(c))
			return
		}
	}

	// 2. Пробуем через tput
	cmd := exec.Command("tput", "cols")
	cmd.Stdin = os.Stdin
	if out, err := cmd.Output(); err == nil {
		if c, err := strconv.Atoi(strings.TrimSpace(string(out))); err == nil && c > 0 {
			cachedWidth.Store(int32(c))
			return
		}
	}

	// 3. Пробуем через stty
	cmd = exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	if out, err := cmd.Output(); err == nil {
		parts := strings.Fields(string(out))
		if len(parts) >= 2 {
			if c, err := strconv.Atoi(parts[1]); err == nil && c > 0 {
				cachedWidth.Store(int32(c))
				return
			}
		}
	}

	// Фолбэк по умолчанию, если ничего не сработало
	cachedWidth.Store(100)
}

func terminalWidth() int {
	w := int(cachedWidth.Load())
	if w <= 0 {
		return 100
	}
	return w
}
