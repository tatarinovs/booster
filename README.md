# Booster — быстрый загрузчик контента с Boosty.to

![Go](https://img.shields.io/badge/Go-%2300ADD8.svg?style=flat&logo=go&logoColor=white)
![Release](https://img.shields.io/github/v/release/tatarinovs/booster?style=flat)
![License](https://img.shields.io/github/license/tatarinovs/booster?style=flat)
![Downloads](https://img.shields.io/github/downloads/tatarinovs/booster/total?style=flat&label=downloads)
![Platforms](https://img.shields.io/badge/platforms-Windows%20%7C%20Linux%20%7C%20macOS-blue?style=flat)

**Booster** (Boosty Downloader) — это высокопроизводительный многопоточный парсер и инструмент для резервного копирования (бэкапа) контента с платформы Boosty.to. Позволяет массово скачивать видео, картинки, аудио, файлы и тексты постов на ваш компьютер.

Проект написан на **Go (Golang)** для максимальной скорости скачивания, экономного расходования оперативной памяти и портативности. Он работает «из коробки» (zero-dependencies) на Windows, Linux и macOS.

## 🚀 Ключевые возможности (Особенности)
- **Умная синхронизация (Бэкап):** Инкрементальное обновление. При повторном запуске скачиваются только новые посты. Проверка занимает секунды.
- **Докачка файлов (Resume):** Если скачивание прервалось или пропал интернет, незавершенные файлы (`.part` + `Range`) будут докачаны с места обрыва.
- **Тексты в Markdown:** Текстовые блоки постов парсятся из внутреннего формата Boosty (Draft.js) и сохраняются в читабельный `.md` со всеми заголовками, ссылками и жирным шрифтом.
- **Многопоточность:** Умный пул из 5 воркеров максимально нагружает ваш интернет-канал, скачивая медиафайлы параллельно.
- **Автоматический выбор качества:** Для видео автоматически выбирается наилучшее доступное разрешение (Ultra HD / 1080p).
- **Надежность:** Автоматические повторные попытки загрузки при ошибках сети и корректное завершение работы (Graceful shutdown) по `Ctrl+C`.
- **Простая авторизация:** Получение токена сессии в один клик через JS-скрипт без долгого поиска куки в инструментах разработчика.

## Сборка

В проекте есть готовые скрипты:
- `build-win.bat` — быстрая сборка `booster.exe`.
- `build-releases.bat` — массовая сборка релизов под все платформы (Windows, Linux, macOS) и архитектуры (amd64, arm64, x86). Все готовые бинарники складываются в отдельную папку `builds/`.

Или вручную:
```bash
go build -trimpath -ldflags="-s -w -buildid=" -o booster.exe ./cmd/booster
```

## Запуск

Имя исполняемого файла зависит от вашей ОС и того, какой релиз вы скачали. В примерах ниже используется `./booster`.

```bash
# Базовый запуск (программа спросит ник в интерактивном режиме)
./booster

# Запуск с указанием автора (ник или ссылка)
./booster -a nickname            # или -a https://boosty.to/nickname

# Указание папки для сохранения
./booster -a nickname -o /path/to/downloads

# Плоский режим (все файлы в одну папку без подпапок по постам)
./booster -a nickname -f
```

> [!NOTE]
> **Для пользователей Linux:**
> После скачивания бинарного файла необходимо сделать его исполняемым:
> ```bash
> chmod +x booster-linux-amd64
> ```

> [!WARNING]
> **Для пользователей macOS:**
> При первом запуске скачанного бинарника macOS заблокирует его (Gatekeeper), так как он не подписан сертификатом Apple.
> Чтобы разрешить запуск, выполните в терминале (подставьте ваше имя файла):
> ```bash
> xattr -rd com.apple.quarantine booster-macos-arm64
> chmod +x booster-macos-arm64
> ```
> Альтернативный способ: Правый клик по файлу в Finder → **Открыть** (Open) → в появившемся предупреждении нажать **Открыть** (Open Anyway).

### Повторный запуск (докачка и обновление)
Программа идеально подходит для регулярной синхронизации (например, через планировщик):
- **Умный пропуск:** Полностью скачанные файлы автоматически пропускаются без лишних сетевых запросов.
- **Докачка (Resume):** Если скачивание прервалось (закрытие программы, обрыв сети), незавершенные файлы (`.part`) будут докачаны с места обрыва.
- **Синхронизация:** При новых запусках скачиваются только новые посты и медиафайлы, опубликованные с момента последней проверки.

## Авторизация

Токен ищется в переменной окружения `BOOSTY_TOKEN`,
затем в файле `.boosty_token` рядом с бинарником, затем в `~/.boosty_token`.
Если токена нет или он просрочен — программа пытается скопировать в буфер
обмена JS-скрипт для получения токена из консоли браузера и просит вставить 
результат.

### Ручное получение токена
Если скрипт автоматически не скопировался в буфер обмена:
1. Зайдите на сайт `boosty.to` под своим аккаунтом.
2. Откройте панель разработчика (обычно `F12` или `Ctrl+Shift+I`) и перейдите на вкладку **Console** (Консоль).
3. Скопируйте и вставьте следующий JS-код, затем нажмите Enter:
   ```javascript
   (function(){function getDecodedCookie(cookieName){const cookies=document.cookie.split(';');r={'%22':'"','%3A':':','%2C':',','%7B':'{','%7D':'}'};for(let c of cookies){const [n,v]=c.trim().split('=');if(n===cookieName&&v){let d=v;Object.entries(r).forEach(([e,dec])=>d=d.replaceAll(e,dec));return d}}return null}if(window.location.hostname==='boosty.to'){const authCookie=getDecodedCookie("auth");if(authCookie){const authObj=JSON.parse(authCookie),token={authorization:authObj.accessToken,expires_in:authObj.expiresAt,full_cookie:document.cookie};console.log("\nJust copy this text:\n\n"+btoa(JSON.stringify(token))+"\n")}else console.warn("Authorization data could not be found. Are you sure you are logged in?")}else console.warn("There is",window.location.hostname,", not boosty.to =)")})();
   ```
4. Скопируйте появившуюся строку (выглядит как длинная base64 последовательность) и вставьте её в программу.

> *Идея и оригинальный JS-скрипт авторизации позаимствованы из отличного проекта [lowfc/boosty_downloader](https://github.com/lowfc/boosty_downloader).*

## Структура

- `main.go` — CLI, сигналы, точка входа
- `auth.go` — токен: decode/encode, load/save, буфер обмена
- `client.go` — HTTP-клиент API boosty.to, пагинация, парсинг постов/медиа
- `models.go` — модели данных (Post, MediaItem)
- `text.go` — рендер текстовых блоков поста из формата Draft.js в удобный Markdown
- `tasks.go` — построение задач загрузки из поста
- `download.go` — воркеры, докачка, retry
- `stats.go` — статистика загрузки
- `progress.go` / `log.go` — однострочный индикатор прогресса и логирование
- `run.go` — основной цикл (аналог `run()` в Python)
- `util.go` — safeFilename, signURL, extractNickname, safeUnlink/Replace
