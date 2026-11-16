package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSavePostText(t *testing.T) {
	path := filepath.Join(t.TempDir(), contentFilename)
	post := Post{TextBlocks: []map[string]any{
		{"type": "text", "content": `["hello","unstyled",[]]`},
	}}
	if err := savePostText(path, &post); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "hello") {
		t.Fatalf("text was not saved: %q, %v", data, err)
	}
	if _, err := os.Stat(path + ".part"); !os.IsNotExist(err) {
		t.Fatalf("temporary file remains: %v", err)
	}
	post.TextBlocks = nil
	if err := savePostText(path, &post); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(data) {
		t.Fatalf("existing text changed: %q, %v", got, err)
	}
}

func TestMissingFileOperations(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	if err := safeUnlink(missing); err != nil {
		t.Fatalf("removing missing file: %v", err)
	}
	dest := filepath.Join(dir, "existing")
	if err := os.WriteFile(dest, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := safeReplace(missing, dest); !os.IsNotExist(err) {
		t.Fatalf("renaming missing source must fail, got %v", err)
	}
	if data, err := os.ReadFile(dest); err != nil || string(data) != "keep" {
		t.Fatalf("destination changed: %q, %v", data, err)
	}
}

func TestRunTextFailureKeepsSyncMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/blog/author":
			fmt.Fprint(w, `{"count":{"posts":1}}`)
		case "/v1/blog/author/post/":
			fmt.Fprint(w, `{"data":[{"id":"new","title":"post","hasAccess":true,"data":[{"type":"text","content":"[\"hello\",\"unstyled\",[]]"}]}],"extra":{"isLast":true}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	withAPIBase(t, srv.URL)

	for _, blocked := range []string{"post-directory", "text-file", "temporary-file"} {
		t.Run(blocked, func(t *testing.T) {
			dir := t.TempDir()
			authorDir := filepath.Join(dir, "author")
			postDir := postDirName(authorDir, &Post{ID: "new", Title: "post"})
			if err := os.MkdirAll(authorDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if blocked == "post-directory" {
				if err := os.WriteFile(postDir, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			} else {
				path := filepath.Join(postDir, contentFilename)
				if blocked == "temporary-file" {
					path += ".part"
				}
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			latest := filepath.Join(authorDir, ".latest_post")
			if err := os.WriteFile(latest, []byte("old"), 0o644); err != nil {
				t.Fatal(err)
			}
			stats := newStats()
			if err := run(context.Background(), runOptions{
				author: "author", outputDir: dir, workers: 1, noGalleries: true,
				cancel: &atomic.Bool{}, abort: &atomic.Bool{}, stats: stats,
			}); err != nil {
				t.Fatal(err)
			}
			if got := exitCode(false, false, stats); got != 2 {
				t.Fatalf("exit code = %d, want 2", got)
			}
			if data, err := os.ReadFile(latest); err != nil || string(data) != "old" {
				t.Fatalf("checkpoint advanced: %q, %v", data, err)
			}
			for _, name := range []string{".syncing", failedFilename} {
				if _, err := os.Stat(filepath.Join(authorDir, name)); err != nil {
					t.Fatalf("missing %s: %v", name, err)
				}
			}
		})
	}
}
