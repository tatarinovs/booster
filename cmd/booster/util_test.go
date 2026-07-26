package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestSafeFilename(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"обычное имя", "Обычный пост", "Обычный пост"},
		{"запрещённые символы", `a<b>c:d"e/f\g|h?i*j`, "a_b_c_d_e_f_g_h_i_j"},
		{"управляющие символы", "a\x00b\x1fc", "a_b_c"},
		{"краевые точки и пробелы", "  .имя.  ", "имя"},
		{"только точки", "...", "unnamed"},
		{"одна точка", ".", "unnamed"},
		{"две точки", "..", "unnamed"},
		{"пустая строка", "", "unnamed"},
		{"только пробелы", "   ", "unnamed"},
		{"зарезервированное имя", "CON", "_CON_"},
		{"зарезервированное с расширением", "nul.txt", "_nul.txt_"},
		{"зарезервированное в другом регистре", "Com1", "_Com1_"},
		{"похоже на зарезервированное", "CONSOLE", "CONSOLE"},

		// Регрессия: внутренние многоточия обязаны сохраняться. Иначе меняется
		// имя папки поста, os.Stat не находит скачанное и пост качается заново.
		{"многоточие внутри", "Ждём... что дальше", "Ждём... что дальше"},
		{"двойная точка внутри", "Часть 1..2", "Часть 1..2"},
		{"многоточие в конце", "Продолжение следует...", "Продолжение следует"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeFilename(tt.in); got != tt.want {
				t.Errorf("safeFilename(%q) = %q, ожидалось %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSafeFilenameTruncatesToRunes(t *testing.T) {
	// Кириллица занимает 2 байта на руну — лимит считается в рунах, не в байтах.
	long := strings.Repeat("я", 300)
	got := safeFilename(long)
	if n := len([]rune(got)); n != 255 {
		t.Errorf("длина результата = %d рун, ожидалось 255", n)
	}
}

func TestSafeFilenameTruncationLeavesNoTrailingDot(t *testing.T) {
	// Windows молча отбрасывает завершающие точки — имя перестало бы совпадать.
	in := strings.Repeat("a", 254) + "..." + "bbb"
	got := safeFilename(in)
	if strings.HasSuffix(got, ".") || strings.HasSuffix(got, " ") {
		t.Errorf("safeFilename(...) = %q, не должно оканчиваться точкой или пробелом", got)
	}
}

func TestSafeFilenameNeverEscapesDirectory(t *testing.T) {
	// Разделители пути вырезаются, поэтому результат всегда один сегмент.
	inputs := []string{
		"../../../etc/passwd",
		`..\..\windows\system32`,
		"....//....//secret",
		"..",
		"../",
	}
	for _, in := range inputs {
		got := safeFilename(in)
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("safeFilename(%q) = %q — содержит разделитель пути", in, got)
		}
		if got == ".." || got == "." {
			t.Errorf("safeFilename(%q) = %q — ссылается на родительский каталог", in, got)
		}
	}
}

func TestIsBoostyHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"boosty.to", true},
		{"api.boosty.to", true},
		{"images.boosty.to", true},
		{"BOOSTY.TO", true},
		{"boosty.to.", true}, // корневая точка FQDN

		// Ключевая проверка: подстрочное сравнение считало эти хосты своими
		// и отправляло туда Authorization с полной cookie сессии.
		{"boosty.to.attacker.com", false},
		{"fake-boosty.to.cdn.ru", false},
		{"notboosty.to", false},
		{"evil.com", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			if got := isBoostyHost(tt.host); got != tt.want {
				t.Errorf("isBoostyHost(%q) = %v, ожидалось %v", tt.host, got, tt.want)
			}
		})
	}
}

func TestSignURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		qs   string
		want map[string]string
	}{
		{
			name: "добавляет параметры подписи",
			raw:  "https://cdn.boosty.to/file.mp3",
			qs:   "?sign=abc&ts=123",
			want: map[string]string{"sign": "abc", "ts": "123"},
		},
		{
			name: "не перезаписывает существующие",
			raw:  "https://cdn.boosty.to/file.mp3?sign=original",
			qs:   "sign=new&ts=123",
			want: map[string]string{"sign": "original", "ts": "123"},
		},
		{
			name: "пустая подпись ничего не ломает",
			raw:  "https://cdn.boosty.to/file.mp3?a=1",
			qs:   "",
			want: map[string]string{"a": "1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := signURL(tt.raw, tt.qs)
			parsed, err := url.Parse(got)
			if err != nil {
				t.Fatalf("результат не разбирается как URL: %v", err)
			}
			q := parsed.Query()
			for k, want := range tt.want {
				if q.Get(k) != want {
					t.Errorf("параметр %q = %q, ожидалось %q (полный URL: %s)", k, q.Get(k), want, got)
				}
			}
		})
	}
}

func TestExtractNickname(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"nickname", "nickname"},
		{"  nickname  ", "nickname"},
		{"https://boosty.to/nickname", "nickname"},
		{"https://boosty.to/nickname/", "nickname"},
		{"https://boosty.to/nickname/posts/123", "nickname"},
		{"boosty.to/nickname?ref=x", "nickname"},
		{"https://boosty.to/nickname#anchor", "nickname"},
		{"", ""},
		{"/", ""},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := extractNickname(tt.in); got != tt.want {
				t.Errorf("extractNickname(%q) = %q, ожидалось %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestAsFloat(t *testing.T) {
	if v, ok := asFloat(float64(42)); !ok || v != 42 {
		t.Errorf("asFloat(42.0) = %v, %v", v, ok)
	}
	for _, bad := range []any{"42", nil, true, []any{1}, map[string]any{}} {
		if _, ok := asFloat(bad); ok {
			t.Errorf("asFloat(%#v) вернул ok=true, ожидалось false", bad)
		}
	}
}

func TestAsMapSlice(t *testing.T) {
	in := []any{
		map[string]any{"a": 1},
		"мусор",
		map[string]any{"b": 2},
		nil,
	}
	got := asMapSlice(in)
	if len(got) != 2 {
		t.Fatalf("получено %d элементов, ожидалось 2", len(got))
	}
	if asMapSlice("не срез") != nil {
		t.Error("asMapSlice на не-срезе должен вернуть nil")
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1024, "1.0KB"},
		{1536, "1.5KB"},
		{1024 * 1024, "1.0MB"},
		{1024 * 1024 * 1024, "1.0GB"},
	}
	for _, tt := range tests {
		if got := formatBytes(tt.in); got != tt.want {
			t.Errorf("formatBytes(%v) = %q, ожидалось %q", tt.in, got, tt.want)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("привет", 3); got != "при" {
		t.Errorf("truncateRunes = %q, ожидалось %q", got, "при")
	}
	if got := truncateRunes("ab", 10); got != "ab" {
		t.Errorf("truncateRunes = %q, ожидалось %q", got, "ab")
	}
}

func TestStripKnownExt(t *testing.T) {
	tests := []struct{ in, ext, want string }{
		{"video.mp4", ".mp4", "video"},
		{"video.MP4", ".mp4", "video"},
		{"video.mkv", ".mp4", "video.mkv"},
		{"video", ".mp4", "video"},
	}
	for _, tt := range tests {
		if got := stripKnownExt(tt.in, tt.ext); got != tt.want {
			t.Errorf("stripKnownExt(%q, %q) = %q, ожидалось %q", tt.in, tt.ext, got, tt.want)
		}
	}
}

func TestFitToWidth(t *testing.T) {
	if got := fitToWidth("abcdef", 3); got != "abc" {
		t.Errorf("fitToWidth = %q, ожидалось %q", got, "abc")
	}
	if got := fitToWidth("abc", 10); got != "abc" {
		t.Errorf("fitToWidth = %q, ожидалось %q", got, "abc")
	}
	if got := fitToWidth("abc", 0); got != "" {
		t.Errorf("fitToWidth(_, 0) = %q, ожидалась пустая строка", got)
	}
	// Широкие символы занимают две колонки — в 3 колонки влезает только один.
	if got := fitToWidth("日本語", 3); got != "日" {
		t.Errorf("fitToWidth(широкие) = %q, ожидалось %q", got, "日")
	}
	// Обрезка не должна разрывать UTF-8 последовательность.
	if got := fitToWidth("привет", 3); got != "при" {
		t.Errorf("fitToWidth(кириллица) = %q, ожидалось %q", got, "при")
	}
}

func TestIsCJKOrWide(t *testing.T) {
	// Основной блок иероглифов (U+4E00–U+9FFF) раньше не учитывался, из-за чего
	// строка считалась вдвое короче и прогресс-бар переносился по строкам.
	wide := []rune{
		'日', '本', '語', // CJK Unified Ideographs
		'中', '文', // китайский
		'한', '글', // Hangul Syllables
		'あ', 'ア', // кана
		'？', // Fullwidth Forms
	}
	for _, r := range wide {
		if !isCJKOrWide(r) {
			t.Errorf("isCJKOrWide(%q / U+%04X) = false, ожидалось true", r, r)
		}
	}

	narrow := []rune{'a', 'Z', '0', ' ', 'я', 'ё', '?', '█', '░', '│', '…'}
	for _, r := range narrow {
		if isCJKOrWide(r) {
			t.Errorf("isCJKOrWide(%q / U+%04X) = true, ожидалось false", r, r)
		}
	}
}

func TestImageExt(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://cdn.boosty.to/image/foo.png", ".png"},
		{"https://cdn.boosty.to/image/foo.PNG", ".png"},
		{"https://cdn.boosty.to/image/foo.gif?sign=abc", ".gif"},
		{"https://cdn.boosty.to/image/foo.webp", ".webp"},
		{"https://cdn.boosty.to/image/foo.jpg", ".jpg"},
		{"https://cdn.boosty.to/image/foo", ".jpg"},            // без расширения
		{"https://cdn.boosty.to/image/foo.exe", ".jpg"},        // не картинка
		{"https://cdn.boosty.to/image/foo.php?a=.png", ".jpg"}, // расширение только из пути
		{"://сломанный", ".jpg"},
	}
	for _, tt := range tests {
		if got := imageExt(tt.in); got != tt.want {
			t.Errorf("imageExt(%q) = %q, ожидалось %q", tt.in, got, tt.want)
		}
	}
}

func TestBestURL(t *testing.T) {
	m := &MediaItem{URLs: map[string]string{"low": "l", "full_hd": "f", "medium": "m"}}
	if got := m.bestURL(); got != "f" {
		t.Errorf("bestURL = %q, ожидалось %q (лучшее доступное качество)", got, "f")
	}
	// Пустые значения игнорируются в пользу следующего качества.
	m2 := &MediaItem{URLs: map[string]string{"ultra_hd": "", "high": "h"}}
	if got := m2.bestURL(); got != "h" {
		t.Errorf("bestURL = %q, ожидалось %q", got, "h")
	}
	if got := (&MediaItem{}).bestURL(); got != "" {
		t.Errorf("bestURL без URL = %q, ожидалась пустая строка", got)
	}
}
