package main

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

func messageID(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if n, ok := v.(float64); ok {
		return strconv.FormatFloat(n, 'f', 0, 64)
	}
	return ""
}

// findDialog uses GET only: exporting a conversation must not create it or mark it read.
func (c *BoostyClient) findDialog(ctx context.Context, author string) (string, string, error) {
	if c.headers["Authorization"] == "" {
		return "", "", fmt.Errorf("для личной переписки требуется авторизация")
	}
	blog, err := c.get(ctx, apiBase+"/v1/blog/"+url.PathEscape(author), nil)
	if err != nil {
		return "", "", err
	}
	owner, _ := blog["owner"].(map[string]any)
	id := messageID(owner["id"])
	if id == "" {
		return "", "", fmt.Errorf("API не вернул ID автора")
	}
	dialog, err := c.get(ctx, apiBase+"/v1/dialog", url.Values{"user_id": {id}})
	if err != nil {
		return "", "", err
	}
	id = messageID(dialog["id"])
	if id == "" {
		return "", "", fmt.Errorf("API не вернул ID диалога")
	}
	return id, asString(dialog["signedQuery"]), nil
}

func parseMessage(raw map[string]any, signature string) Post {
	// Message IDs are numeric; posts use strings. Keep the shared media pipeline.
	normalized := make(map[string]any, len(raw)+4)
	for k, v := range raw {
		normalized[k] = v
	}
	normalized["id"] = messageID(raw["id"])
	normalized["title"] = "message_" + messageID(raw["id"])
	normalized["publishTime"] = raw["createdAt"]
	normalized["hasAccess"] = raw["payWall"] != true && raw["isDeleted"] != true
	normalized["signedQuery"] = signature
	post := parsePost(normalized)
	for i := range post.Media {
		if post.Media[i].Kind == MediaImage {
			post.Media[i].URL = signURL(post.Media[i].URL, signature)
		}
	}
	return post
}

func (c *BoostyClient) iterMessages(ctx context.Context, dialogID, signature string) (<-chan Post, <-chan error) {
	out := make(chan Post)
	errs := make(chan error, 1)
	go func() {
		defer close(out)
		// reverse=true walks newest to oldest, as required by the sync checkpoint.
		offset := "9223372036854775807"
		seen := map[string]bool{}
		for {
			data, err := c.get(ctx, apiBase+"/v1/dialog/"+url.PathEscape(dialogID)+"/message/", url.Values{
				"limit": {"20"}, "reverse": {"true"}, "offset": {offset},
			})
			if err != nil {
				errs <- err
				return
			}
			rows, ok := data["data"].([]any)
			if !ok {
				errs <- fmt.Errorf("API не вернул список сообщений")
				return
			}
			for _, raw := range rows {
				m, ok := raw.(map[string]any)
				if !ok || messageID(m["id"]) == "" {
					errs <- fmt.Errorf("неверный формат сообщения API")
					return
				}
				id := messageID(m["id"])
				if seen[id] {
					continue
				}
				seen[id] = true
				select {
				case out <- parseMessage(m, signature):
				case <-ctx.Done():
					return
				}
			}
			extra, _ := data["extra"].(map[string]any)
			if extra["isLast"] == true {
				return
			}
			next := messageID(extra["offset"])
			if len(rows) == 0 || next == "" || next == offset {
				errs <- fmt.Errorf("API не продвинул страницу сообщений")
				return
			}
			offset = next
		}
	}()
	return out, errs
}

func (c *BoostyClient) refreshMessageMediaURL(ctx context.Context, dialogID, id, mediaID string) (string, error) {
	dialog, err := c.get(ctx, apiBase+"/v1/dialog/"+url.PathEscape(dialogID), nil)
	if err != nil {
		return "", err
	}
	raw, err := c.get(ctx, apiBase+"/v1/dialog/message/"+url.PathEscape(id), nil)
	if err != nil {
		return "", err
	}
	// Never resolve a message returned from another conversation.
	if messageID(raw["dialogId"]) != dialogID {
		return "", fmt.Errorf("сообщение принадлежит другому диалогу")
	}
	post := parseMessage(raw, asString(dialog["signedQuery"]))
	if !post.HasAccess {
		return "", fmt.Errorf("нет доступа к сообщению %s", id)
	}
	for i := range post.Media {
		if post.Media[i].ID == mediaID {
			if u := mediaURL(&post, &post.Media[i]); u != "" {
				return u, nil
			}
		}
	}
	return "", fmt.Errorf("вложение %s не найдено в сообщении %s", mediaID, id)
}
