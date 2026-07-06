package main

import (
	"fmt"
	"os"
	"sync"
)

var logMu sync.Mutex

// clearAndLog печатает сообщение, предварительно затерев текущую строку прогресса
// (аналог tqdm.write в питоновской версии — чтобы не ломать индикатор прогресса).
func clearAndLog(level, format string, args ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	msg := fmt.Sprintf(format, args...)

	var color string
	switch level {
	case "WARNING":
		color = "\033[33m" // Желтый
	case "ERROR":
		color = "\033[1;31m" // Ярко-красный
	}
	reset := "\033[0m"

	progressClearLocked()
	if color != "" {
		fmt.Fprintf(os.Stderr, "%s%s%s\n", color, msg, reset)
	} else {
		fmt.Fprintln(os.Stderr, msg)
	}
	progressRepaintLocked()
}

func logInfo(format string, args ...any)  { clearAndLog("INFO", format, args...) }
func logWarn(format string, args ...any)  { clearAndLog("WARNING", format, args...) }
func logError(format string, args ...any) { clearAndLog("ERROR", format, args...) }
