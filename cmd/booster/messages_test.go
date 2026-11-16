package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestPostsAndMessagesSync(t *testing.T) {
	var downloads atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected mutation: %s", r.Method)
		}
		switch r.URL.Path {
		case "/v1/blog/author":
			fmt.Fprint(w, `{"owner":{"id":42},"count":{"posts":0}}`)
		case "/v1/blog/author/post/":
			fmt.Fprint(w, `{"data":[],"extra":{"isLast":true}}`)
		case "/v1/dialog":
			if r.URL.Query().Get("user_id") != "42" {
				t.Error("missing owner ID")
			}
			fmt.Fprint(w, `{"id":9,"signedQuery":"?sign=ok"}`)
		case "/v1/dialog/9/message/":
			if r.URL.Query().Get("reverse") != "true" {
				t.Error("history must descend")
			}
			if r.URL.Query().Get("offset") == "9223372036854775807" {
				fmt.Fprintf(w, `{"data":[{"id":21,"createdAt":1700000000,"data":[{"type":"text","content":"Text without media"}]},{"id":20,"dialogId":9,"createdAt":1700000000,"payWall":false,"data":[{"type":"image","width":10,"id":"photo","url":%q}]},{"id":19,"payWall":true,"teaser":[{"type":"image","width":10,"id":"teaser","url":"/never"}]}],"extra":{"offset":19,"isLast":false}}`, srv.URL+"/photo.jpg")
			} else {
				fmt.Fprint(w, `{"data":[{"id":18,"createdAt":1700000000,"data":[]}],"extra":{"isLast":true}}`)
			}
		case "/photo.jpg":
			if r.URL.Query().Get("sign") != "ok" {
				t.Error("missing message signature")
			}
			downloads.Add(1)
			fmt.Fprint(w, "image bytes")
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	withAPIBase(t, srv.URL)
	var cancel, abort atomic.Bool
	dir := t.TempDir()
	opts := runOptions{author: "author", token: &AuthToken{Authorization: "test"}, outputDir: dir, workers: 1, noGalleries: true, messages: true, cancel: &cancel, abort: &abort, stats: newStats()}
	for i := 0; i < 2; i++ {
		if err := run(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
	}
	if downloads.Load() != 1 {
		t.Fatalf("downloads = %d", downloads.Load())
	}
	checkpoint, err := os.ReadFile(filepath.Join(dir, "author", "messages", ".latest_message"))
	if err != nil || string(checkpoint) != "21" {
		t.Fatalf("checkpoint=%q err=%v", checkpoint, err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "author", "messages", "*", "*.jpg"))
	if err != nil || len(files) != 1 {
		t.Fatalf("media files=%v err=%v", files, err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "author", "messages"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != "2023-11-14_message_20_20" {
			t.Errorf("unexpected message directory: %s", entry.Name())
		}
	}
}

func TestMessagePaginationFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":20,"data":[]}],"extra":{"offset":"9223372036854775807","isLast":false}}`)
	}))
	defer srv.Close()
	withAPIBase(t, srv.URL)
	posts, errs := newBoostyClient(nil).iterMessages(context.Background(), "9", "")
	for range posts {
	}
	select {
	case err := <-errs:
		if err == nil {
			t.Fatal("missing pagination error")
		}
	default:
		t.Fatal("missing pagination error")
	}
}

func TestRefreshMessageMediaURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/dialog/9":
			fmt.Fprint(w, `{"signedQuery":"?sign=fresh"}`)
		case "/v1/dialog/message/20":
			fmt.Fprint(w, `{"id":20,"dialogId":9,"data":[{"id":"audio","type":"audio_file","url":"https://cdn.example/audio.mp3"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	withAPIBase(t, srv.URL)
	got, err := newBoostyClient(nil).refreshMessageMediaURL(context.Background(), "9", "20", "audio")
	if err != nil || got != "https://cdn.example/audio.mp3?sign=fresh" {
		t.Fatalf("url=%q err=%v", got, err)
	}
}
