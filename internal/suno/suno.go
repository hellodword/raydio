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
	"time"

	"golang.org/x/sync/errgroup"
)

const (
	DefaultBaseURL     = "https://studio-api-prod.suno.com"
	manifestFile       = ".suno-manifest.json"
	defaultUserAgent   = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"
	defaultContentType = "application/json"
)

type Client struct {
	baseURL       string
	httpClient    *http.Client
	playlistSlots chan struct{}
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
	maxAudioBytes  int64
	maxCoverBytes  int64
	retries        int
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
	Clips map[string]ManifestClip `json:"clips"`
}

type ManifestClip struct {
	ID       string `json:"id"`
	Audio    string `json:"audio"`
	Cover    string `json:"cover,omitempty"`
	Metadata string `json:"metadata"`
}

type ClipMetadata struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Artist string `json:"artist"`
}

func NewClient(baseURL string, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), httpClient: httpClient, playlistSlots: make(chan struct{}, 1)}
}

func (c *Client) Playlist(ctx context.Context, uuid string) ([]Clip, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/playlist/"+uuid, nil)
	if err != nil {
		return nil, 0, err
	}
	setPlaylistHeaders(req)
	release, err := c.acquirePlaylist(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer release()
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

func (c *Client) acquirePlaylist(ctx context.Context) (func(), error) {
	select {
	case c.playlistSlots <- struct{}{}:
		return func() { <-c.playlistSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func NewSyncer(client *Client, logger *slog.Logger) *Syncer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Syncer{
		client:         client,
		logger:         logger,
		maxConcurrency: 4,
		maxAudioBytes:  128 * 1024 * 1024,
		maxCoverBytes:  16 * 1024 * 1024,
		retries:        2,
	}
}

func (s *Syncer) SetDownloadLimits(maxAudioBytes, maxCoverBytes int64) {
	if maxAudioBytes > 0 {
		s.maxAudioBytes = maxAudioBytes
	}
	if maxCoverBytes > 0 {
		s.maxCoverBytes = maxCoverBytes
	}
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
	nextManifest := Manifest{Clips: map[string]ManifestClip{}}
	active := map[string]struct{}{}
	unique := map[string]struct{}{}
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(s.maxConcurrency)
	for _, clip := range clips {
		clip := clip
		key := clipKey(clip)
		if _, ok := unique[key]; ok {
			mu.Lock()
			result.Skipped++
			mu.Unlock()
			continue
		}
		unique[key] = struct{}{}
		active[key] = struct{}{}
		g.Go(func() error {
			entry, downloaded, skipped, err := s.syncClip(gctx, radio.InboxDir, key, clip)
			mu.Lock()
			defer mu.Unlock()
			result.Downloaded += downloaded
			result.Skipped += skipped
			if err != nil {
				result.Errors++
				s.logger.Warn("suno clip sync failed", "radio", radio.Alias, "uuid", radio.UUID, "clip", key, "error", err)
				if oldEntry, ok := oldManifest.Clips[key]; ok {
					nextManifest.Clips[key] = oldEntry
				}
				if gctx.Err() != nil {
					return gctx.Err()
				}
				return nil
			}
			nextManifest.Clips[key] = entry
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

func (s *Syncer) syncClip(ctx context.Context, dir, key string, clip Clip) (ManifestClip, int, int, error) {
	if clip.AudioURL == "" {
		return ManifestClip{}, 0, 0, errors.New("complete clip has no audio_url")
	}
	entry := ManifestClip{
		ID:       firstNonEmpty(clip.ID, key),
		Audio:    key + ".mp3",
		Metadata: key + ".json",
	}
	downloaded := 0
	skipped := 0
	if clip.ImageURL != "" {
		coverExt, wrote, err := downloadWithExt(ctx, s.client.httpClient, s.client.baseURL, clip.ImageURL, filepath.Join(dir, key), true, s.maxCoverBytes, s.retries)
		if err != nil {
			return entry, downloaded, skipped, err
		}
		if coverExt != "" {
			entry.Cover = key + coverExt
			if wrote {
				downloaded++
			} else {
				skipped++
			}
		}
	}
	wrote, err := downloadFile(ctx, s.client.httpClient, s.client.baseURL, clip.AudioURL, filepath.Join(dir, entry.Audio), false, s.maxAudioBytes, s.retries)
	if err != nil {
		return entry, downloaded, skipped, err
	}
	if wrote {
		downloaded++
	} else {
		skipped++
	}
	if err := writeClipMetadata(filepath.Join(dir, entry.Metadata), ClipMetadata{
		ID:     entry.ID,
		Title:  strings.TrimSpace(clip.Title),
		Artist: strings.TrimSpace(clip.Handle),
	}); err != nil {
		return entry, downloaded, skipped, err
	}
	return entry, downloaded, skipped, nil
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func downloadWithExt(ctx context.Context, client *http.Client, baseURL, rawURL, base string, skipExisting bool, maxBytes int64, retries int) (string, bool, error) {
	resolved, err := resolveURL(baseURL, rawURL)
	if err != nil {
		return "", false, err
	}
	var ext string
	var wrote bool
	err = retryDownload(ctx, retries, func() error {
		var err error
		ext, wrote, err = downloadWithExtOnce(ctx, client, resolved, base, skipExisting, maxBytes)
		return err
	})
	return ext, wrote, err
}

func downloadWithExtOnce(ctx context.Context, client *http.Client, resolved, base string, skipExisting bool, maxBytes int64) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resolved, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("user-agent", defaultUserAgent)
	res, err := client.Do(req)
	if err != nil {
		return "", false, downloadErr{err: err, retryable: true}
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", false, downloadErr{err: fmt.Errorf("download %s returned %s", resolved, res.Status), retryable: retryableStatus(res.StatusCode)}
	}
	ext := extFromContentType(res.Header.Get("Content-Type"))
	if ext == "" {
		return "", false, fmt.Errorf("download %s returned unsupported cover content-type %q", resolved, res.Header.Get("Content-Type"))
	}
	wrote, err := writeResponseBody(res.Body, base+ext, skipExisting, maxBytes)
	return ext, wrote, err
}

func downloadFile(ctx context.Context, client *http.Client, baseURL, rawURL, path string, overwrite bool, maxBytes int64, retries int) (bool, error) {
	if !overwrite && existingNonEmpty(path) {
		return false, nil
	}
	resolved, err := resolveURL(baseURL, rawURL)
	if err != nil {
		return false, err
	}
	var wrote bool
	err = retryDownload(ctx, retries, func() error {
		var err error
		wrote, err = downloadFileOnce(ctx, client, resolved, path, overwrite, maxBytes)
		return err
	})
	return wrote, err
}

func downloadFileOnce(ctx context.Context, client *http.Client, resolved, path string, overwrite bool, maxBytes int64) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resolved, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("user-agent", defaultUserAgent)
	res, err := client.Do(req)
	if err != nil {
		return false, downloadErr{err: err, retryable: true}
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return false, downloadErr{err: fmt.Errorf("download %s returned %s", resolved, res.Status), retryable: retryableStatus(res.StatusCode)}
	}
	if !isAudioContentType(res.Header.Get("Content-Type")) {
		return false, fmt.Errorf("download %s returned unsupported audio content-type %q", resolved, res.Header.Get("Content-Type"))
	}
	return writeResponseBody(res.Body, path, !overwrite, maxBytes)
}

type downloadErr struct {
	err       error
	retryable bool
}

func (e downloadErr) Error() string {
	return e.err.Error()
}

func retryDownload(ctx context.Context, retries int, fn func() error) error {
	var err error
	for attempt := 0; attempt <= retries; attempt++ {
		err = fn()
		if err == nil {
			return nil
		}
		var d downloadErr
		if !errors.As(err, &d) || !d.retryable || attempt == retries {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return err
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

func writeResponseBody(r io.Reader, path string, skipExisting bool, maxBytes int64) (bool, error) {
	if skipExisting && existingNonEmpty(path) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmpFile, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return false, err
	}
	tmp := tmpFile.Name()
	limited := &io.LimitedReader{R: r, N: maxBytes + 1}
	_, copyErr := io.Copy(tmpFile, limited)
	closeErr := tmpFile.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return false, copyErr
	}
	if limited.N == 0 {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("download exceeds %d bytes", maxBytes)
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
	tmpFile, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := tmpFile.Name()
	if _, err := tmpFile.Write(b); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := tmpFile.Chmod(perm); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func existingNonEmpty(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Size() > 0
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

func isAudioContentType(contentType string) bool {
	typ, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	switch typ {
	case "audio/mpeg", "audio/mp3", "audio/mpeg3":
		return true
	default:
		return false
	}
}

func loadManifest(path string) (Manifest, error) {
	out := Manifest{Clips: map[string]ManifestClip{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return Manifest{Clips: map[string]ManifestClip{}}, err
	}
	if out.Clips == nil {
		out.Clips = map[string]ManifestClip{}
	}
	return out, nil
}

func saveManifest(path string, manifest Manifest) error {
	if manifest.Clips == nil {
		manifest.Clips = map[string]ManifestClip{}
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
	for key, clip := range manifest.Clips {
		if _, ok := active[key]; ok {
			continue
		}
		for _, name := range clip.files() {
			path, ok := cleanManifestPath(dir, name)
			if !ok {
				continue
			}
			err := os.Remove(path)
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

func (c ManifestClip) files() []string {
	files := make([]string, 0, 3)
	if c.Audio != "" {
		files = append(files, c.Audio)
	}
	if c.Cover != "" {
		files = append(files, c.Cover)
	}
	if c.Metadata != "" {
		files = append(files, c.Metadata)
	}
	return files
}

func writeClipMetadata(path string, metadata ClipMetadata) error {
	b, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if old, err := os.ReadFile(path); err == nil && string(old) == string(b) {
		return nil
	}
	return writeFileAtomic(path, b, 0o644)
}

func cleanManifestPath(dir, name string) (string, bool) {
	if name == "" || filepath.IsAbs(name) {
		return "", false
	}
	root := filepath.Clean(dir)
	path := filepath.Join(root, filepath.Clean(name))
	rel, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return path, true
}
