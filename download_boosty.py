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
from urllib.parse import parse_qsl, urlencode, urlparse

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
DOWNLOAD_TIMEOUT = 3600
POSTS_PAGE_LIMIT = 20
MAX_CONCURRENT_DOWNLOADS = 5

VIDEO_QUALITY_GRADE = ("ultra_hd", "full_hd", "high", "medium", "low")

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------

class TqdmLoggingHandler(logging.Handler):
    def emit(self, record):
        try:
            msg = self.format(record)
            tqdm_asyncio.write(msg)
            self.flush()
        except Exception:
            self.handleError(record)

# Configure logging to use tqdm.write to avoid breaking progress bars
tqdm_handler = TqdmLoggingHandler()
tqdm_handler.setFormatter(logging.Formatter("%(asctime)s [%(levelname)s] %(message)s", datefmt="%H:%M:%S"))

logging.basicConfig(
    level=logging.INFO,
    handlers=[tqdm_handler]
)
logger = logging.getLogger(__name__)



# ---------------------------------------------------------------------------
# Helper functions to handle Windows file locking behavior
# ---------------------------------------------------------------------------
async def safe_unlink(path: Path, max_retries: int = 5, delay: float = 0.5):
    """Safely unlink a file with retries for Windows WinError 32."""
    if not path.exists():
        return
    for i in range(max_retries):
        try:
            path.unlink()
            return
        except PermissionError:
            if i < max_retries - 1:
                await asyncio.sleep(delay)
            else:
                raise

async def safe_replace(src_path: Path, dest_path: Path, max_retries: int = 5, delay: float = 0.5):
    """Safely replace/rename a file with retries for Windows WinError 32."""
    for i in range(max_retries):
        try:
            # os.replace is safer than path.replace on Windows for atomicity
            os.replace(src_path, dest_path)
            return
        except PermissionError:
            if i < max_retries - 1:
                await asyncio.sleep(delay)
            else:
                raise


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


# Removed text DTOs to simplify parsing

@dataclass
class BoostyPostDto:
    has_access: bool
    id: str
    int_id: int
    publish_time: int
    title: Optional[str] = None
    signed_query: str = ""
    text_content: list = field(default_factory=list)
    media: list = field(default_factory=list)


@dataclass
class BoostyExtraDto:
    is_last: bool
    offset: str


@dataclass
class BoostyPostsListDto:
    extra: BoostyExtraDto
    data: list = field(default_factory=list)
    total: int = 0


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
        t = item.get("type")
        if t == "link":
            text, _, _ = _parse_boosty_text(item.get("content"))
            result.append(f"{text} (ссылка: {item.get('url', '')})")
        elif t in ("text", "header"):
            if item.get("modificator") == "BLOCK_END":
                result.append("\n")
            else:
                text, _, _ = _parse_boosty_text(item.get("content"))
                result.append(text)
        elif t == "list":
            result.extend(process_list(item.get("items", [])))
    return "".join(result)


# ---------------------------------------------------------------------------
# API client
# ---------------------------------------------------------------------------


class BoostyClient:
    def __init__(self, auth_token: Optional[AuthToken] = None,
                 timeout: int = DOWNLOAD_TIMEOUT):
        self.auth_token = auth_token
        self.timeout = timeout
        self._base_headers = {
            "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36",
            "Sec-Ch-Ua": '"Google Chrome";v="123", "Not:A-Brand";v="8", "Chromium";v="123"',
            "Sec-Ch-Ua-Mobile": "?0",
            "Sec-Ch-Ua-Platform": '"Windows"',
        }
        self._session: Optional[aiohttp.ClientSession] = None

    def _headers(self) -> dict:
        h = dict(self._base_headers)
        if self.auth_token:
            h["Authorization"] = f"Bearer {self.auth_token.authorization}"
            h["Cookie"] = self.auth_token.cookie
        return h

    async def __aenter__(self) -> "BoostyClient":
        self._session = aiohttp.ClientSession(headers=self._headers())
        return self

    async def __aexit__(self, *exc) -> None:
        if self._session:
            await self._session.close()
            self._session = None

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
        return None

    def _wrap_post(self, content: dict) -> BoostyPostDto:
        result = BoostyPostDto(
            has_access=content.get("hasAccess", False),
            id=content["id"],
            int_id=content["intId"],
            title=content.get("title"),
            publish_time=content["publishTime"],
            signed_query=content.get("signedQuery", ""),
        )
        for media in content.get("data", []):
            t = media.get("type")
            if t in ("text", "header", "link", "list"):
                result.text_content.append(media)
            else:
                wrapped = self._wrap_media(media)
                if wrapped is not None:
                    result.media.append(wrapped)
        return result

    async def get_post_info(self, author: str, post_id: str) -> BoostyPostDto:
        url = f"{API_BASE}/v1/blog/{author}/post/{post_id}"
        async with self._session.get(url) as resp:
            resp.raise_for_status()
            content = await resp.json()
        return self._wrap_post(content)

    async def get_blog_total_posts(self, author: str) -> int:
        url = f"{API_BASE}/v1/blog/{author}"
        async with self._session.get(url) as resp:
            resp.raise_for_status()
            content = await resp.json()
        count_obj = content.get("count", {})
        return count_obj.get("posts", 0)

    async def get_posts_list(
        self, author: str, limit: int = POSTS_PAGE_LIMIT, offset: Optional[str] = None
    ) -> BoostyPostsListDto:
        params = {"limit": limit, "reply_limit": 0, "comments_limit": 0}
        if offset:
            params["offset"] = offset
        url = f"{API_BASE}/v1/blog/{author}/post/"
        async with self._session.get(url, params=params) as resp:
            resp.raise_for_status()
            content = await resp.json()
        extra = content["extra"]
        result = BoostyPostsListDto(
            extra=BoostyExtraDto(is_last=extra["isLast"], offset=extra.get("offset", "")),
            total=extra.get("total", 0)  # Still keep this as fallback
        )
        for post in content["data"]:
            result.data.append(self._wrap_post(post))
        return result

    async def fetch_posts_lazy(self, author: str):
        """Async generator that yields pages of posts (full BoostyPostsListDto)."""
        offset = None
        while True:
            page = await self.get_posts_list(author, limit=POSTS_PAGE_LIMIT, offset=offset)
            yield page
            if page.extra.is_last:
                break
            offset = page.extra.offset
            if not offset:
                break


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
    # Using enumerate with 1-based index and formatting with leading zeros (e.g. 001, 002)
    # assuming less than 1000 media items per post is common
    for index, m in enumerate(post.media, start=1):
        prefix = f"{index:03d}_"
        if isinstance(m, BoostyImageDto):
            items.append(DownloadItem(url=m.url, path=post_path / f"{prefix}{m.id}.jpg", media_type="photo"))
        elif isinstance(m, BoostyVideoDto):
            for q in VIDEO_QUALITY_GRADE:
                info = m.player_urls.get(q)
                if info and info.url:
                    title = m.get_title()
                    path = post_path / validate_windows_dir_name(f"{prefix}{title}")
                    items.append(DownloadItem(url=info.url, path=path, fetch_size=True, media_type="video"))
                    break
        elif isinstance(m, BoostyAudioDto) and post.signed_query:
            url = sign_url(m.url, post.signed_query)
            title = m.get_title()
            path = post_path / validate_windows_dir_name(f"{prefix}{title}")
            items.append(DownloadItem(url=url, path=path, media_type="audio"))
        elif isinstance(m, BoostyFileDto) and post.signed_query:
            url = sign_url(m.url, post.signed_query)
            title = m.title
            path = post_path / validate_windows_dir_name(f"{prefix}{title}")
            items.append(DownloadItem(url=url, path=path, media_type="file"))
    return items


@dataclass
class DownloadContext:
    client: "BoostyClient"
    session: aiohttp.ClientSession
    cdn_session: aiohttp.ClientSession
    author_dir: Path
    failed_path: Path
    semaphore: asyncio.Semaphore
    post_pbar: Optional[tqdm_asyncio] = None
    byte_pbar: Optional[tqdm_asyncio] = None
    stats: Optional["DownloadStats"] = None
    total_posts: int = 0


@dataclass
class DownloadStats:
    photos_downloaded: int = 0
    photos_skipped: int = 0
    videos_downloaded: int = 0
    videos_skipped: int = 0
    other_downloaded: int = 0
    other_skipped: int = 0
    posts_no_access: int = 0
    errors: int = 0

    def total_downloaded(self) -> int:
        return self.photos_downloaded + self.videos_downloaded + self.other_downloaded

    def total_skipped(self) -> int:
        return self.photos_skipped + self.videos_skipped + self.other_skipped


def _update_stats(ctx: DownloadContext, media_type: str, skipped: bool, error: bool) -> None:
    if ctx.stats is None:
        return
    st = ctx.stats
    if error:
        st.errors += 1
        return
    if media_type == "photo":
        if skipped:
            st.photos_skipped += 1
        else:
            st.photos_downloaded += 1
    elif media_type == "video":
        if skipped:
            st.videos_skipped += 1
        else:
            st.videos_downloaded += 1
    else:
        if skipped:
            st.other_skipped += 1
        else:
            st.other_downloaded += 1


async def download_file(
    ctx: DownloadContext,
    item: DownloadItem,
    referer: Optional[str] = None,
) -> bool:
    if item.path.exists():
        _update_stats(ctx, item.media_type, skipped=True, error=False)
        return True

    MAX_RETRIES = 5
    RETRY_DELAY = 3

    async def _do_download() -> bool:
        part_path = item.path.with_suffix(item.path.suffix + ".part")
        
        # Try to resume from existing .part file
        initial_size = 0
        if part_path.exists():
            initial_size = part_path.stat().st_size
            
        part_deleted_this_run = False

        parsed = urlparse(item.url)
        hostname = parsed.hostname or ""
        is_cdn = "boosty.to" not in hostname
        dl_session = ctx.cdn_session if is_cdn else ctx.session
        timeout = aiohttp.ClientTimeout(total=None, sock_read=120, sock_connect=30)

        for attempt in range(1, MAX_RETRIES + 1):
            downloaded_bytes = 0
            size_added_to_total = False
            total_size_expected = None
            headers = {}
            if initial_size > 0:
                headers["Range"] = f"bytes={initial_size}-"
            if referer:
                headers["Referer"] = referer
            
            try:
                async with dl_session.get(item.url, headers=headers, timeout=timeout) as resp:
                    resp.raise_for_status()
                    
                    # Handle full vs partial content responses
                    if resp.status == 206: # Partial content
                        total_size_expected = initial_size + (resp.content_length or 0)
                        open_mode = "ab" # Append
                    else:
                        # Server didn't respect Range or we started from 0
                        total_size_expected = resp.content_length if item.fetch_size else None
                        initial_size = 0
                        open_mode = "wb"

                    if ctx.byte_pbar is not None and total_size_expected is not None and not size_added_to_total:
                        ctx.byte_pbar.total = (ctx.byte_pbar.total or 0) + total_size_expected - ctx.byte_pbar.n  # adjusting total
                        size_added_to_total = True

                    async with aiofiles.open(part_path, open_mode) as f:
                        try:
                            async for chunk in resp.content.iter_chunked(256 * 1024):
                                if chunk:
                                    await f.write(chunk)
                                    len_chunk = len(chunk)
                                    downloaded_bytes += len_chunk
                                    initial_size += len_chunk
                                    if ctx.byte_pbar is not None:
                                        ctx.byte_pbar.update(len_chunk)
                        except (aiohttp.ClientPayloadError, aiohttp.ClientError, asyncio.TimeoutError) as payload_err:
                            if downloaded_bytes > 0:
                                raise RuntimeError("Server closed connection prematurely. Will retry.") from payload_err
                            else:
                                raise RuntimeError(f"CDN dropped connection: {payload_err}") from payload_err

                # Validate if we got the expected size
                if item.fetch_size and total_size_expected is not None and initial_size < total_size_expected:
                   raise RuntimeError(f"Incomplete file: Expected {total_size_expected} bytes, got {initial_size} bytes.")

                # Success
                await safe_replace(part_path, item.path)
                _update_stats(ctx, item.media_type, skipped=False, error=False)
                return True
                
            except Exception as e:
                err_str = str(e)
                # If CDN returns 400, it might be due to a bad Range request OR an expired URL.
                is_400 = "400" in err_str or "Bad Request" in err_str or "403" in err_str or "Forbidden" in err_str
                
                if is_400:
                    if initial_size > 0 and not part_deleted_this_run:
                        logger.info("CDN returned 400/403 with Range header. Deleting .part and restarting from scratch for %s", item.path.name)
                        await safe_unlink(part_path)
                        initial_size = 0
                        part_deleted_this_run = True
                        continue  # Try immediately from scratch
                    else:
                        # 400/403 with NO range header (initial_size = 0) means the URL is just expired/invalid.
                        logger.error("Download failed (likely expired URL) %s: %s", item.url, e)
                        break  # Break out of the retry loop, no point hammering an expired URL

                if attempt < MAX_RETRIES:
                    logger.warning("Retry %d/%d for %s due to error: %s (Resuming from %s bytes)", 
                                   attempt, MAX_RETRIES, item.path.name, e, initial_size)
                    await asyncio.sleep(RETRY_DELAY)
                else:
                    logger.error("Download failed after %d attempts %s: %s", MAX_RETRIES, item.url, e)
                    break # To write to failed log

            # If loop breaks, it means failure
            
        line = f"{item.path}\t{item.url}\n"
        async with aiofiles.open(ctx.failed_path, "a", encoding="utf-8") as f:
            await f.write(line)
        _update_stats(ctx, item.media_type, skipped=False, error=True)
        return False

    async with ctx.semaphore:
        return await _do_download()


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def get_post_dir(author_dir: Path, post: BoostyPostDto) -> Path:
    # Format date as YY-MM-DD
    post_date = datetime.fromtimestamp(post.publish_time, tz=timezone.utc).strftime("%y-%m-%d")
    title_part = validate_windows_dir_name(post.title or "post")
    return author_dir / f"{post_date}_{title_part}_{post.id}"


async def process_post(
    ctx: DownloadContext,
    author: str,
    post: BoostyPostDto,
    post_index: int = 0,
) -> None:
    if not post.has_access:
        logger.warning("No access to post %s, skipping", post.id)
        if ctx.stats is not None:
            ctx.stats.posts_no_access += 1
        if ctx.post_pbar is not None:
            ctx.post_pbar.update(1)
        return
    post_dir = get_post_dir(ctx.author_dir, post)
    post_dir.mkdir(parents=True, exist_ok=True)

    if ctx.post_pbar is not None:
        ctx.post_pbar.set_description(f"Processing post {post_index}/{ctx.total_posts or '??'}")

    # Save text
    content_file = post_dir / CONTENT_FILENAME
    if not content_file.exists() and post.text_content:
        try:
            text = _to_plain_text(post.text_content)
            if post.title:
                text = f"{post.title}\n\n{text}"
            post_time = datetime.fromtimestamp(post.publish_time, tz=timezone.utc)
            text += f"\n\n---\nPublished {post_time.strftime('%d.%m.%Y %H:%M')} UTC\n"
            async with aiofiles.open(content_file, "w", encoding="utf-8") as f:
                await f.write(text)
        except Exception as e:
            logger.error("Failed to save post text for %s: %s", post.id, e)

    items = build_download_items(post, post_dir)
    if not items:
        if ctx.post_pbar is not None:
            ctx.post_pbar.update(1)
        return
    
    post_url = f"https://boosty.to/{author}/posts/{post.id}"
    
    await asyncio.gather(
        *[
            download_file(
                ctx,
                item, 
                referer=post_url if "boosty.to" not in item.url else None
            )
            for item in items
        ]
    )
    if ctx.post_pbar is not None:
        ctx.post_pbar.update(1)


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

    async with BoostyClient(auth_token=token) as client:
        logger.info("Fetching blog info for %s...", author)
        total_posts = await client.get_blog_total_posts(author)

        semaphore = asyncio.Semaphore(MAX_CONCURRENT_DOWNLOADS)
        timeout = aiohttp.ClientTimeout(total=DOWNLOAD_TIMEOUT)

        post_index = 1
        # Dual progress bars: one for overall posts, one for aggregate raw download speed
        with tqdm_asyncio(total=total_posts, unit="post", desc="Posts   ", position=0) as post_pbar, \
             tqdm_asyncio(unit="B", unit_scale=True, desc="Download", position=1, leave=False) as byte_pbar:
            # CDN session for external links (MUST have Referer - tests show okcdn requires it or fails with 400)
            async with aiohttp.ClientSession(
                timeout=timeout,
                headers={"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36"}
            ) as cdn_session:

                ctx = DownloadContext(
                    client=client,
                    session=client._session,
                    cdn_session=cdn_session,
                    author_dir=author_dir,
                    failed_path=failed_path,
                    semaphore=semaphore,
                    post_pbar=post_pbar,
                    byte_pbar=byte_pbar,
                    stats=stats,
                    total_posts=total_posts
                )

                async for page in client.fetch_posts_lazy(author):
                    if cancel_event.is_set():
                        break

                    # Update total if it changed (e.g. new post published)
                    if page.total > 0:
                        post_pbar.total = page.total
                        ctx.total_posts = page.total

                    for post in page.data:
                        if cancel_event.is_set():
                            break
                        await process_post(
                            ctx, author, post, post_index=post_index
                        )
                        post_index += 1

    logger.info("Done. Output: %s", author_dir)
    if failed_path.exists():
        logger.info("Some downloads failed. See %s", failed_path)
    return stats


def _print_stats(stats: DownloadStats) -> None:
    print("\n" + "=" * 50)
    print("Статистика загрузки")
    print("=" * 50)

    # Use 15 chars for consistent label alignment
    def fmt(label, d, s):
        return f"  {label:<15} загружено {str(d):<5} пропущено {s}"

    print(fmt("Фото:", stats.photos_downloaded, stats.photos_skipped))
    print(fmt("Видео:", stats.videos_downloaded, stats.videos_skipped))

    other = stats.other_downloaded + stats.other_skipped
    if other:
        print(fmt("Прочее:", stats.other_downloaded, stats.other_skipped))

    if stats.posts_no_access:
        print(f"  {'Нет доступа:':<15} {stats.posts_no_access}")

    print(f"  {'Ошибок:':<15} {stats.errors}")

    print(fmt("Итого:", stats.total_downloaded(), stats.total_skipped()))
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
