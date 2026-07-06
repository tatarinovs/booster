package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// blockText декодирует поле content блока (JSON-массив, первый элемент — текст).
func blockText(contentJSON string) string {
	if contentJSON == "" {
		return ""
	}
	var data []any
	if err := json.Unmarshal([]byte(contentJSON), &data); err != nil || len(data) == 0 {
		return ""
	}
	if s, ok := data[0].(string); ok {
		return s
	}
	return ""
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asMapSlice(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// postToText рендерит текстовые блоки поста в обычный текст.
func postToText(blocks []map[string]any) string {
	var parts []string

	var renderList func(items []map[string]any, indent int)
	renderList = func(items []map[string]any, indent int) {
		for _, item := range items {
			var sb strings.Builder
			for _, d := range asMapSlice(item["data"]) {
				sb.WriteString(blockText(asString(d["content"])))
			}
			parts = append(parts, strings.Repeat("  ", indent)+"- "+sb.String()+"\n")
			if sub := asMapSlice(item["items"]); len(sub) > 0 {
				renderList(sub, indent+1)
			}
		}
	}

	for _, block := range blocks {
		t := asString(block["type"])
		switch t {
		case "link":
			parts = append(parts, fmt.Sprintf("%s (ссылка: %s)\n",
				blockText(asString(block["content"])), asString(block["url"])))
		case "text", "header":
			content := blockText(asString(block["content"]))
			if content != "" {
				parts = append(parts, content)
			}
			if asString(block["modificator"]) == "BLOCK_END" || content != "" {
				parts = append(parts, "\n")
			}
		case "list":
			renderList(asMapSlice(block["items"]), 0)
		}
	}

	return strings.TrimSpace(strings.Join(parts, ""))
}
