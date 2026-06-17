package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"raydio/internal/audio"
)

func TestMergeMetadataSidecarOverridesTags(t *testing.T) {
	dir := t.TempDir()
	mp3 := filepath.Join(dir, "song.mp3")
	sidecar := filepath.Join(dir, "song.json")
	if err := os.WriteFile(sidecar, []byte(`{"title":"Side Title","artist":"Side Artist","album":"Side Album","comment":"Side Comment"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := mergeMetadata(mp3, audio.Tags{
		Title:   "Tag Title",
		Artist:  "Tag Artist",
		Album:   "Tag Album",
		Comment: "Tag Comment",
	})
	if meta.Title != "Side Title" || meta.Artist != "Side Artist" || meta.Album != "Side Album" || meta.Comment != "Side Comment" {
		t.Fatalf("sidecar did not override tags: %+v", meta)
	}
}

func TestMergeMetadataFallsBackToTags(t *testing.T) {
	meta := mergeMetadata(filepath.Join(t.TempDir(), "song.mp3"), audio.Tags{
		Title:  "Tag Title",
		Artist: "Tag Artist",
	})
	if meta.Title != "Tag Title" || meta.Artist != "Tag Artist" {
		t.Fatalf("tag fallback failed: %+v", meta)
	}
}
