#!/usr/bin/env python3
"""
Загрузчик медиа с boosty.to.
Качает фото, видео, аудио и файлы по нику автора.
Поддерживает авторизацию, докачку, повторные попытки, graceful shutdown.
"""

from __future__ import annotations

import argparse
import asyncio
import base64
import json
import logging
import os
import re
import sys
import threading
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import AsyncIterator, Optional
from urllib.parse import parse_qsl, urlencode, urlparse

import aiofiles
import aiohttp
from tqdm.asyncio import tqdm_asyncio

# ---------------------------------------------------------------------------
# Константы
# ---------------------------------------------------------------------------

SCRIPT_DIR       = Path(__file__).resolve().parent
TOKEN_ENV        = "BOOSTY_TOKEN"
TOKEN_FILENAME   = ".boosty_token"
FAILED_FILENAME  = "failed.txt"
CONTENT_FILENAME = "content.txt"
API_BASE         = "https://api.boosty.to"

POSTS_PAGE_LIMIT     = 20
WORKERS              = 5     # параллельных загрузок
MAX_DOWNLOAD_RETRIES = 5
DOWNLOAD_RETRY_DELAY = 3     # секунд между попытками
API_MAX_RETRIES      = 5
API_RETRY_BASE_DELAY = 2

# Без общего таймаута, но с таймаутом на коннект и чтение
CDN_TIMEOUT = aiohttp.ClientTimeout(total=None, sock_read=120, sock_connect=30)
# API-запросы короткие, должны отвечать быстро
API_TIMEOUT = aiohttp.ClientTimeout(total=30)

VIDEO_QUALITIES = ("ultra_hd", "full_hd", "high", "medium", "low")

# ---------------------------------------------------------------------------
# Логирование — через tqdm.write чтобы не ломать прогресс-бары
# ---------------------------------------------------------------------------

class _TqdmHandler(logging.Handler):
    def emit(self, record: logging.LogRecord) -> None:
        try:
            tqdm_asyncio.write(self.format(record))
        except Exception:
            self.handleError(record)

logging.basicConfig(
    level=logging.INFO,
    handlers=[_TqdmHandler()],
    format="%(asctime)s [%(levelname)s] %(message)s",
    datefmt="%H:%M:%S",
)
log = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Auth
# ---------------------------------------------------------------------------

@dataclass
class AuthToken:
    authorization: str
    cookie: str
    expires_at: int  # unix timestamp в секундах

    def is_expired(self) -> bool:
        return time.time() >= self.expires_at

    @staticmethod
    def decode(raw: str) -> Optional[AuthToken]:
        raw = raw.strip()
        if not raw:
            return None
        try:
            data = json.loads(base64.b64decode(raw))
            exp = data["expires_in"]
            if exp > 1e12:
                exp = int(exp / 1000)
            return AuthToken(
                authorization=data["authorization"],
                cookie=data["full_cookie"],
                expires_at=int(exp),
            )
        except Exception as e:
            log.error("Не удалось декодировать токен: %s", e)
            return None

    def encode(self) -> str:
        return base64.b64encode(json.dumps({
            "authorization": self.authorization,
            "full_cookie":   self.cookie,
            "expires_in":    self.expires_at * 1000,
        }).encode()).decode()


def load_token() -> Optional[AuthToken]:
    if raw := os.environ.get(TOKEN_ENV):
        return AuthToken.decode(raw)
    for p in (SCRIPT_DIR / TOKEN_FILENAME, Path.home() / TOKEN_FILENAME):
        if p.is_file():
            try:
                return AuthToken.decode(p.read_text(encoding="utf-8"))
            except Exception as e:
                log.warning("Не удалось прочитать токен из %s: %s", p, e)
    return None


def save_token(token: AuthToken) -> None:
    path = SCRIPT_DIR / TOKEN_FILENAME
    path.write_text(token.encode(), encoding="utf-8")
    log.info("Токен сохранён: %s", path)


def prompt_token() -> Optional[AuthToken]:
    print(
        "\nДля получения токена: войдите на boosty.to, откройте F12 → Console,\n"
        "выполните скрипт авторизации (см. README), вставьте результат сюда."
    )
    raw = input("Токен (Enter — пропустить): ").strip()
    if not raw:
        return None
    token = AuthToken.decode(raw)
    if token:
        save_token(token)
    return token

# ---------------------------------------------------------------------------
# Утилиты
# ---------------------------------------------------------------------------

def safe_filename(name: str) -> str:
    """Очищает строку для использования как имя файла/папки на Windows."""
    ILLEGAL  = r'[<>:"/\\|?*\x00-\x1f]'
    RESERVED = {
        "CON","PRN","AUX","NUL",
        "COM1","COM2","COM3","COM4","COM5","COM6","COM7","COM8","COM9",
        "LPT1","LPT2","LPT3","LPT4","LPT5","LPT6","LPT7","LPT8","LPT9",
    }
    clean = re.sub(ILLEGAL, "_", name).strip(" .")
    if Path(clean).stem.upper() in RESERVED:
        clean = f"_{clean}_"
    return clean[:255] or "unnamed"


def sign_url(url: str, qs: str) -> str:
    """Добавляет параметры подписи в URL, не перезаписывая существующие."""
    parsed = urlparse(url)
    params = dict(parse_qsl(parsed.query))
    for k, v in parse_qsl(qs.lstrip("?")):
        params.setdefault(k, v)
    return parsed._replace(query=urlencode(params)).geturl()


def extract_nickname(s: str) -> str:
    s = s.strip()
    if m := re.search(r"boosty\.to/([^/?#]+)", s):
        return m.group(1)
    return s.replace("/", "").strip()


async def _safe_unlink(path: Path) -> None:
    """Удаляет файл с повторными попытками (Windows держит файлы открытыми)."""
    for attempt in range(5):
        try:
            path.unlink(missing_ok=True)
            return
        except PermissionError:
            if attempt == 4:
                raise
            await asyncio.sleep(0.5)


async def _safe_replace(src: Path, dst: Path) -> None:
    """Атомарно переименовывает файл с повторными попытками."""
    for attempt in range(5):
        try:
            os.replace(src, dst)
            return
        except PermissionError:
            if attempt == 4:
                raise
            await asyncio.sleep(0.5)

# ---------------------------------------------------------------------------
# Модели данных API
# ---------------------------------------------------------------------------

@dataclass
class Image:
    id: str
    url: str

@dataclass
class Video:
    id: str
    title: str
    urls: dict[str, str]  # quality → url

    def best_url(self) -> Optional[str]:
        for q in VIDEO_QUALITIES:
            if url := self.urls.get(q):
                return url
        return None

@dataclass
class Audio:
    id: str
    url: str
    title: str

@dataclass
class File:
    id: str
    url: str
    title: str

MediaItem = Image | Video | Audio | File

@dataclass
class Post:
    id: str
    int_id: int
    has_access: bool
    publish_time: int
    title: Optional[str]
    signed_query: str
    text_blocks: list[dict] = field(default_factory=list)
    media: list[MediaItem]  = field(default_factory=list)

# ---------------------------------------------------------------------------
# API-клиент
# ---------------------------------------------------------------------------

class BoostyClient:
    def __init__(self, token: Optional[AuthToken]) -> None:
        self._token   = token
        self._session: Optional[aiohttp.ClientSession] = None
        self._headers = self._build_headers()

    def _build_headers(self) -> dict:
        h = {
            "User-Agent":         "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36",
            "Sec-Ch-Ua":          '"Google Chrome";v="123", "Not:A-Brand";v="8", "Chromium";v="123"',
            "Sec-Ch-Ua-Mobile":   "?0",
            "Sec-Ch-Ua-Platform": '"Windows"',
        }
        if self._token:
            h["Authorization"] = f"Bearer {self._token.authorization}"
            h["Cookie"]        = self._token.cookie
        return h

    async def __aenter__(self) -> BoostyClient:
        self._session = aiohttp.ClientSession(headers=self._headers, timeout=API_TIMEOUT)
        return self

    async def __aexit__(self, *_) -> None:
        if self._session:
            await self._session.close()

    async def _get(self, url: str, **params) -> dict:
        assert self._session, "Используй 'async with BoostyClient'"
        for attempt in range(1, API_MAX_RETRIES + 1):
            try:
                async with self._session.get(url, params=params or None) as r:
                    r.raise_for_status()
                    return await r.json()
            except (aiohttp.ClientResponseError, aiohttp.ClientError, asyncio.TimeoutError) as e:
                status = getattr(e, "status", None)
                if status and 400 <= status < 500:
                    log.error("API %s → %s", url, e)
                    raise
                if attempt == API_MAX_RETRIES:
                    log.error("API %s — %d попыток исчерпано: %s", url, attempt, e)
                    raise
                delay = API_RETRY_BASE_DELAY * 2 ** (attempt - 1)
                log.warning("API %s — ошибка, повтор через %ds (%d/%d): %s", url, delay, attempt, API_MAX_RETRIES, e)
                await asyncio.sleep(delay)
        raise RuntimeError("unreachable")

    async def blog_post_count(self, author: str) -> int:
        data = await self._get(f"{API_BASE}/v1/blog/{author}")
        return data.get("count", {}).get("posts", 0)

    async def iter_posts(self, author: str) -> AsyncIterator[Post]:
        """Асинхронный генератор постов постранично."""
        offset: Optional[str] = None
        while True:
            data = await self._get(
                f"{API_BASE}/v1/blog/{author}/post/",
                limit=POSTS_PAGE_LIMIT,
                reply_limit=0,
                comments_limit=0,
                **({"offset": offset} if offset else {}),
            )
            for raw in data.get("data", []):
                yield self._parse_post(raw)
            extra = data.get("extra", {})
            if extra.get("isLast") or not (offset := extra.get("offset")):
                break

    # --- Парсинг ---

    def _parse_post(self, raw: dict) -> Post:
        post = Post(
            id           = raw["id"],
            int_id       = raw["intId"],
            has_access   = raw.get("hasAccess", False),
            publish_time = raw["publishTime"],
            title        = raw.get("title") or None,
            signed_query = raw.get("signedQuery", ""),
        )
        seen: set[str] = set()
        for block in (*raw.get("data", []), *raw.get("media", [])):
            bid = block.get("id")
            if bid and bid in seen:
                continue
            t = block.get("type")
            if t in ("text", "header", "link", "list"):
                post.text_blocks.append(block)
            else:
                if m := self._parse_media(block):
                    post.media.append(m)
                    if bid:
                        seen.add(bid)
        return post

    def _parse_media(self, b: dict) -> Optional[MediaItem]:
        if b.get("isTeaser") or b.get("is_teaser"):
            return None
        t = b.get("type")
        if t == "image":
            return Image(id=b["id"], url=b["url"]) if "width" in b else None
        if t in ("ok_video", "video"):
            urls = {
                u["type"]: u["url"]
                for u in (b.get("playerUrls") or b.get("player_urls") or [])
                if u.get("type") in VIDEO_QUALITIES and u.get("url")
            }
            return Video(id=b["id"], title=b.get("title") or b["id"], urls=urls)
        if t == "audio_file":
            return Audio(id=b["id"], url=b["url"], title=b.get("title") or b["id"])
        if t == "file":
            return File(id=b["id"], url=b["url"], title=b.get("title") or b["id"])
        # external_video (YouTube/Vimeo) и неизвестные типы — пропускаем
        return None

# ---------------------------------------------------------------------------
# Текст поста → plain text
# ---------------------------------------------------------------------------

def _block_text(content_json: str) -> str:
    try:
        data = json.loads(content_json)
        return data[0] if data else ""
    except (json.JSONDecodeError, IndexError, TypeError):
        return ""


def post_to_text(blocks: list[dict]) -> str:
    parts: list[str] = []

    def render_list(items: list, indent: int = 0) -> None:
        for item in items:
            text = "".join(_block_text(d.get("content", "")) for d in item.get("data", []))
            parts.append("  " * indent + "- " + text + "\n")
            if sub := item.get("items"):
                render_list(sub, indent + 1)

    for block in blocks:
        t = block.get("type")
        if t == "link":
            parts.append(f"{_block_text(block.get('content', ''))} (ссылка: {block.get('url', '')})\n")
        elif t in ("text", "header"):
            content = _block_text(block.get("content", ""))
            if content:
                parts.append(content)
            if block.get("modificator") == "BLOCK_END" or content:
                parts.append("\n")
        elif t == "list":
            render_list(block.get("items", []))

    return "".join(parts).strip()

# ---------------------------------------------------------------------------
# Задача загрузки
# ---------------------------------------------------------------------------

@dataclass
class DownloadTask:
    url: str
    dest: Path
    media_type: str         # "photo" | "video" | "audio" | "file"
    referer: Optional[str]
    is_video: bool = False  # нужна ли проверка размера после скачивания

# ---------------------------------------------------------------------------
# Статистика
# ---------------------------------------------------------------------------

@dataclass
class Stats:
    photos:    int = 0
    videos:    int = 0
    audio:     int = 0
    files:     int = 0
    skipped:   int = 0
    errors:    int = 0
    no_access: int = 0
    processed_bytes:   int   = 0
    last_pbar_update:  float = 0.0
    _lock: asyncio.Lock = field(default_factory=asyncio.Lock, repr=False, compare=False)

    async def record(self, media_type: str, *, skipped: bool = False, error: bool = False) -> None:
        async with self._lock:
            if error:
                self.errors += 1
            elif skipped:
                self.skipped += 1
            elif media_type == "photo":
                self.photos += 1
            elif media_type == "video":
                self.videos += 1
            elif media_type == "audio":
                self.audio += 1
            else:
                self.files += 1

    def total(self) -> int:
        return self.photos + self.videos + self.audio + self.files

    def print_summary(self) -> None:
        sep = "=" * 50
        print(f"\n{sep}\nСтатистика загрузки\n{sep}")
        rows = [
            ("Фото:",        self.photos),
            ("Видео:",       self.videos),
            ("Аудио:",       self.audio),
            ("Файлы:",       self.files),
            ("Пропущено:",   self.skipped),
            ("Нет доступа:", self.no_access),
            ("Ошибок:",      self.errors),
            ("Итого:",       self.total()),
        ]
        for label, val in rows:
            if val or label == "Итого:":
                print(f"  {label:<14} {val}")
        print(sep)

# ---------------------------------------------------------------------------
# Воркер загрузки
# ---------------------------------------------------------------------------

async def _worker(
    wid: int,
    queue: asyncio.Queue[Optional[DownloadTask]],
    cdn_session: aiohttp.ClientSession,
    api_session: aiohttp.ClientSession,
    stats: Stats,
    cancel: asyncio.Event,
    abort: asyncio.Event,
    failed_path: Path,
    post_pbar: tqdm_asyncio,
) -> None:
    """
    Берёт задачи из очереди и скачивает их последовательно.
    None — sentinel, сигнал завершения.
    После cancel не берёт новых задач, но дожидается текущей.
    """
    pbar_pos = wid + 1  # позиция 0 занята баром постов

    while True:
        task = await queue.get()
        try:
            if task is None:  # sentinel
                return
            if cancel.is_set():
                continue  # задача отброшена, но task_done будет вызван в finally
            await _download(task, cdn_session, api_session, stats, cancel, abort, failed_path, pbar_pos, post_pbar)
        except Exception as e:
            log.error("Воркер %d — неожиданная ошибка для %s: %s", wid, getattr(task, "dest", "?"), e)
            if task:
                await stats.record(task.media_type, error=True)
        finally:
            queue.task_done()


async def _download(
    task: DownloadTask,
    cdn_session: aiohttp.ClientSession,
    api_session: aiohttp.ClientSession,
    stats: Stats,
    cancel: asyncio.Event,
    abort: asyncio.Event,
    failed_path: Path,
    pbar_pos: int,
    post_pbar: tqdm_asyncio,
) -> None:
    """Скачивает один файл с поддержкой докачки и retry."""
    if task.dest.exists():
        await stats.record(task.media_type, skipped=True)
        return

    # Папка может не существовать если воркер обгоняет создание директории
    task.dest.parent.mkdir(parents=True, exist_ok=True)

    part    = task.dest.with_suffix(task.dest.suffix + ".part")
    initial = part.stat().st_size if part.exists() else 0

    # CDN-файлы качаем без auth-заголовков, API-файлы — с ними
    is_cdn  = "boosty.to" not in (urlparse(task.url).hostname or "")
    session = cdn_session if is_cdn else api_session

    desc = f"  {task.dest.name[:25]:<25}"
    with tqdm_asyncio(total=None, unit="B", unit_scale=True, desc=desc,
                      position=pbar_pos, leave=False, dynamic_ncols=True) as pbar:

        deleted_part = False

        for attempt in range(1, MAX_DOWNLOAD_RETRIES + 1):
            if abort.is_set():
                return  # .part остаётся — при следующем запуске докачается

            hdrs: dict[str, str] = {}
            if initial > 0:
                hdrs["Range"] = f"bytes={initial}-"
            if task.referer:
                hdrs["Referer"] = task.referer

            try:
                async with session.get(task.url, headers=hdrs, timeout=CDN_TIMEOUT) as resp:
                    resp.raise_for_status()

                    if resp.status == 206:  # сервер поддержал Range
                        expected = initial + (resp.content_length or 0)
                        mode = "ab"
                    else:
                        expected = resp.content_length if task.is_video else None
                        initial  = 0
                        mode     = "wb"

                    pbar.reset(total=expected)
                    pbar.update(initial)

                    async with aiofiles.open(part, mode) as f:
                        async for chunk in resp.content.iter_chunked(256 * 1024):
                            if abort.is_set():
                                return
                            await f.write(chunk)
                            n = len(chunk)
                            initial += n
                            pbar.update(n)

                            # Обновляем суммарный постфикс не чаще раза в 0.5с
                            async with stats._lock:
                                stats.processed_bytes += n
                                now = time.time()
                                if now - stats.last_pbar_update > 0.5:
                                    total_bytes = stats.processed_bytes
                                    total_fmt   = tqdm_asyncio.format_sizeof(total_bytes)
                                    elapsed     = post_pbar.format_dict.get('elapsed', 0) or 1
                                    speed_fmt   = tqdm_asyncio.format_sizeof(total_bytes / elapsed)
                                    post_pbar.set_postfix_str(f"Total: {total_fmt} | Speed: {speed_fmt}/s")
                                    stats.last_pbar_update = now

                # Проверяем что видео скачалось полностью
                if task.is_video and expected and initial < expected:
                    raise RuntimeError(f"Неполный файл: ожидалось {expected} байт, получено {initial}")

                await _safe_replace(part, task.dest)
                await stats.record(task.media_type)
                return  # успех

            except aiohttp.ClientResponseError as e:
                if e.status in (400, 403):
                    if initial > 0 and not deleted_part:
                        log.info("CDN отклонил Range для %s, качаем заново", task.dest.name)
                        await _safe_unlink(part)
                        initial      = 0
                        deleted_part = True
                        continue  # сразу повторяем без sleep
                    log.error("HTTP %d (протухший URL?): %s", e.status, task.url)
                    break
                if e.status == 404:
                    log.error("Файл не найден (404): %s", task.url)
                    break
                _warn_retry(attempt, task.dest.name, e)

            except (aiohttp.ClientError, asyncio.TimeoutError, RuntimeError) as e:
                _warn_retry(attempt, task.dest.name, e)

            if attempt < MAX_DOWNLOAD_RETRIES:
                await asyncio.sleep(DOWNLOAD_RETRY_DELAY)

        # Все попытки исчерпаны
        log.error("Не удалось скачать после %d попыток: %s", MAX_DOWNLOAD_RETRIES, task.url)
        async with aiofiles.open(failed_path, "a", encoding="utf-8") as f:
            await f.write(f"{task.dest}\t{task.url}\n")
        await stats.record(task.media_type, error=True)


def _warn_retry(attempt: int, name: str, e: Exception) -> None:
    if attempt < MAX_DOWNLOAD_RETRIES:
        log.warning("Попытка %d/%d для %s: %s", attempt, MAX_DOWNLOAD_RETRIES, name, e)

# ---------------------------------------------------------------------------
# Построение задач из поста
# ---------------------------------------------------------------------------

def _post_dir(base: Path, post: Post) -> Path:
    date  = datetime.fromtimestamp(post.publish_time, tz=timezone.utc).strftime("%Y-%m-%d")
    title = safe_filename(post.title or "post")[:100]
    return base / f"{date}_{title}_{post.id}"


def _flat_prefix(post: Post, index: int) -> str:
    date  = datetime.fromtimestamp(post.publish_time, tz=timezone.utc).strftime("%Y-%m-%d")
    title = safe_filename(post.title or "post")[:100]
    return f"{date}_{title}_{index:03d}"


def make_tasks(post: Post, dest_dir: Path, is_flat: bool) -> list[DownloadTask]:
    tasks: list[DownloadTask] = []
    post_url = f"https://boosty.to/posts/{post.id}"

    for i, m in enumerate(post.media, start=1):
        pfx = _flat_prefix(post, i) if is_flat else f"{i:03d}"

        if isinstance(m, Image):
            fname = f"{pfx}.jpg" if is_flat else f"{pfx}_{m.id}.jpg"
            tasks.append(DownloadTask(
                url=m.url, dest=dest_dir / fname,
                media_type="photo", referer=None,
            ))

        elif isinstance(m, Video):
            url = m.best_url()
            if not url:
                log.warning("Нет доступных URL для видео %s в посте %s", m.id, post.id)
                continue
            clean_title = safe_filename(m.title or m.id)
            if clean_title.lower().endswith(".mp4"):
                clean_title = clean_title[:-4]
            title = clean_title[:100]
            fname = f"{pfx}.mp4" if is_flat else f"{pfx}_{title}.mp4"
            tasks.append(DownloadTask(
                url=url, dest=dest_dir / fname,
                media_type="video", referer=post_url, is_video=True,
            ))

        elif isinstance(m, (Audio, File)):
            if not post.signed_query:
                log.warning("Нет signed_query для %s в посте %s, пропускаем", type(m).__name__, post.id)
                continue
            url   = sign_url(m.url, post.signed_query)
            
            clean_title = safe_filename(m.title or m.id)
            
            if isinstance(m, Audio):
                title = clean_title[:-4][:100] if clean_title.lower().endswith(".mp3") else clean_title[:100]
                fname = f"{pfx}.mp3" if is_flat else f"{pfx}_{title}.mp3"
                tasks.append(DownloadTask(url=url, dest=dest_dir / fname, media_type="audio", referer=None))
            else:
                ext   = Path(clean_title).suffix
                title = Path(clean_title).stem[:100]
                fname = f"{pfx}{ext}" if is_flat else f"{pfx}_{title}{ext}"
                tasks.append(DownloadTask(url=url, dest=dest_dir / fname, media_type="file", referer=None))

    return tasks

# ---------------------------------------------------------------------------
# Основной цикл
# ---------------------------------------------------------------------------

async def run(
    author: str,
    token: Optional[AuthToken],
    output_dir: Path,
    is_flat: bool,
    cancel: asyncio.Event,
    abort: asyncio.Event,
    stats: Stats,
) -> None:
    author_dir = output_dir / safe_filename(author)
    author_dir.mkdir(parents=True, exist_ok=True)
    failed_path = author_dir / FAILED_FILENAME

    # Очередь с backpressure: не даём пагинации убегать далеко вперёд воркеров.
    # Это критично для boosty — ссылки на видео протухают.
    queue: asyncio.Queue[Optional[DownloadTask]] = asyncio.Queue(maxsize=WORKERS * 3)

    async with BoostyClient(token) as client:
        total = await client.blog_post_count(author)
        log.info("Постов у автора: %d", total)

        # CDN-сессия без auth-заголовков — многие CDN отклоняют лишние заголовки
        cdn_headers = {"User-Agent": client._headers["User-Agent"]}
        async with aiohttp.ClientSession(headers=cdn_headers) as cdn_session:

            fmt = "Посты: {n_fmt}/{total_fmt} {percentage:3.0f}%|{bar}| {elapsed}<{remaining}{postfix}"
            with tqdm_asyncio(total=total, unit="пост", bar_format=fmt, position=0) as post_pbar:

                # Запускаем воркеров после создания post_pbar — он нужен им для постфикса
                workers = [
                    asyncio.create_task(
                        _worker(i, queue, cdn_session, client._session,
                                stats, cancel, abort, failed_path, post_pbar)
                    )
                    for i in range(WORKERS)
                ]

                try:
                    async for post in client.iter_posts(author):
                        if cancel.is_set():
                            break

                        if not post.has_access:
                            async with stats._lock:
                                stats.no_access += 1
                            post_pbar.update(1)
                            continue

                        # Папка поста
                        if is_flat:
                            post_dir = author_dir
                        else:
                            post_dir = _post_dir(author_dir, post)
                            post_dir.mkdir(parents=True, exist_ok=True)

                        # Сохраняем текст поста
                        if text := post_to_text(post.text_blocks):
                            if is_flat:
                                date = datetime.fromtimestamp(post.publish_time, tz=timezone.utc).strftime("%Y-%m-%d")
                                slug = safe_filename(post.title or "post")[:50]
                                tf = post_dir / f"{date}_{slug}_{CONTENT_FILENAME}"
                            else:
                                tf = post_dir / CONTENT_FILENAME
                            if not tf.exists():
                                try:
                                    async with aiofiles.open(tf, "w", encoding="utf-8") as f:
                                        await f.write(text)
                                except Exception as e:
                                    log.error("Ошибка сохранения текста поста %s: %s", post.id, e)

                        # Кладём файлы в очередь.
                        # queue.put() блокируется если очередь полна — это намеренный
                        # backpressure, не даём пагинации уходить далеко вперёд.
                        for task in make_tasks(post, post_dir, is_flat):
                            if cancel.is_set():
                                break
                            await queue.put(task)

                        post_pbar.update(1)

                finally:
                    # Отправляем по одному sentinel на каждого воркера
                    for _ in workers:
                        await queue.put(None)
                    # Ждём пока все воркеры обработают свои sentinel и завершатся
                    await asyncio.gather(*workers, return_exceptions=True)

    log.info("Готово. Файлы: %s", author_dir)
    if failed_path.exists():
        log.info("Часть файлов не скачана. См. %s", failed_path)

# ---------------------------------------------------------------------------
# Точка входа
# ---------------------------------------------------------------------------

def main() -> None:
    parser = argparse.ArgumentParser(description="Загрузка медиа с boosty.to")
    parser.add_argument("-a", "--author", help="Ник автора или ссылка на профиль")
    parser.add_argument("-o", "--output", type=Path, help="Папка для загрузок (по умолчанию — рядом со скриптом)")
    parser.add_argument("-f", "--flat", action="store_true", help="Все файлы в одну папку без подпапок по постам")
    args = parser.parse_args()

    author = extract_nickname(args.author or "")
    if not author:
        author = extract_nickname(input("Ник автора (boosty.to/...): "))
    if not author:
        print("Ник автора не указан.")
        sys.exit(1)

    output_dir = (args.output or SCRIPT_DIR).resolve()

    token = load_token()
    if token and token.is_expired():
        log.warning("Токен истёк.")
        token = None
    if not token:
        token = prompt_token()
    if token:
        exp = datetime.fromtimestamp(token.expires_at, tz=timezone.utc).strftime("%Y-%m-%d %H:%M UTC")
        log.info("Авторизован (токен до %s).", exp)

    cancel = asyncio.Event()
    abort  = asyncio.Event()
    stats  = Stats()
    done   = threading.Event()

    # loop_ref: передаём ссылку на event loop из фонового потока в главный
    loop_ref: list[asyncio.AbstractEventLoop] = []

    def _run_thread() -> None:
        loop = asyncio.new_event_loop()
        asyncio.set_event_loop(loop)
        loop_ref.append(loop)
        try:
            loop.run_until_complete(
                run(author, token, output_dir, args.flat, cancel, abort, stats)
            )
        except Exception as e:
            log.error("Критическая ошибка: %s", e)
        finally:
            loop.close()
            done.set()

    thread = threading.Thread(target=_run_thread, daemon=True)
    thread.start()

    # Ждём пока loop стартует
    while not loop_ref and thread.is_alive():
        time.sleep(0.01)

    def _notify(event: asyncio.Event) -> None:
        if loop_ref and not loop_ref[0].is_closed():
            loop_ref[0].call_soon_threadsafe(event.set)

    try:
        # join() с таймаутом — единственный надёжный способ поймать Ctrl+C на Windows
        while not done.wait(timeout=0.2):
            pass
    except KeyboardInterrupt:
        sys.stderr.write("\r\033[K\033[33m[СТОП] Завершаем текущие загрузки... (повторный Ctrl+C — прервать немедленно)\033[0m\n")
        sys.stderr.flush()
        _notify(cancel)
        try:
            while not done.wait(timeout=0.2):
                pass
        except KeyboardInterrupt:
            sys.stderr.write("\r\033[K\033[1;31m[ПРИНУДИТЕЛЬНО] Прерываем активные загрузки...\033[0m\n")
            sys.stderr.flush()
            _notify(abort)
            while not done.wait(timeout=0.2):
                pass

    stats.print_summary()

    try:
        input("\nНажмите Enter для выхода...")
    except (KeyboardInterrupt, EOFError):
        pass


if __name__ == "__main__":
    main()
