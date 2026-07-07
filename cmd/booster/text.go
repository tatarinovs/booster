package main

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// multiNewlineRe схлопывает три и более последовательных переноса строк в два.
var multiNewlineRe = regexp.MustCompile(`\n{3,}`)

// blockTextAndStyles извлекает текст, тип блока и стили.
func blockTextAndStyles(contentJSON string) (text string, blockType string, styles []any) {
	if contentJSON == "" {
		return "", "unstyled", nil
	}
	var data []any
	if err := json.Unmarshal([]byte(contentJSON), &data); err != nil || len(data) == 0 {
		return "", "unstyled", nil
	}
	text, _ = data[0].(string)
	if len(data) > 1 {
		blockType, _ = data[1].(string)
	}
	if len(data) > 2 {
		if s, ok := data[2].([]any); ok {
			styles = s
		}
	}
	return text, blockType, styles
}

// applyStyles применяет жирный, курсив и подчеркивание к тексту.
// Работает за O(n + m·log m): однопроходная сборка через strings.Builder.
func applyStyles(text string, styles []any) string {
	if len(styles) == 0 {
		return text
	}

	// 0: BOLD (**), 2: ITALIC (*), 4: UNDERLINE (__)
	styleMap := map[int]string{0: "**", 2: "*", 4: "__"}

	type point struct {
		pos int
		tag string
	}
	var points []point

	runes := []rune(text)
	textLen := len(runes)

	for _, s := range styles {
		arr, ok := s.([]any)
		if !ok || len(arr) != 3 {
			continue
		}
		styleID := int(arr[0].(float64))
		offset := int(arr[1].(float64))
		length := int(arr[2].(float64))
		if offset > textLen {
			continue
		}
		tag, exists := styleMap[styleID]
		if !exists {
			continue
		}
		endPos := offset + length
		if endPos > textLen {
			endPos = textLen
		}
		points = append(points, point{offset, tag}, point{endPos, tag})
	}

	if len(points) == 0 {
		return text
	}

	// Сортировка по возрастанию для однопроходной вставки слева направо.
	sort.SliceStable(points, func(i, j int) bool {
		return points[i].pos < points[j].pos
	})

	var sb strings.Builder
	sb.Grow(len(text) + len(points)*4)

	lastPos := 0
	for _, p := range points {
		if p.pos > lastPos {
			sb.WriteString(string(runes[lastPos:p.pos]))
		}
		sb.WriteString(p.tag)
		lastPos = p.pos
	}
	if lastPos < textLen {
		sb.WriteString(string(runes[lastPos:]))
	}

	return sb.String()
}

func getPrefix(blockType string) string {
	switch blockType {
	case "header", "header-one":
		return "# "
	case "header-two":
		return "## "
	case "header-three":
		return "### "
	case "blockquote":
		return "> "
	case "unordered-list-item":
		return "* "
	case "ordered-list-item":
		return "1. "
	default:
		return ""
	}
}

// postToMarkdown рендерит текстовые блоки поста в Markdown.
func postToMarkdown(blocks []map[string]any) string {
	var parts []string

	var renderList func(items []map[string]any, level int)
	renderList = func(items []map[string]any, level int) {
		indent := strings.Repeat("  ", level)
		for _, item := range items {
			var sb strings.Builder
			for _, d := range asMapSlice(item["data"]) {
				text, _, styles := blockTextAndStyles(asString(d["content"]))
				sb.WriteString(applyStyles(text, styles))
			}
			parts = append(parts, indent+"* "+sb.String()+"\n")
			if sub := asMapSlice(item["items"]); len(sub) > 0 {
				renderList(sub, level+1)
			}
		}
	}

	for _, block := range blocks {
		t := asString(block["type"])
		switch t {
		case "link":
			text, _, styles := blockTextAndStyles(asString(block["content"]))
			formatted := applyStyles(text, styles)
			parts = append(parts, "["+formatted+"]("+asString(block["url"])+") ")
		case "text", "header":
			text, bType, styles := blockTextAndStyles(asString(block["content"]))
			formatted := applyStyles(text, styles)
			prefix := getPrefix(bType)

			if formatted != "" || prefix != "" {
				parts = append(parts, prefix+formatted)
			}
			if asString(block["modificator"]) == "BLOCK_END" || formatted != "" || prefix != "" {
				parts = append(parts, "\n\n")
			}
		case "list":
			renderList(asMapSlice(block["items"]), 0)
			parts = append(parts, "\n")
		}
	}

	result := multiNewlineRe.ReplaceAllString(strings.Join(parts, ""), "\n\n")
	return strings.TrimSpace(result)
}
