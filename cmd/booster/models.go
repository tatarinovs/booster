package main

// MediaKind различает типы медиа-вложений поста.
type MediaKind int

const (
	MediaImage MediaKind = iota
	MediaVideo
	MediaAudio
	MediaFile
)

// MediaItem — единое представление вложения поста (аналог MediaItem union в Python).
type MediaItem struct {
	Kind  MediaKind
	ID    string
	URL   string            // для Image/Audio/File
	Title string            // для Video/Audio/File
	URLs  map[string]string // для Video: качество → url
}

var videoQualities = []string{"ultra_hd", "full_hd", "high", "medium", "low"}

// bestURL возвращает лучший доступный по качеству URL видео.
func (m *MediaItem) bestURL() string {
	for _, q := range videoQualities {
		if u, ok := m.URLs[q]; ok && u != "" {
			return u
		}
	}
	return ""
}

// Post — распарсенный пост блога.
type Post struct {
	ID          string
	IntID       int64
	HasAccess   bool
	PublishTime int64
	Title       string
	SignedQuery string
	TextBlocks  []map[string]any
	Media       []MediaItem
}
