package suno

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testUUID = "00000000-0000-0000-0000-000000000001"

func TestClientPlaylistReturnsCompleteClips(t *testing.T) {
	var sawUserAgent bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/playlist/"+testUUID {
			t.Fatalf("path = %q", r.URL.Path)
		}
		sawUserAgent = strings.Contains(r.Header.Get("user-agent"), "Chrome/149")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"playlist_clips": [
				{"clip": {"id":"clip-a","status":"complete","title":"A","handle":"Artist","image_url":"http://example.test/a.jpg","audio_url":"http://example.test/a.mp3"}},
				{"clip": {"id":"clip-b","status":"submitted","title":"B","handle":"Artist","image_url":"http://example.test/b.jpg","audio_url":"http://example.test/b.mp3"}}
			]
		}`))
	}))
	defer srv.Close()

	clips, seen, err := NewClient(srv.URL, srv.Client()).Playlist(context.Background(), testUUID)
	if err != nil {
		t.Fatal(err)
	}
	if seen != 2 {
		t.Fatalf("seen = %d", seen)
	}
	if len(clips) != 1 || clips[0].ID != "clip-a" || clips[0].Title != "A" || clips[0].Handle != "Artist" {
		t.Fatalf("clips = %+v", clips)
	}
	if !sawUserAgent {
		t.Fatal("missing expected user agent")
	}
}

func TestSyncRadioDownloadsClipAssetsAndManifest(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/playlist/" + testUUID:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"playlist_clips": [
					{"clip": {"id":"clip-a","status":"complete","title":"Song A","handle":"Artist A","image_url":"/cover/clip-a","audio_url":"/audio/clip-a.mp3"}}
				]
			}`))
		case "/cover/clip-a":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("png"))
		case "/audio/clip-a.mp3":
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write([]byte("mp3"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, rewriteClient(srv.Client(), srv.URL))
	result, err := NewSyncer(client, nil).SyncRadio(context.Background(), Radio{
		Alias:    "monthly",
		UUID:     testUUID,
		InboxDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Seen != 1 || result.Complete != 1 || result.Downloaded != 2 || result.Skipped != 0 || result.Errors != 0 {
		t.Fatalf("result = %+v", result)
	}
	assertFile(t, filepath.Join(dir, "clip-a.mp3"), "mp3")
	assertFile(t, filepath.Join(dir, "clip-a.png"), "png")
	sidecar, err := os.ReadFile(filepath.Join(dir, "clip-a.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sidecar), `"title": "Song A"`) || !strings.Contains(string(sidecar), `"artist": "Artist A"`) {
		t.Fatalf("sidecar = %s", sidecar)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, manifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), "clip-a.mp3") || !strings.Contains(string(manifest), "clip-a.png") {
		t.Fatalf("manifest = %s", manifest)
	}
}

func TestSyncRadioSkipsExistingMP3AndDeletesStaleManifestFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "clip-a.mp3"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stale.mp3"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manual.mp3"), []byte("manual"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveManifest(filepath.Join(dir, manifestFile), Manifest{Files: map[string][]string{
		"stale": {"stale.mp3"},
	}}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/playlist/" + testUUID:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"playlist_clips": [
					{"clip": {"id":"clip-a","status":"complete","title":"Song A","handle":"Artist A","audio_url":"/audio/clip-a.mp3"}}
				]
			}`))
		case "/audio/clip-a.mp3":
			_, _ = w.Write([]byte("new"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, rewriteClient(srv.Client(), srv.URL))
	result, err := NewSyncer(client, nil).SyncRadio(context.Background(), Radio{
		Alias:    "monthly",
		UUID:     testUUID,
		InboxDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Skipped != 1 || result.Deleted != 1 || result.Downloaded != 0 {
		t.Fatalf("result = %+v", result)
	}
	assertFile(t, filepath.Join(dir, "clip-a.mp3"), "old")
	if _, err := os.Stat(filepath.Join(dir, "stale.mp3")); !os.IsNotExist(err) {
		t.Fatalf("stale exists err=%v", err)
	}
	assertFile(t, filepath.Join(dir, "manual.mp3"), "manual")
}

func rewriteClient(client *http.Client, baseURL string) *http.Client {
	client.Transport = rewriteTransport{baseURL: baseURL, base: http.DefaultTransport}
	return client
}

type rewriteTransport struct {
	baseURL string
	base    http.RoundTripper
}

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "" {
		base := strings.TrimRight(t.baseURL, "/")
		req.URL.Scheme = strings.SplitN(base, "://", 2)[0]
		req.URL.Host = strings.TrimPrefix(base, req.URL.Scheme+"://")
	}
	return t.base.RoundTrip(req)
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
