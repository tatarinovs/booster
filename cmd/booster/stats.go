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

	photos    int
	videos    int
	audio     int
	files     int
	gallery   int
	galleries int
	skipped   int
	errors    int
	noAccess  int
	cancelled int
	external  int

	processedBytes atomic.Int64
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
	case mediaType == "gallery":
		// Сам архив в счётчике не нужен: фото считает addGalleryFiles
		// по факту распаковки.
	default:
		s.files++
	}
}

func (s *Stats) incNoAccess() {
	s.mu.Lock()
	s.noAccess++
	s.mu.Unlock()
}

// incCancelled учитывает задачу, отброшенную при мягкой остановке.
func (s *Stats) incCancelled() {
	s.mu.Lock()
	s.cancelled++
	s.mu.Unlock()
}

// incExternal учитывает внешнее видео, ссылка на которое сохранена в файл.
func (s *Stats) incExternal() {
	s.mu.Lock()
	s.external++
	s.mu.Unlock()
}

// incGallery учитывает найденную внешнюю галерею.
func (s *Stats) incGallery() {
	s.mu.Lock()
	s.galleries++
	s.mu.Unlock()
}

// addGalleryFiles учитывает фото, извлечённые из архива галереи.
func (s *Stats) addGalleryFiles(n int) {
	s.mu.Lock()
	s.gallery += n
	s.mu.Unlock()
}

// errorCount возвращает число неудавшихся загрузок.
func (s *Stats) errorCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.errors
}

func (s *Stats) addBytes(n int64) {
	s.processedBytes.Add(n)
}

func (s *Stats) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totalLocked()
}

// totalLocked — то же самое для вызывающего, который уже держит s.mu.
func (s *Stats) totalLocked() int {
	return s.photos + s.videos + s.audio + s.files + s.gallery
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
		{"Фото из галерей:", s.gallery},
		{"Галерей:", s.galleries},
		{"Пропущено:", s.skipped},
		{"Нет доступа:", s.noAccess},
		{"Отменено:", s.cancelled},
		{"Внешних видео:", s.external},
		{"Ошибок:", s.errors},
		{"Итого:", s.totalLocked()},
	}
	for _, r := range rows {
		if r.val != 0 || r.label == "Итого:" {
			fmt.Printf("  %-14s %d\n", r.label, r.val)
		}
	}
	fmt.Println(sep)
}
