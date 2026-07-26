package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// style собирает элемент стиля в том виде, в каком его отдаёт API.
func style(id, offset, length float64) any {
	return []any{id, offset, length}
}

func TestApplyStyles(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		styles []any
		want   string
	}{
		{"без стилей", "текст", nil, "текст"},
		{"жирный целиком", "abcdef", []any{style(0, 0, 6)}, "**abcdef**"},
		{"курсив в середине", "abcdef", []any{style(2, 2, 2)}, "ab*cd*ef"},
		{"подчёркивание", "abcdef", []any{style(4, 0, 3)}, "__abc__def"},
		{
			name:   "вложенность",
			text:   "abcdefghij",
			styles: []any{style(0, 0, 10), style(2, 2, 3)},
			want:   "**ab*cde*fghij**",
		},
		{
			name:   "диапазон за границей текста обрезается",
			text:   "abc",
			styles: []any{style(0, 1, 100)},
			want:   "a**bc**",
		},
		{
			name:   "смещение за пределами текста игнорируется",
			text:   "abc",
			styles: []any{style(0, 10, 5)},
			want:   "abc",
		},
		{
			name:   "неизвестный стиль игнорируется",
			text:   "abc",
			styles: []any{style(99, 0, 3)},
			want:   "abc",
		},
		{
			name:   "нулевая длина игнорируется",
			text:   "abc",
			styles: []any{style(0, 1, 0)},
			want:   "abc",
		},
		{
			// Раньше два перекрывающихся диапазона одного стиля давали
			// «**a**b**c**» — по паре тегов на каждый диапазон.
			name:   "перекрытие одного стиля схлопывается",
			text:   "abcdef",
			styles: []any{style(0, 0, 4), style(0, 2, 4)},
			want:   "**abcdef**",
		},
		{
			name:   "кириллица считается в рунах",
			text:   "привет",
			styles: []any{style(0, 0, 3)},
			want:   "**при**вет",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := applyStyles(tt.text, tt.styles); got != tt.want {
				t.Errorf("applyStyles(%q) = %q, ожидалось %q", tt.text, got, tt.want)
			}
		})
	}
}

// TestApplyStylesNeverPanics — главная защита от C1: до правки непроверенные
// приведения типов роняли весь процесс, потому что run выполняется в горутине.
func TestApplyStylesNeverPanics(t *testing.T) {
	bad := []struct {
		name   string
		styles []any
	}{
		{"число строкой", []any{[]any{"0", "0", "3"}}},
		{"null вместо числа", []any{[]any{nil, nil, nil}}},
		{"булево", []any{[]any{true, false, true}}},
		{"мало элементов", []any{[]any{float64(0), float64(1)}}},
		{"много элементов", []any{[]any{float64(0), float64(1), float64(2), float64(3)}}},
		{"не срез", []any{"мусор"}},
		{"вложенный объект", []any{[]any{map[string]any{}, float64(0), float64(1)}}},
		{"отрицательное смещение", []any{style(0, -5, 3)}},
		{"отрицательная длина", []any{style(0, 1, -10)}},
		{"смещение и длина отрицательны", []any{style(0, -10, -10)}},
		{"огромные значения", []any{style(0, 1e18, 1e18)}},
		{"смесь корректного и мусора", []any{[]any{"x", "y", "z"}, style(0, 0, 3)}},
	}

	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("applyStyles паникует на %s: %v", tt.name, r)
				}
			}()
			applyStyles("абвгде", tt.styles)
		})
	}
}

func TestApplyStylesNegativeOffsetKeepsVisibleRange(t *testing.T) {
	// Отрицательное смещение подрезается до нуля, а не выбрасывается целиком.
	if got := applyStyles("abcdef", []any{style(0, -2, 5)}); got != "**abc**def" {
		t.Errorf("applyStyles = %q, ожидалось %q", got, "**abc**def")
	}
}

func TestBlockTextAndStyles(t *testing.T) {
	text, bType, styles := blockTextAndStyles(`["Привет","header-two",[[0,0,6]]]`)
	if text != "Привет" {
		t.Errorf("text = %q", text)
	}
	if bType != "header-two" {
		t.Errorf("blockType = %q", bType)
	}
	if len(styles) != 1 {
		t.Errorf("получено %d стилей, ожидался 1", len(styles))
	}

	// Некорректный вход не должен паниковать и обязан дать нейтральный результат.
	for _, bad := range []string{"", "не json", "{}", "[]", "null"} {
		text, bType, _ := blockTextAndStyles(bad)
		if text != "" || bType != "unstyled" {
			t.Errorf("blockTextAndStyles(%q) = (%q, %q), ожидалось (\"\", \"unstyled\")", bad, text, bType)
		}
	}
}

func TestGetPrefix(t *testing.T) {
	tests := map[string]string{
		"header":              "# ",
		"header-one":          "# ",
		"header-two":          "## ",
		"header-three":        "### ",
		"blockquote":          "> ",
		"unordered-list-item": "* ",
		"ordered-list-item":   "1. ",
		"unstyled":            "",
		"неизвестный":         "",
	}
	for in, want := range tests {
		if got := getPrefix(in); got != want {
			t.Errorf("getPrefix(%q) = %q, ожидалось %q", in, got, want)
		}
	}
}

func TestPostToMarkdown(t *testing.T) {
	blocks := []map[string]any{
		{"type": "text", "content": `["Заголовок","header-two",[]]`},
		{"type": "text", "content": `["Обычный абзац","unstyled",[[0,0,7]]]`},
		{"type": "link", "content": `["ссылка","unstyled",[]]`, "url": "https://example.com"},
		{"type": "list", "items": []any{
			map[string]any{
				"data": []any{map[string]any{"content": `["первый","unstyled",[]]`}},
				"items": []any{
					map[string]any{"data": []any{map[string]any{"content": `["вложенный","unstyled",[]]`}}},
				},
			},
			map[string]any{"data": []any{map[string]any{"content": `["второй","unstyled",[]]`}}},
		}},
	}

	got := postToMarkdown(blocks)

	for _, want := range []string{
		"## Заголовок",
		"**Обычный** абзац",
		"[ссылка](https://example.com)",
		"* первый",
		"  * вложенный",
		"* второй",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("результат не содержит %q.\nПолучено:\n%s", want, got)
		}
	}

	// Три и более переноса подряд схлопываются в два.
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("остались лишние пустые строки:\n%q", got)
	}
}

func TestPostToMarkdownEmpty(t *testing.T) {
	if got := postToMarkdown(nil); got != "" {
		t.Errorf("postToMarkdown(nil) = %q, ожидалась пустая строка", got)
	}
}

func TestIsRetryableStatus(t *testing.T) {
	retryable := []int{500, 502, 503, 504, http.StatusTooManyRequests}
	for _, s := range retryable {
		if !isRetryableStatus(s) {
			t.Errorf("статус %d должен считаться повторяемым", s)
		}
	}
	// 429 — ключевой случай: раньше он обрывал синхронизацию как фатальный.
	if !isRetryableStatus(429) {
		t.Error("429 обязан быть повторяемым")
	}
	for _, s := range []int{400, 401, 403, 404, 410} {
		if isRetryableStatus(s) {
			t.Errorf("статус %d не должен повторяться", s)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d, ok := parseRetryAfter("10"); !ok || d != 10*time.Second {
		t.Errorf("parseRetryAfter(\"10\") = %v, %v", d, ok)
	}
	// Значение подрезается до потолка.
	if d, ok := parseRetryAfter("99999"); !ok || d != retryAfterCap {
		t.Errorf("parseRetryAfter(\"99999\") = %v, ожидался потолок %v", d, retryAfterCap)
	}
	// HTTP-дата в прошлом не даёт задержки.
	if _, ok := parseRetryAfter("Mon, 02 Jan 2006 15:04:05 GMT"); ok {
		t.Error("дата в прошлом не должна давать задержку")
	}
	for _, bad := range []string{"", "   ", "не число", "-5"} {
		if _, ok := parseRetryAfter(bad); ok {
			t.Errorf("parseRetryAfter(%q) вернул ok=true", bad)
		}
	}
}

func TestBackoffDelayGrowsAndStaysBounded(t *testing.T) {
	for attempt := 1; attempt <= apiMaxRetries; attempt++ {
		base := apiRetryBaseDelay << uint(attempt-1)
		d := backoffDelay(attempt)
		// Джиттер добавляет не больше половины базовой задержки.
		if d < base || d >= base+base/2 {
			t.Errorf("backoffDelay(%d) = %v, ожидалось в [%v, %v)", attempt, d, base, base+base/2)
		}
	}
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		name        string
		failed      bool
		interrupted bool
		errors      int
		want        int
	}{
		{"успех", false, false, 0, 0},
		{"критическая ошибка", true, false, 0, 1},
		{"ошибки загрузки", false, false, 3, 2},
		{"прервано пользователем", false, true, 0, 130},
		{"критическая важнее ошибок", true, false, 5, 1},
		{"ошибки важнее прерывания", false, true, 2, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStats()
			for i := 0; i < tt.errors; i++ {
				s.record("photo", false, true)
			}
			if got := exitCode(tt.failed, tt.interrupted, s); got != tt.want {
				t.Errorf("exitCode = %d, ожидалось %d", got, tt.want)
			}
		})
	}
}

func TestStatsCounters(t *testing.T) {
	s := newStats()
	s.record("photo", false, false)
	s.record("video", false, false)
	s.record("audio", false, false)
	s.record("file", false, false)
	s.record("photo", true, false) // пропущено
	s.record("photo", false, true) // ошибка
	s.incNoAccess()
	s.incCancelled()

	if got := s.total(); got != 4 {
		t.Errorf("total = %d, ожидалось 4", got)
	}
	if got := s.errorCount(); got != 1 {
		t.Errorf("errorCount = %d, ожидалось 1", got)
	}
	if s.skipped != 1 || s.noAccess != 1 || s.cancelled != 1 {
		t.Errorf("skipped=%d noAccess=%d cancelled=%d, ожидалось по 1",
			s.skipped, s.noAccess, s.cancelled)
	}
}

func TestStatsAddBytesSupportsRollback(t *testing.T) {
	// Отброшенный .part вычитается, иначе итоговый объём завышался при ретраях.
	s := newStats()
	s.addBytes(1000)
	s.addBytes(-1000)
	s.addBytes(500)
	if got, _ := s.snapshotBytes(); got != 500 {
		t.Errorf("processedBytes = %d, ожидалось 500", got)
	}
}
