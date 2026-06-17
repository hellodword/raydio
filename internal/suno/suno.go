package suno

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"
)

const (
	DefaultBaseURL     = "https://studio-api-prod.suno.com"
	manifestFile       = ".suno-manifest.json"
	defaultUserAgent   = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"
	defaultContentType = "application/json"
)

type Client struct {
	baseURL    string
	httpClient *http.Client
}

type Clip struct {
	ID       string
	Title    string
	Handle   string
	ImageURL string
	AudioURL string
}

type Radio struct {
	Alias    string
	UUID     string
	InboxDir string
}

type Syncer struct {
	client         *Client
	logger         *slog.Logger
	maxConcurrency int
}

type Result struct {
	Seen       int
	Complete   int
	Downloaded int
	Skipped    int
	Deleted    int
	Errors     int
}

type Manifest struct {
	Files map[string][]string `json:"files"`
}

func NewClient(baseURL string, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), httpClient: httpClient}
}

func (c *Client) Playlist(ctx context.Context, uuid string) ([]Clip, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/playlist/"+uuid, nil)
	if err != nil {
		return nil, 0, err
	}
	setPlaylistHeaders(req)
	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, 0, fmt.Errorf("suno playlist %s returned %s", uuid, res.Status)
	}
	var payload playlistResponse
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		return nil, 0, err
	}
	clips := make([]Clip, 0, len(payload.PlaylistClips))
	for _, item := range payload.PlaylistClips {
		clip := item.Clip
		if clip.Status != "complete" {
			continue
		}
		clips = append(clips, Clip{
			ID:       strings.TrimSpace(clip.ID),
			Title:    strings.TrimSpace(clip.Title),
			Handle:   strings.TrimSpace(clip.Handle),
			ImageURL: strings.TrimSpace(clip.ImageURL),
			AudioURL: strings.TrimSpace(clip.AudioURL),
		})
	}
	return clips, len(payload.PlaylistClips), nil
}

func NewSyncer(client *Client, logger *slog.Logger) *Syncer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Syncer{client: client, logger: logger, maxConcurrency: 4}
}

func (s *Syncer) SyncRadio(ctx context.Context, radio Radio) (Result, error) {
	var result Result
	if err := os.MkdirAll(radio.InboxDir, 0o755); err != nil {
		return result, err
	}
	clips, seen, err := s.client.Playlist(ctx, radio.UUID)
	if err != nil {
		return result, err
	}
	result.Seen = seen
	result.Complete = len(clips)

	oldManifest, err := loadManifest(filepath.Join(radio.InboxDir, manifestFile))
	if err != nil {
		return result, err
	}
	nextManifest := Manifest{Files: map[string][]string{}}
	active := map[string]struct{}{}
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(s.maxConcurrency)
	for _, clip := range clips {
		clip := clip
		key := clipKey(clip)
		active[key] = struct{}{}
		g.Go(func() error {
			files, downloaded, skipped, err := s.syncClip(gctx, radio.InboxDir, key, clip)
			mu.Lock()
			defer mu.Unlock()
			result.Downloaded += downloaded
			result.Skipped += skipped
			if err != nil {
				result.Errors++
				s.logger.Warn("suno clip sync failed", "radio", radio.Alias, "uuid", radio.UUID, "clip", key, "error", err)
				if oldFiles := oldManifest.Files[key]; len(oldFiles) > 0 {
					nextManifest.Files[key] = oldFiles
				}
				if gctx.Err() != nil {
					return gctx.Err()
				}
				return nil
			}
			nextManifest.Files[key] = files
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return result, err
	}
	deleted, err := deleteStale(radio.InboxDir, oldManifest, active)
	result.Deleted = deleted
	if err != nil {
		result.Errors++
		s.logger.Warn("suno stale delete failed", "radio", radio.Alias, "uuid", radio.UUID, "error", err)
	}
	if err := saveManifest(filepath.Join(radio.InboxDir, manifestFile), nextManifest); err != nil {
		return result, err
	}
	return result, nil
}

func (s *Syncer) syncClip(ctx context.Context, dir, key string, clip Clip) ([]string, int, int, error) {
	if clip.AudioURL == "" {
		return nil, 0, 0, errors.New("complete clip has no audio_url")
	}
	files := []string{}
	downloaded := 0
	skipped := 0
	if clip.ImageURL != "" {
		coverExt, wrote, err := downloadWithExt(ctx, s.client.httpClient, s.client.baseURL, clip.ImageURL, filepath.Join(dir, key), true)
		if err != nil {
			return files, downloaded, skipped, err
		}
		if coverExt != "" {
			files = append(files, key+coverExt)
			if wrote {
				downloaded++
			} else {
				skipped++
			}
		}
	}
	wrote, err := downloadFile(ctx, s.client.httpClient, s.client.baseURL, clip.AudioURL, filepath.Join(dir, key+".mp3"), false)
	files = append(files, key+".mp3")
	if err != nil {
		return files, downloaded, skipped, err
	}
	if wrote {
		downloaded++
	} else {
		skipped++
	}
	return files, downloaded, skipped, nil
}

func setPlaylistHeaders(req *http.Request) {
	req.Header.Set("accept", "*/*")
	req.Header.Set("accept-language", "en-US,en;q=0.9")
	req.Header.Set("content-type", defaultContentType)
	req.Header.Set("origin", "https://suno.com")
	req.Header.Set("priority", "u=1, i")
	req.Header.Set("referer", "https://suno.com/")
	req.Header.Set("sec-ch-ua", `"Google Chrome";v="149", "Chromium";v="149", "Not)A;Brand";v="24"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Linux"`)
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-site", "same-site")
	req.Header.Set("user-agent", defaultUserAgent)
}

type playlistResponse struct {
	PlaylistClips []struct {
		Clip struct {
			ID       string `json:"id"`
			Status   string `json:"status"`
			Title    string `json:"title"`
			Handle   string `json:"handle"`
			ImageURL string `json:"image_url"`
			AudioURL string `json:"audio_url"`
		} `json:"clip"`
	} `json:"playlist_clips"`
}

func clipKey(clip Clip) string {
	if clip.ID != "" {
		return safeName(clip.ID)
	}
	sum := sha256.Sum256([]byte(clip.AudioURL))
	return hex.EncodeToString(sum[:])
}

func safeName(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "clip"
	}
	return out
}

func downloadWithExt(ctx context.Context, client *http.Client, baseURL, rawURL, base string, skipExisting bool) (string, bool, error) {
	ext := extFromURL(rawURL)
	resolved, err := resolveURL(baseURL, rawURL)
	if err != nil {
		return "", false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resolved, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("user-agent", defaultUserAgent)
	res, err := client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", false, fmt.Errorf("download %s returned %s", resolved, res.Status)
	}
	if byType := extFromContentType(res.Header.Get("Content-Type")); byType != "" {
		ext = byType
	}
	if ext == "" {
		ext = ".jpg"
	}
	wrote, err := writeResponseBody(res.Body, base+ext, skipExisting)
	return ext, wrote, err
}

func downloadFile(ctx context.Context, client *http.Client, baseURL, rawURL, path string, overwrite bool) (bool, error) {
	if !overwrite && existingNonEmpty(path) {
		return false, nil
	}
	resolved, err := resolveURL(baseURL, rawURL)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resolved, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("user-agent", defaultUserAgent)
	res, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return false, fmt.Errorf("download %s returned %s", resolved, res.Status)
	}
	return writeResponseBody(res.Body, path, !overwrite)
}

func writeResponseBody(r io.Reader, path string, skipExisting bool) (bool, error) {
	if skipExisting && existingNonEmpty(path) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp := path + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return false, err
	}
	_, copyErr := io.Copy(out, r)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return false, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return false, closeErr
	}
	return true, os.Rename(tmp, path)
}

func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func existingNonEmpty(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Size() > 0
}

func extFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	ext := strings.ToLower(filepath.Ext(u.Path))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp":
		return ext
	default:
		return ""
	}
}

func resolveURL(baseURL, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.IsAbs() {
		return u.String(), nil
	}
	base, err := url.Parse(strings.TrimRight(baseURL, "/") + "/")
	if err != nil {
		return "", err
	}
	return base.ResolveReference(u).String(), nil
}

func extFromContentType(contentType string) string {
	typ, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	switch typ {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	default:
		return ""
	}
}

func loadManifest(path string) (Manifest, error) {
	out := Manifest{Files: map[string][]string{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return Manifest{Files: map[string][]string{}}, err
	}
	if out.Files == nil {
		out.Files = map[string][]string{}
	}
	return out, nil
}

func saveManifest(path string, manifest Manifest) error {
	if manifest.Files == nil {
		manifest.Files = map[string][]string{}
	}
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return writeFileAtomic(path, b, 0o644)
}

func deleteStale(dir string, manifest Manifest, active map[string]struct{}) (int, error) {
	deleted := 0
	for key, files := range manifest.Files {
		if _, ok := active[key]; ok {
			continue
		}
		for _, name := range files {
			if name == "" || filepath.IsAbs(name) || strings.Contains(name, "..") {
				continue
			}
			err := os.Remove(filepath.Join(dir, name))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return deleted, err
			}
			deleted++
		}
	}
	return deleted, nil
}
