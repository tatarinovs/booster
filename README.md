# Booster CLI

Многопоточный консольный загрузчик контента с Boosty.to.
Проект написан на **Go** (Golang) для максимальной производительности, низкого потребления памяти и работы без сторонних зависимостей.

## Особенности
- Авторизация через токен из консоли браузера.
- Постраничный обход постов с инкрементальной синхронизацией (быстрая докачка новых постов за 1 секунду).
- Пул из 5 воркеров с умным пулом буферов (экономное расходование RAM).
- Докачка оборванных файлов (`.part` + `Range`), 5 повторных попыток на файл.
- Graceful shutdown по `Ctrl+C` (сохраняет состояние для продолжения при следующем запуске).

## Сборка

В проекте есть готовые скрипты:
- `build-win.bat` — собирает `booster.exe` (встраивает иконку и сжимает через UPX).
- `build-linux.bat` — кросс-компиляция `booster_linux` под Linux (amd64).

Или вручную:
```bash
go build -trimpath -ldflags="-s -w" -o booster.exe .
```

## Запуск

```bash
./boosty -a nickname            # или -a https://boosty.to/nickname
./boosty -a nickname -o /path/to/downloads
./boosty -a nickname -f         # плоский режим — все файлы в одну папку
```

Если ник не передан флагом — программа спросит его в интерактивном режиме.

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

## Структура

- `main.go` — CLI, сигналы, точка входа
- `auth.go` — токен: decode/encode, load/save, буфер обмена
- `client.go` — HTTP-клиент API boosty.to, пагинация, парсинг постов/медиа
- `models.go` — модели данных (Post, MediaItem)
- `text.go` — рендер текстовых блоков поста в plain text
- `tasks.go` — построение задач загрузки из поста
- `download.go` — воркеры, докачка, retry
- `stats.go` — статистика загрузки
- `progress.go` / `log.go` — однострочный индикатор прогресса и логирование
- `run.go` — основной цикл (аналог `run()` в Python)
- `util.go` — safeFilename, signURL, extractNickname, safeUnlink/Replace
