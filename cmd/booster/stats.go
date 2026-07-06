package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Stats собирает статистику загрузки, потокобезопасна.
type Stats struct {
	mu sync.Mutex

	photos   int
	videos   int
	audio    int
	files    int
	skipped  int
	errors   int
	noAccess int

	processedBytes atomic.Int64
	lastPbarUpdate time.Time
	startTime      time.Time
}

func newStats() *Stats {
	return &Stats{startTime: time.Now()}
}

func (s *Stats) record(mediaType string, skipped, errored bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case errored:
		s.errors++
	case skipped:
		s.skipped++
	case mediaType == "photo":
		s.photos++
	case mediaType == "video":
		s.videos++
	case mediaType == "audio":
		s.audio++
	default:
		s.files++
	}
}

func (s *Stats) incNoAccess() {
	s.mu.Lock()
	s.noAccess++
	s.mu.Unlock()
}

func (s *Stats) addBytes(n int64) {
	s.processedBytes.Add(n)
}

func (s *Stats) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.photos + s.videos + s.audio + s.files
}

func (s *Stats) snapshotBytes() (int64, time.Duration) {
	return s.processedBytes.Load(), time.Since(s.startTime)
}

func (s *Stats) printSummary() {
	s.mu.Lock()
	defer s.mu.Unlock()
	sep := "=================================================="
	fmt.Printf("\n%s\nСтатистика загрузки\n%s\n", sep, sep)

	endTime := time.Now()
	fmt.Printf("  Начато:        %s\n", s.startTime.Format("15:04:05"))
	fmt.Printf("  Завершено:     %s\n", endTime.Format("15:04:05"))
	fmt.Printf("  Затрачено:     %v\n\n", endTime.Sub(s.startTime).Round(time.Second))

	rows := []struct {
		label string
		val   int
	}{
		{"Фото:", s.photos},
		{"Видео:", s.videos},
		{"Аудио:", s.audio},
		{"Файлы:", s.files},
		{"Пропущено:", s.skipped},
		{"Нет доступа:", s.noAccess},
		{"Ошибок:", s.errors},
		{"Итого:", s.photos + s.videos + s.audio + s.files},
	}
	for _, r := range rows {
		if r.val != 0 || r.label == "Итого:" {
			fmt.Printf("  %-14s %d\n", r.label, r.val)
		}
	}
	fmt.Println(sep)
}
