#!/usr/bin/env python3
"""
CLI script to download media from boosty.to.
Asks for author nickname, creates folder by name, downloads all available media and post text.
Supports auth via token; resumes by skipping existing files; logs failed downloads to failed.txt.
"""

import argparse
import asyncio
import base64
import json
import logging
import os
import re
import sys
import threading
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Optional
from urllib.parse import parse_qsl, urlencode, urlparse, urlunparse

import aiofiles
import aiohttp
from tqdm.asyncio import tqdm_asyncio

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

SCRIPT_DIR = Path(__file__).resolve().parent
TOKEN_ENV = "BOOSTY_TOKEN"
TOKEN_FILENAME = ".boosty_token"
FAILED_FILENAME = "failed.txt"
CONTENT_FILENAME = "content.txt"
API_BASE = "https://api.boosty.to"
CHUNK_SIZE = 153_600
DOWNLOAD_TIMEOUT = 3600
POSTS_PAGE_LIMIT = 20
MAX_CONCURRENT_DOWNLOADS = 5

VIDEO_QUALITY_GRADE = ("ultra_hd", "full_hd", "high", "medium", "low")

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
    datefmt="%H:%M:%S",
)
logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Auth
# ---------------------------------------------------------------------------


@dataclass
class AuthToken:
    authorization: str
    cookie: str
    expires_in: int  # Unix timestamp (seconds) when token expires

    @staticmethod
    def from_str(data: str) -> Optional["AuthToken"]:
        data = data.strip()
        if not data:
            return None
        try:
            decoded = base64.b64decode(data)
            result = json.loads(decoded)
            expires = result["expires_in"]
            if expires > 1e12:  # milliseconds
                expires = int(expires / 1000)
            return AuthToken(
                authorization=result["authorization"],
                cookie=result["full_cookie"],
                expires_in=int(expires),
            )
        except Exception as e:
            logger.error("Failed to decode auth token: %s", e)
            return None

    def is_expired(self) -> bool:
        return datetime.now(timezone.utc).timestamp() >= self.expires_in


def find_token_path() -> Optional[Path]:
    for path in (SCRIPT_DIR / TOKEN_FILENAME, Path.home() / TOKEN_FILENAME):
        if path.is_file():
            return path
    return None


def load_token() -> Optional[AuthToken]:
    raw = os.environ.get(TOKEN_ENV)
    if raw:
        return AuthToken.from_str(raw)
    token_path = find_token_path()
    if token_path:
        try:
            raw = token_path.read_text(encoding="utf-8").strip()
            return AuthToken.from_str(raw)
        except Exception as e:
            logger.warning("Could not read token file %s: %s", token_path, e)
    return None


def save_token(token: AuthToken) -> None:
    path = SCRIPT_DIR / TOKEN_FILENAME
    raw = base64.b64encode(
        json.dumps(
            {
                "authorization": token.authorization,
                "full_cookie": token.cookie,
                "expires_in": token.expires_in * 1000,
            }
        ).encode()
    ).decode()
    path.write_text(raw, encoding="utf-8")
    logger.info("Token saved to %s", path)


def prompt_token() -> Optional[AuthToken]:
    print(
        "\nTo get a token: log in at boosty.to, open F12 → Console, run the auth script\n"
        "(see README or https://github.com/lowfc/boosty_downloader), then paste the output here."
    )
    raw = input("Paste token (or Enter to skip auth): ").strip()
    if not raw:
        return None
    token = AuthToken.from_str(raw)
    if token:
        save_token(token)
    return token


# ---------------------------------------------------------------------------
# Utils
# ---------------------------------------------------------------------------


def sign_url(url: str, qs: str) -> str:
    parsed = urlparse(url)
    existing = dict(parse_qsl(parsed.query)) if parsed.query else {}
    if qs.startswith("?"):
        qs = qs[1:]
    for k, v in parse_qsl(qs):
        if k not in existing:
            existing[k] = v
    new_query = urlencode(existing)
    return parsed._replace(query=new_query).geturl()


def validate_windows_dir_name(name: str) -> str:
    illegal = r'[<>:"/\\|?*\x00-\x1f]'
    reserved = {
        "CON", "PRN", "AUX", "NUL",
        "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
        "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9",
    }
    clean = re.sub(illegal, "_", name).strip(" .")
    if Path(clean).stem.upper() in reserved:
        clean = f"_{clean}_"
    return clean[:255] or "unnamed"


# ---------------------------------------------------------------------------
# API models (simplified from boosty_downloader)
# ---------------------------------------------------------------------------


@dataclass
class BoostyImageDto:
    id: str
    url: str
    width: int
    height: int
    size: int


@dataclass
class BoostyPlayerUrlDto:
    url: str
    size: str


@dataclass
class BoostyVideoDto:
    id: str
    title: str
    player_urls: dict = field(default_factory=dict)  # quality -> BoostyPlayerUrlDto

    def get_title(self) -> str:
        return f"{self.title or self.id}.mp4"


@dataclass
class BoostyAudioDto:
    id: str
    url: str
    size: int
    title: str

    def get_title(self) -> str:
        return self.title or f"{self.id}.mp3"


@dataclass
class BoostyFileDto:
    id: str
    url: str
    size: int
    title: str


@dataclass
class BoostyTextDto:
    content: str
    modificator: str


@dataclass
class BoostyLinkDto:
    content: str
    url: str


@dataclass
class BoostyListDto:
    style: str
    items: list = field(default_factory=list)


@dataclass
class BoostyPostTextDto:
    content: list = field(default_factory=list)


@dataclass
class BoostyPostDto:
    has_access: bool
    id: str
    int_id: int
    publish_time: int
    title: Optional[str] = None
    signed_query: str = ""
    text_content: BoostyPostTextDto = field(default_factory=BoostyPostTextDto)
    media: list = field(default_factory=list)


@dataclass
class BoostyExtraDto:
    is_last: bool
    offset: str


@dataclass
class BoostyPostsListDto:
    extra: BoostyExtraDto
    data: list = field(default_factory=list)


# ---------------------------------------------------------------------------
# DraftJS → plain text (simplified from boosty_downloader)
# ---------------------------------------------------------------------------


def _parse_boosty_text(content_json: str) -> tuple:
    if not content_json:
        return "", "unstyled", []
    try:
        data = json.loads(content_json)
        text = data[0]
        block_type = data[1]
        styles = data[2] if len(data) > 2 else []
        return text, block_type, styles
    except (json.JSONDecodeError, IndexError, TypeError):
        return "", "unstyled", []


def _to_plain_text(item_list: list) -> str:
    result = []

    def process_list(items: list, level: int = 0) -> list:
        lines = []
        indent = "  " * level
        for i in items:
            text_parts = [
                _parse_boosty_text(d.get("content"))[0]
                for d in i.get("data", [])
            ]
            lines.append(f"{indent}- {''.join(text_parts)}")
            for sub in i.get("items", []):
                lines.extend(process_list(sub, level + 1))
        return lines

    for item in item_list:
        if hasattr(item, "url"):  # BoostyLinkDto
            text, _, _ = _parse_boosty_text(item.content)
            result.append(f"{text} (ссылка: {item.url})")
        elif hasattr(item, "modificator"):  # BoostyTextDto
            if item.modificator == "BLOCK_END":
                result.append("\n")
            else:
                text, _, _ = _parse_boosty_text(item.content)
                result.append(text)
        elif hasattr(item, "items"):  # BoostyListDto
            result.extend(process_list(item.items))
    return "".join(result)


# ---------------------------------------------------------------------------
# API client
# ---------------------------------------------------------------------------


class BoostyClient:
    def __init__(self, auth_token: Optional[AuthToken] = None,
                 chunk_size: int = CHUNK_SIZE,
                 timeout: int = DOWNLOAD_TIMEOUT):
        self.auth_token = auth_token
        self.chunk_size = chunk_size
        self.timeout = timeout
        self._base_headers = {
            "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36",
            "Sec-Ch-Ua": '"Google Chrome";v="123", "Not:A-Brand";v="8", "Chromium";v="123"',
            "Sec-Ch-Ua-Mobile": "?0",
            "Sec-Ch-Ua-Platform": '"Windows"',
        }

    def _headers(self) -> dict:
        h = dict(self._base_headers)
        if self.auth_token:
            h["Authorization"] = f"Bearer {self.auth_token.authorization}"
            h["Cookie"] = self.auth_token.cookie
        return h

    def _wrap_media(self, media: dict):
        t = media.get("type")
        if t == "image":
            if "width" not in media:
                return None
            return BoostyImageDto(
                id=media["id"],
                url=media["url"],
                width=media["width"],
                height=media["height"],
                size=media["size"],
            )
        if t == "ok_video":
            video = BoostyVideoDto(id=media["id"], title=media.get("title"))
            for url in media.get("playerUrls", []):
                if url.get("url") and url.get("type") in VIDEO_QUALITY_GRADE:
                    video.player_urls[url["type"]] = BoostyPlayerUrlDto(
                        url=url["url"], size=url["type"]
                    )
            return video
        if t == "audio_file":
            return BoostyAudioDto(
                id=media["id"],
                url=media["url"],
                size=media["size"],
                title=media.get("title", ""),
            )
        if t == "file":
            return BoostyFileDto(
                id=media["id"],
                url=media["url"],
                size=media["size"],
                title=media.get("title", ""),
            )
        if t in ("text", "header"):
            return BoostyTextDto(
                content=media.get("content", ""),
                modificator=media.get("modificator", ""),
            )
        if t == "link":
            return BoostyLinkDto(content=media.get("content", ""), url=media.get("url", ""))
        if t == "list":
            return BoostyListDto(style=media.get("style", ""), items=media.get("items", []))
        return None

    def _wrap_post(self, content: dict) -> BoostyPostDto:
        text_content = BoostyPostTextDto()
        result = BoostyPostDto(
            has_access=content.get("hasAccess", False),
            id=content["id"],
            int_id=content["intId"],
            title=content.get("title"),
            publish_time=content["publishTime"],
            signed_query=content.get("signedQuery", ""),
        )
        for media in content.get("data", []):
            wrapped = self._wrap_media(media)
            if wrapped is None:
                continue
            if isinstance(wrapped, (BoostyTextDto, BoostyLinkDto, BoostyListDto)):
                text_content.content.append(wrapped)
            else:
                result.media.append(wrapped)
        result.text_content = text_content
        return result

    async def get_post_info(self, author: str, post_id: str) -> BoostyPostDto:
        url = f"{API_BASE}/v1/blog/{author}/post/{post_id}"
        async with aiohttp.ClientSession(headers=self._headers()) as session:
            async with session.get(url) as resp:
                resp.raise_for_status()
                content = await resp.json()
        return self._wrap_post(content)

    async def get_posts_list(
        self, author: str, limit: int = POSTS_PAGE_LIMIT, offset: Optional[str] = None
    ) -> BoostyPostsListDto:
        params = {"limit": limit, "reply_limit": 0, "comments_limit": 0}
        if offset:
            params["offset"] = offset
        url = f"{API_BASE}/v1/blog/{author}/post/"
        async with aiohttp.ClientSession(headers=self._headers()) as session:
            async with session.get(url, params=params) as resp:
                resp.raise_for_status()
                content = await resp.json()
        extra = content["extra"]
        result = BoostyPostsListDto(
            extra=BoostyExtraDto(is_last=extra["isLast"], offset=extra.get("offset", "")),
        )
        for post in content["data"]:
            result.data.append(self._wrap_post(post))
        return result

    async def fetch_all_posts(self, author: str) -> list[BoostyPostDto]:
        all_posts = []
        offset = None
        while True:
            page = await self.get_posts_list(author, limit=POSTS_PAGE_LIMIT, offset=offset)
            all_posts.extend(page.data)
            if page.extra.is_last:
                break
            offset = page.extra.offset
            if not offset:
                break
        return all_posts


# ---------------------------------------------------------------------------
# Download task
# ---------------------------------------------------------------------------


@dataclass
class DownloadItem:
    url: str
    path: Path
    fetch_size: bool = False
    media_type: str = "file"  # "photo" | "video" | "audio" | "file"


def build_download_items(post: BoostyPostDto, post_path: Path) -> list[DownloadItem]:
    items = []
    for m in post.media:
        if isinstance(m, BoostyImageDto):
            items.append(DownloadItem(url=m.url, path=post_path / f"{m.id}.jpg", media_type="photo"))
        elif isinstance(m, BoostyVideoDto):
            for q in VIDEO_QUALITY_GRADE:
                info = m.player_urls.get(q)
                if info and info.url:
                    path = post_path / validate_windows_dir_name(m.get_title())
                    items.append(DownloadItem(url=info.url, path=path, fetch_size=True, media_type="video"))
                    break
        elif isinstance(m, BoostyAudioDto) and post.signed_query:
            url = sign_url(m.url, post.signed_query)
            path = post_path / validate_windows_dir_name(m.get_title())
            items.append(DownloadItem(url=url, path=path, media_type="audio"))
        elif isinstance(m, BoostyFileDto) and post.signed_query:
            url = sign_url(m.url, post.signed_query)
            path = post_path / validate_windows_dir_name(m.title)
            items.append(DownloadItem(url=url, path=path, media_type="file"))
    return items


@dataclass
class DownloadStats:
    photos_downloaded: int = 0
    photos_skipped: int = 0
    videos_downloaded: int = 0
    videos_skipped: int = 0
    other_downloaded: int = 0
    other_skipped: int = 0
    errors: int = 0

    def total_downloaded(self) -> int:
        return self.photos_downloaded + self.videos_downloaded + self.other_downloaded

    def total_skipped(self) -> int:
        return self.photos_skipped + self.videos_skipped + self.other_skipped


def _update_stats(stats: Optional[DownloadStats], media_type: str, skipped: bool, error: bool) -> None:
    if stats is None:
        return
    if error:
        stats.errors += 1
        return
    if media_type == "photo":
        if skipped:
            stats.photos_skipped += 1
        else:
            stats.photos_downloaded += 1
    elif media_type == "video":
        if skipped:
            stats.videos_skipped += 1
        else:
            stats.videos_downloaded += 1
    else:
        if skipped:
            stats.other_skipped += 1
        else:
            stats.other_downloaded += 1


async def download_file(
    session: aiohttp.ClientSession,
    item: DownloadItem,
    failed_path: Path,
    pbar: Optional[tqdm_asyncio] = None,
    semaphore: Optional[asyncio.Semaphore] = None,
    stats: Optional[DownloadStats] = None,
) -> bool:
    if item.path.exists():
        _update_stats(stats, item.media_type, skipped=True, error=False)
        return True

    async def _do_download() -> bool:
        try:
            async with session.get(item.url) as resp:
                resp.raise_for_status()
                size = resp.content_length if item.fetch_size else None
                if pbar is not None and size is not None:
                    pbar.total = (pbar.total or 0) + size
                async with aiofiles.open(item.path, "wb") as f:
                    async for chunk in resp.content.iter_chunked(CHUNK_SIZE):
                        if chunk:
                            await f.write(chunk)
                            if pbar is not None:
                                pbar.update(len(chunk))
            _update_stats(stats, item.media_type, skipped=False, error=False)
            return True
        except Exception as e:
            logger.exception("Download failed %s: %s", item.url, e)
            line = f"{item.path}\t{item.url}\n"
            async with aiofiles.open(failed_path, "a", encoding="utf-8") as f:
                await f.write(line)
            _update_stats(stats, item.media_type, skipped=False, error=True)
            return False

    if semaphore is not None:
        async with semaphore:
            return await _do_download()
    return await _do_download()


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def get_post_dir(author_dir: Path, post: BoostyPostDto) -> Path:
    title_part = validate_windows_dir_name(post.title or "post")
    return author_dir / f"{title_part}_{post.id}"


async def process_post(
    client: BoostyClient,
    author: str,
    post: BoostyPostDto,
    author_dir: Path,
    failed_path: Path,
    session: aiohttp.ClientSession,
    semaphore: asyncio.Semaphore,
    pbar: Optional[tqdm_asyncio] = None,
    post_index: int = 0,
    total_posts: int = 0,
    stats: Optional[DownloadStats] = None,
) -> None:
    if not post.has_access:
        logger.warning("No access to post %s, skipping", post.id)
        return
    post_dir = get_post_dir(author_dir, post)
    post_dir.mkdir(parents=True, exist_ok=True)

    if pbar is not None and total_posts > 0:
        pbar.set_postfix(post=f"{post_index}/{total_posts}", refresh=True)

    # Save text
    content_file = post_dir / CONTENT_FILENAME
    if not content_file.exists() and post.text_content.content:
        try:
            text = _to_plain_text(post.text_content.content)
            if post.title:
                text = f"{post.title}\n\n{text}"
            post_time = datetime.fromtimestamp(post.publish_time, tz=timezone.utc)
            text += f"\n\n---\nPublished {post_time.strftime('%d.%m.%Y %H:%M')} UTC\n"
            async with aiofiles.open(content_file, "w", encoding="utf-8") as f:
                await f.write(text)
        except Exception as e:
            logger.exception("Failed to save post text for %s: %s", post.id, e)

    items = build_download_items(post, post_dir)
    if not items:
        return
    await asyncio.gather(
        *[
            download_file(session, item, failed_path, pbar, semaphore, stats)
            for item in items
        ]
    )


async def run(
    author: str,
    token: Optional[AuthToken],
    download_dir: Path,
    cancel_event: asyncio.Event,
) -> DownloadStats:
    author_dir = download_dir / validate_windows_dir_name(author)
    author_dir.mkdir(parents=True, exist_ok=True)
    failed_path = author_dir / FAILED_FILENAME
    stats = DownloadStats()

    client = BoostyClient(auth_token=token)
    logger.info("Fetching posts list for %s...", author)
    posts = await client.fetch_all_posts(author)
    logger.info("Found %d posts.", len(posts))

    semaphore = asyncio.Semaphore(MAX_CONCURRENT_DOWNLOADS)
    timeout = aiohttp.ClientTimeout(total=DOWNLOAD_TIMEOUT)
    total_posts = len(posts)
    with tqdm_asyncio(unit="B", unit_scale=True, desc="Downloading") as pbar:
        async with aiohttp.ClientSession(
            headers=client._headers(), timeout=timeout
        ) as session:
            for i, post in enumerate(posts, start=1):
                if cancel_event.is_set():
                    logger.info("Interrupt requested, stopping after current post.")
                    break
                await process_post(
                    client, author, post, author_dir, failed_path,
                    session, semaphore, pbar,
                    post_index=i,
                    total_posts=total_posts,
                    stats=stats,
                )

    logger.info("Done. Output: %s", author_dir)
    if failed_path.exists():
        logger.info("Some downloads failed. See %s", failed_path)
    return stats


def _print_stats(stats: DownloadStats) -> None:
    print("\n" + "=" * 50)
    print("Статистика загрузки")
    print("=" * 50)
    print(f"  Фото:     загружено {stats.photos_downloaded}, пропущено {stats.photos_skipped}")
    print(f"  Видео:    загружено {stats.videos_downloaded}, пропущено {stats.videos_skipped}")
    other = stats.other_downloaded + stats.other_skipped
    if other:
        print(f"  Прочее:   загружено {stats.other_downloaded}, пропущено {stats.other_skipped}")
    print(f"  Ошибок:   {stats.errors}")
    print(f"  Итого:    загружено {stats.total_downloaded()}, пропущено {stats.total_skipped()}")
    print("=" * 50)


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Загрузка медиа с boosty.to по нику автора."
    )
    parser.add_argument(
        "-a", "--author",
        type=str,
        default=None,
        help="Ник автора (boosty.to/...). Если не указан — запрашивается в консоли.",
    )
    parser.add_argument(
        "-o", "--output",
        type=Path,
        default=None,
        help="Папка для загрузок. По умолчанию — рядом со скриптом.",
    )
    args = parser.parse_args()

    author = (args.author or "").strip().replace("/", "").strip()
    if not author:
        author = input("Author nickname (boosty.to/...): ").strip().replace("/", "").strip()
    if not author:
        print("No author given.")
        sys.exit(1)

    download_dir = args.output if args.output is not None else SCRIPT_DIR
    download_dir = download_dir.resolve()

    token = load_token()
    if token and token.is_expired():
        logger.warning("Saved token is expired.")
        token = None
    if not token:
        token = prompt_token()
    if token:
        logger.info("Using auth token (expires %s).", datetime.fromtimestamp(token.expires_in, tz=timezone.utc))

    cancel_event = asyncio.Event()
    loop = asyncio.new_event_loop()
    asyncio.set_event_loop(loop)
    stats_result: list[DownloadStats] = []

    def run_async() -> None:
        nonlocal stats_result
        try:
            stats = loop.run_until_complete(
                run(author, token, download_dir, cancel_event)
            )
            stats_result.append(stats)
        finally:
            loop.close()

    thread = threading.Thread(target=run_async)
    thread.start()
    try:
        thread.join()
    except KeyboardInterrupt:
        print("\nПрервано. Ожидание завершения текущих загрузок...")
        loop.call_soon_threadsafe(cancel_event.set)
        thread.join()
        if stats_result:
            _print_stats(stats_result[0])
        input("\nНажмите Enter для выхода...")
        sys.exit(0)

    if stats_result:
        _print_stats(stats_result[0])
    input("\nНажмите Enter для выхода...")


if __name__ == "__main__":
    main()
