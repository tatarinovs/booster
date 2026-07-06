package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	apiBase           = "https://api.boosty.to"
	postsPageLimit    = 20
	apiMaxRetries     = 5
	apiRetryBaseDelay = 2 * time.Second
	apiTimeout        = 30 * time.Second
)

// BoostyClient — HTTP-клиент для API boosty.to.
type BoostyClient struct {
	token   *AuthToken
	headers map[string]string
	http    *http.Client
}

func newBoostyClient(token *AuthToken) *BoostyClient {
	h := map[string]string{
		"User-Agent":         "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36",
		"Sec-Ch-Ua":          `"Google Chrome";v="123", "Not:A-Brand";v="8", "Chromium";v="123"`,
		"Sec-Ch-Ua-Mobile":   "?0",
		"Sec-Ch-Ua-Platform": `"Windows"`,
	}
	if token != nil {
		h["Authorization"] = "Bearer " + token.Authorization
		h["Cookie"] = token.Cookie
	}
	return &BoostyClient{
		token:   token,
		headers: h,
		http:    &http.Client{Timeout: apiTimeout},
	}
}

// httpError оборачивает HTTP-статус ошибки, аналог aiohttp.ClientResponseError.
type httpError struct {
	status int
	url    string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("HTTP %d for %s", e.status, e.url)
}

func (c *BoostyClient) get(ctx context.Context, rawURL string, params url.Values) (map[string]any, error) {
	full := rawURL
	if len(params) > 0 {
		full = rawURL + "?" + params.Encode()
	}

	var lastErr error
	for attempt := 1; attempt <= apiMaxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			if attempt == apiMaxRetries {
				logError("API %s — %d попыток исчерпано: %v", rawURL, attempt, err)
				return nil, err
			}
			delay := backoffDelay(attempt)
			logWarn("API %s — ошибка, повтор через %ds (%d/%d): %v", rawURL, int(delay.Seconds()), attempt, apiMaxRetries, err)
			sleepCtx(ctx, delay)
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 400 {
			herr := &httpError{status: resp.StatusCode, url: rawURL}
			if resp.StatusCode >= 400 && resp.StatusCode < 500 {
				logError("API %s → %v", rawURL, herr)
				return nil, herr
			}
			lastErr = herr
			if attempt == apiMaxRetries {
				logError("API %s — %d попыток исчерпано: %v", rawURL, attempt, herr)
				return nil, herr
			}
			delay := backoffDelay(attempt)
			logWarn("API %s — ошибка, повтор через %ds (%d/%d): %v", rawURL, int(delay.Seconds()), attempt, apiMaxRetries, herr)
			sleepCtx(ctx, delay)
			continue
		}

		if readErr != nil {
			lastErr = readErr
			if attempt == apiMaxRetries {
				return nil, readErr
			}
			delay := backoffDelay(attempt)
			sleepCtx(ctx, delay)
			continue
		}

		var data map[string]any
		if err := json.Unmarshal(body, &data); err != nil {
			return nil, err
		}
		return data, nil
	}
	return nil, lastErr
}

func backoffDelay(attempt int) time.Duration {
	d := apiRetryBaseDelay
	for i := 1; i < attempt; i++ {
		d *= 2
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// blogPostCount возвращает общее число постов автора.
func (c *BoostyClient) blogPostCount(ctx context.Context, author string) (int, error) {
	data, err := c.get(ctx, fmt.Sprintf("%s/v1/blog/%s", apiBase, author), nil)
	if err != nil {
		return 0, err
	}
	if count, ok := data["count"].(map[string]any); ok {
		if posts, ok := count["posts"].(float64); ok {
			return int(posts), nil
		}
	}
	return 0, nil
}

// iterPosts постранично запрашивает посты автора и отправляет их в канал.
// Канал закрывается по завершении; ошибки, если есть, публикуются в errCh (буфер 1).
func (c *BoostyClient) iterPosts(ctx context.Context, author string) (<-chan Post, <-chan error) {
	out := make(chan Post)
	errCh := make(chan error, 1)

	go func() {
		defer close(out)
		offset := ""
		for {
			params := url.Values{}
			params.Set("limit", fmt.Sprintf("%d", postsPageLimit))
			params.Set("reply_limit", "0")
			params.Set("comments_limit", "0")
			if offset != "" {
				params.Set("offset", offset)
			}

			data, err := c.get(ctx, fmt.Sprintf("%s/v1/blog/%s/post/", apiBase, author), params)
			if err != nil {
				errCh <- err
				return
			}

			if rawPosts, ok := data["data"].([]any); ok {
				for _, rp := range rawPosts {
					if m, ok := rp.(map[string]any); ok {
						select {
						case out <- parsePost(m):
						case <-ctx.Done():
							return
						}
					}
				}
			}

			extra, _ := data["extra"].(map[string]any)
			isLast, _ := extra["isLast"].(bool)
			nextOffset, _ := extra["offset"].(string)
			if isLast || nextOffset == "" {
				return
			}
			offset = nextOffset

			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}()

	return out, errCh
}

func parsePost(raw map[string]any) Post {
	post := Post{
		ID:          asString(raw["id"]),
		HasAccess:   raw["hasAccess"] == true,
		Title:       asString(raw["title"]),
		SignedQuery: asString(raw["signedQuery"]),
	}
	if intID, ok := raw["intId"].(float64); ok {
		post.IntID = int64(intID)
	}
	if pt, ok := raw["publishTime"].(float64); ok {
		post.PublishTime = int64(pt)
	}

	seen := map[string]bool{}
	blocks := append(asMapSlice(raw["data"]), asMapSlice(raw["media"])...)
	for _, block := range blocks {
		bid := asString(block["id"])
		if bid != "" && seen[bid] {
			continue
		}
		t := asString(block["type"])
		switch t {
		case "text", "header", "link", "list":
			post.TextBlocks = append(post.TextBlocks, block)
		default:
			if m := parseMedia(block); m != nil {
				post.Media = append(post.Media, *m)
				if bid != "" {
					seen[bid] = true
				}
			}
		}
	}
	return post
}

func parseMedia(b map[string]any) *MediaItem {
	if b["isTeaser"] == true || b["is_teaser"] == true {
		return nil
	}
	t := asString(b["type"])
	id := asString(b["id"])

	switch t {
	case "image":
		if _, hasWidth := b["width"]; hasWidth {
			return &MediaItem{Kind: MediaImage, ID: id, URL: asString(b["url"])}
		}
		return nil

	case "ok_video", "video":
		urls := map[string]string{}
		playerURLs := b["playerUrls"]
		if playerURLs == nil {
			playerURLs = b["player_urls"]
		}
		for _, u := range asMapSlice(playerURLs) {
			ut := asString(u["type"])
			uu := asString(u["url"])
			if uu == "" {
				continue
			}
			for _, q := range videoQualities {
				if ut == q {
					urls[ut] = uu
				}
			}
		}
		title := asString(b["title"])
		if title == "" {
			title = id
		}
		return &MediaItem{Kind: MediaVideo, ID: id, Title: title, URLs: urls}

	case "audio_file":
		title := asString(b["title"])
		if title == "" {
			title = id
		}
		return &MediaItem{Kind: MediaAudio, ID: id, URL: asString(b["url"]), Title: title}

	case "file":
		title := asString(b["title"])
		if title == "" {
			title = id
		}
		return &MediaItem{Kind: MediaFile, ID: id, URL: asString(b["url"]), Title: title}

	default:
		// external_video (YouTube/Vimeo) и неизвестные типы — пропускаем
		return nil
	}
}
