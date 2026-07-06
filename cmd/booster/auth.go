package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	tokenEnv      = "BOOSTY_TOKEN"
	tokenFilename = ".boosty_token"
)

// AuthToken хранит данные авторизации на boosty.to.
type AuthToken struct {
	Authorization string
	Cookie        string
	ExpiresAt     int64 // unix timestamp в секундах
}

func (t *AuthToken) IsExpired() bool {
	return time.Now().Unix() >= t.ExpiresAt
}

type rawToken struct {
	Authorization string      `json:"authorization"`
	FullCookie    string      `json:"full_cookie"`
	ExpiresIn     json.Number `json:"expires_in"`
}

// decodeToken декодирует base64+JSON токен, скопированный из консоли браузера.
func decodeToken(raw string) *AuthToken {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		logError("Не удалось декодировать токен: %v", err)
		return nil
	}
	var rt rawToken
	if err := json.Unmarshal(decoded, &rt); err != nil {
		logError("Не удалось декодировать токен: %v", err)
		return nil
	}
	expF, err := rt.ExpiresIn.Float64()
	if err != nil {
		logError("Не удалось декодировать токен: %v", err)
		return nil
	}
	if expF > 1e12 {
		expF = expF / 1000
	}
	return &AuthToken{
		Authorization: rt.Authorization,
		Cookie:        rt.FullCookie,
		ExpiresAt:     int64(expF),
	}
}

func (t *AuthToken) encode() string {
	data, _ := json.Marshal(map[string]any{
		"authorization": t.Authorization,
		"full_cookie":   t.Cookie,
		"expires_in":    t.ExpiresAt * 1000,
	})
	return base64.StdEncoding.EncodeToString(data)
}

// loadToken пытается прочитать токен из переменной окружения или файлов.
func loadToken(scriptDir string) *AuthToken {
	if raw := os.Getenv(tokenEnv); raw != "" {
		return decodeToken(raw)
	}
	home, _ := os.UserHomeDir()
	candidates := []string{filepath.Join(scriptDir, tokenFilename)}
	if home != "" {
		candidates = append(candidates, filepath.Join(home, tokenFilename))
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		return decodeToken(string(data))
	}
	return nil
}

// saveToken сохраняет токен в файл рядом со скриптом.
func saveToken(scriptDir string, token *AuthToken) {
	path := filepath.Join(scriptDir, tokenFilename)
	if err := os.WriteFile(path, []byte(token.encode()), 0o600); err != nil {
		logError("Не удалось сохранить токен: %v", err)
		return
	}
	logInfo("Токен сохранён: %s", path)
}

// authJS — скрипт для получения токена из браузера (boosty.to → F12 → Console).
const authJS = `(function(){function getDecodedCookie(cookieName){const cookies=document.cookie.split(';')` +
	`,r={'%22':'"','%3A':':','%2C':',','%7B':'{','%7D':'}'};for(let c of cookies){` +
	`const [n,v]=c.trim().split('=');if(n===cookieName&&v){let d=v;` +
	`Object.entries(r).forEach(([e,dec])=>d=d.replaceAll(e,dec));return d}}return null}` +
	`if(window.location.hostname==='boosty.to'){const authCookie=getDecodedCookie("auth");` +
	`if(authCookie){const authObj=JSON.parse(authCookie),token={authorization:authObj.accessToken,` +
	`expires_in:authObj.expiresAt,full_cookie:document.cookie};` +
	`console.log("\nJust copy this text:\n\n"+btoa(JSON.stringify(token))+"\n")}` +
	`else console.warn("Authorization data could not be found. Are you sure you are logged in?")}` +
	`else console.warn("There is",window.location.hostname,", not boosty.to =)")})();`

// copyToClipboard копирует текст в системный буфер обмена. Возвращает true при успехе.
func copyToClipboard(text string) bool {
	var cmds [][]string
	switch runtime.GOOS {
	case "windows":
		cmds = [][]string{{"clip"}}
	case "darwin":
		cmds = [][]string{{"pbcopy"}}
	default:
		cmds = [][]string{
			{"xclip", "-selection", "clipboard"},
			{"xsel", "--clipboard", "--input"},
		}
	}
	for _, c := range cmds {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err == nil {
			return true
		}
	}
	return false
}

// promptToken предлагает пользователю вставить токен, полученный через консоль браузера.
func promptToken(scriptDir string) *AuthToken {
	copied := copyToClipboard(authJS)
	if copied {
		fmt.Println("\nДля получения токена:\n" +
			"  1. Откройте boosty.to в браузере и войдите в аккаунт.\n" +
			"  2. Нажмите F12 → вкладка Console.\n" +
			"  3. Скрипт уже скопирован в буфер обмена — вставьте его (Ctrl+V) и нажмите Enter.\n" +
			"     Если браузер просит разрешение — введите 'allow pasting' и повторите.\n" +
			"  4. Скопируйте строку base64 из консоли и вставьте ниже.")
	} else {
		fmt.Println("\nДля получения токена: войдите на boosty.to, откройте F12 → Console,\n" +
			"выполните скрипт авторизации (см. README), вставьте результат сюда.")
	}
	fmt.Print("Токен (Enter — пропустить): ")
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		logWarn("Stdin недоступен, пропускаем ввод токена.")
		return nil
	}
	raw := strings.TrimSpace(line)
	if raw == "" {
		return nil
	}
	token := decodeToken(raw)
	if token != nil {
		saveToken(scriptDir, token)
	}
	return token
}
