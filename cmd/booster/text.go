package main

import (
	"encoding/json"
	"regexp"
	"slices"
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

// 0: BOLD (**), 2: ITALIC (*), 4: UNDERLINE (__)
var styleTags = map[int]string{0: "**", 2: "*", 4: "__"}

// styleOrder задаёт детерминированный порядок вложения тегов.
var styleOrder = []int{0, 2, 4}

// applyStyles применяет жирный, курсив и подчёркивание к тексту.
//
// Диапазоны стилей сначала раскладываются в набор активных стилей на каждой руне,
// а затем один проход открывает и закрывает теги по стеку. За счёт этого
// пересекающиеся и дублирующие друг друга диапазоны одного стиля схлопываются
// в одну пару тегов, а вложенность не ломается.
//
// Данные приходят из внешнего API, поэтому все поля извлекаются через asFloat:
// раньше здесь стояли непроверенные приведения типов, и любое нечисловое
// значение роняло весь процесс паникой.
func applyStyles(text string, styles []any) string {
	if len(styles) == 0 {
		return text
	}

	runes := []rune(text)
	textLen := len(runes)
	if textLen == 0 {
		return text
	}

	// active[i] — множество стилей, действующих на руне i.
	active := make([]map[int]bool, textLen)
	matched := false

	for _, s := range styles {
		arr, ok := s.([]any)
		if !ok || len(arr) != 3 {
			continue
		}
		idF, okID := asFloat(arr[0])
		offF, okOff := asFloat(arr[1])
		lenF, okLen := asFloat(arr[2])
		if !okID || !okOff || !okLen {
			continue
		}
		styleID, offset, length := int(idF), int(offF), int(lenF)
		if _, known := styleTags[styleID]; !known {
			continue
		}
		// Отрицательное смещение раньше приводило к выходу за границы среза.
		if offset < 0 {
			length += offset
			offset = 0
		}
		if length <= 0 || offset >= textLen {
			continue
		}
		end := offset + length
		if end > textLen {
			end = textLen
		}
		for i := offset; i < end; i++ {
			if active[i] == nil {
				active[i] = make(map[int]bool, 2)
			}
			active[i][styleID] = true
		}
		matched = true
	}

	if !matched {
		return text
	}

	var sb strings.Builder
	sb.Grow(len(text) + 16)

	var open []int // стек открытых стилей
	for i := 0; i <= textLen; i++ {
		var cur map[int]bool
		if i < textLen {
			cur = active[i]
		}

		// Раскручиваем стек до самого глубокого стиля, который больше не
		// действует; всё, что лежало выше него, переоткроется ниже.
		cut := -1
		for idx, id := range open {
			if !cur[id] {
				cut = idx
				break
			}
		}
		if cut >= 0 {
			for j := len(open) - 1; j >= cut; j-- {
				sb.WriteString(styleTags[open[j]])
			}
			open = open[:cut]
		}

		for _, id := range styleOrder {
			if !cur[id] || slices.Contains(open, id) {
				continue
			}
			sb.WriteString(styleTags[id])
			open = append(open, id)
		}

		if i < textLen {
			sb.WriteRune(runes[i])
		}
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
