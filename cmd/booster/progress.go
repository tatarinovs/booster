package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// maxWorkerLines ограничивает число строк воркеров в индикаторе прогресса.
const maxWorkerLines = 10

// progressState хранит данные для индикатора прогресса.
type progressState struct {
	totalPosts       int
	donePosts        int
	stats            *Stats
	workers          map[int]string // id воркера → текущий файл
	workerCount      int
	lastPrintedLines int
	finished         bool
}

var progress = &progressState{workers: map[int]string{}}

func progressInit(total int, stats *Stats, workers int) {
	logMu.Lock()
	defer logMu.Unlock()
	progress.totalPosts = total
	progress.stats = stats
	progress.workerCount = workers
	progress.lastPrintedLines = 0
	progress.finished = false
}

func progressSetTotal(total int) {
	logMu.Lock()
	progress.totalPosts = total
	logMu.Unlock()
	progressRepaint()
}

func progressPostDone() {
	logMu.Lock()
	progress.donePosts++
	logMu.Unlock()
	progressRepaint()
}

func progressWorkerSet(id int, name string) {
	logMu.Lock()
	if name == "" {
		delete(progress.workers, id)
	} else {
		progress.workers[id] = name
	}
	logMu.Unlock()
	progressRepaint()
}

// progressClearLocked стирает текущий прогресс-бар с экрана, чтобы лог не перекрывался.
func progressClearLocked() {
	if !ansiEnabled {
		return
	}
	if progress.lastPrintedLines > 0 {
		// Поднимаемся на lastPrintedLines вверх и стираем всё до конца экрана
		fmt.Fprintf(os.Stderr, "\r\033[%dA\033[J", progress.lastPrintedLines)
		progress.lastPrintedLines = 0
	} else {
		fmt.Fprintf(os.Stderr, "\r\033[K")
	}
}

// progressRepaint перерисовывает строку прогресса (блокирует logMu самостоятельно).
func progressRepaint() {
	logMu.Lock()
	defer logMu.Unlock()
	progressRepaintLocked()
}

// progressRepaintLocked перерисовывает строку прогресса; вызывающий уже держит logMu.
func progressRepaintLocked() {
	// Без консоли перерисовывать нечего: escape-последовательности в файле
	// только мешают, а сам ход работы виден по строкам лога.
	if !ansiEnabled || progress.finished {
		return
	}
	if progress.totalPosts == 0 && progress.donePosts == 0 {
		return
	}

	pct := 0.0
	if progress.totalPosts > 0 {
		pct = float64(progress.donePosts) / float64(progress.totalPosts) * 100
	}

	// Прогресс-бар
	barWidth := 20
	filled := 0
	if progress.totalPosts > 0 {
		filled = int(float64(barWidth) * float64(progress.donePosts) / float64(progress.totalPosts))
		if filled > barWidth {
			filled = barWidth
		}
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)

	var totalFmt, speedFmt string
	if progress.stats != nil {
		bytes, elapsed := progress.stats.snapshotBytes()
		totalFmt = formatBytes(float64(bytes))
		secs := elapsed.Seconds()
		if secs < 1 {
			secs = 1
		}
		speedFmt = formatBytes(float64(bytes) / secs)
	}

	var lines []string

	// 1. Основная часть
	main := fmt.Sprintf(" Посты: %s %d/%d %3.0f%% │ %s │ %s/s",
		bar, progress.donePosts, progress.totalPosts, pct, totalFmt, speedFmt)
	lines = append(lines, main)

	// 2. Воркеры. При большом -workers список не должен занимать весь экран,
	// поэтому показываем только первые maxWorkerLines строк.
	termW := terminalWidth()
	shown := progress.workerCount
	if shown > maxWorkerLines {
		shown = maxWorkerLines
	}
	for i := 0; i < shown; i++ {
		name := progress.workers[i]
		if name == "" {
			lines = append(lines, fmt.Sprintf("  w%d: [ожидание...]", i))
		} else {
			prefix := fmt.Sprintf("  w%d: ", i)
			maxName := termW - len(prefix) - 2
			if maxName < 5 {
				maxName = 5
			}
			lines = append(lines, prefix+truncateForDisplay(name, maxName))
		}
	}
	if rest := progress.workerCount - shown; rest > 0 {
		active := 0
		for id := range progress.workers {
			if id >= shown {
				active++
			}
		}
		lines = append(lines, fmt.Sprintf("  … ещё %d воркеров, активных: %d", rest, active))
	}

	// Возвращаемся наверх
	if progress.lastPrintedLines > 0 {
		fmt.Fprintf(os.Stderr, "\r\033[%dA", progress.lastPrintedLines)
	}

	// Печатаем новые строки
	for i, l := range lines {
		// Обрезаем до ширины терминала, чтобы избежать случайного переноса
		safeLine := fitToWidth(l, termW-1)
		if i == len(lines)-1 {
			fmt.Fprintf(os.Stderr, "\r\033[K%s", safeLine)
		} else {
			fmt.Fprintf(os.Stderr, "\r\033[K%s\n", safeLine)
		}
	}

	progress.lastPrintedLines = len(lines) - 1
}

// truncateForDisplay обрезает строку до n рун (не байтов), безопасно для UTF-8.
func truncateForDisplay(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(runes[:n-1]) + "…"
}

func isCJKOrWide(r rune) bool {
	return (r >= 0x1100 && r <= 0x115F) || // Hangul Jamo
		(r >= 0x2E80 && r <= 0x303E) || // CJK Radicals
		(r >= 0x3040 && r <= 0x33BF) || // Кана и CJK-символы
		(r >= 0x3400 && r <= 0x4DBF) || // CJK Extension A
		(r >= 0x4E00 && r <= 0x9FFF) || // CJK Unified Ideographs — основной блок иероглифов
		(r >= 0xA000 && r <= 0xA4CF) || // Yi
		(r >= 0xAC00 && r <= 0xD7A3) || // Hangul Syllables
		(r >= 0xF900 && r <= 0xFAFF) || // CJK Compatibility
		(r >= 0xFE30 && r <= 0xFE6F) || // CJK Compatibility Forms
		(r >= 0xFF01 && r <= 0xFF60) || // Fullwidth Forms
		(r >= 0xFFE0 && r <= 0xFFE6) || // Fullwidth Signs
		(r >= 0x20000 && r <= 0x2FFFF) || // CJK Extension B+
		(r >= 0x30000 && r <= 0x3FFFF) // CJK Extension G+
}

// fitToWidth обрезает строку до указанного числа колонок терминала.
func fitToWidth(s string, maxW int) string {
	if maxW <= 0 {
		return ""
	}
	w := 0
	for i, r := range s {
		rw := 1
		if r >= 0x1100 && isCJKOrWide(r) {
			rw = 2
		}
		if w+rw > maxW {
			return s[:i]
		}
		w += rw
	}
	return s
}

func formatBytes(n float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%dB", int(n))
	}
	return fmt.Sprintf("%.1f%s", n, units[i])
}

// progressStartTicker периодически перерисовывает строку прогресса (для обновления скорости).
func progressStartTicker(stop <-chan struct{}) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			progressRepaint()
		case <-stop:
			return
		}
	}
}

// progressFinish завершает индикатор прогресса, переводя курсор на новую строку.
func progressFinish() {
	logMu.Lock()
	defer logMu.Unlock()
	if progress.finished {
		return
	}
	progress.finished = true
	if ansiEnabled && progress.lastPrintedLines > 0 {
		fmt.Fprintln(os.Stderr)
		progress.lastPrintedLines = 0
	}
}
