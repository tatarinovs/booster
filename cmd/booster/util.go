package main

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

var illegalCharsRe = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

var reservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// safeFilename очищает строку для использования как имя файла/папки на Windows.
func safeFilename(name string) string {
	clean := illegalCharsRe.ReplaceAllString(name, "_")
	clean = strings.Trim(clean, " .")

	stem := clean
	if ext := filepath.Ext(clean); ext != "" {
		stem = strings.TrimSuffix(clean, ext)
	}
	if reservedNames[strings.ToUpper(stem)] {
		clean = "_" + clean + "_"
	}
	if len(clean) > 255 {
		clean = clean[:255]
	}
	if clean == "" {
		clean = "unnamed"
	}
	return clean
}

// signURL добавляет параметры подписи в URL, не перезаписывая существующие.
func signURL(rawURL string, qs string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	values := parsed.Query()

	qs = strings.TrimPrefix(qs, "?")
	extra, err := url.ParseQuery(qs)
	if err == nil {
		for k, vs := range extra {
			if _, exists := values[k]; !exists && len(vs) > 0 {
				values.Set(k, vs[0])
			}
		}
	}
	parsed.RawQuery = values.Encode()
	return parsed.String()
}

var boostyNicknameRe = regexp.MustCompile(`boosty\.to/([^/?#]+)`)

// extractNickname извлекает ник автора из ссылки или возвращает строку как есть.
func extractNickname(s string) string {
	s = strings.TrimSpace(s)
	if m := boostyNicknameRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return strings.TrimSpace(strings.ReplaceAll(s, "/", ""))
}

// safeUnlink удаляет файл с повторными попытками (Windows держит файлы открытыми).
func safeUnlink(path string) error {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		err := os.Remove(path)
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		lastErr = err
		if runtime.GOOS != "windows" {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return lastErr
}

// safeReplace атомарно переименовывает файл с повторными попытками.
func safeReplace(src, dst string) error {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		err := os.Rename(src, dst)
		if err == nil {
			return nil
		}
		lastErr = err
		if runtime.GOOS != "windows" {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return lastErr
}
