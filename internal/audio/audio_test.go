package audio

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestEnsureSilenceProducesCleanMP3(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "silence.mp3")
	if err := EnsureSilence(ctx, path, 20); err != nil {
		t.Fatal(err)
	}
	v, err := ValidateCleanMP3(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if v.FrameCount < 20 {
		t.Fatalf("frame count = %d, want at least 20", v.FrameCount)
	}
	if v.FrameSize != FrameSize || v.Bitrate != Bitrate || v.SampleRate != SampleRate || v.Channels != Channels {
		t.Fatalf("bad validation: %+v", v)
	}
}

func TestTranscodeRejectsSourceVBRButOutputsCleanMP3(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "vbr.mp3")
	dst := filepath.Join(dir, "clean.mp3")
	if err := exec.CommandContext(ctx, "ffmpeg",
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-c:a", "libmp3lame", "-q:a", "4",
		"-f", "mp3", src,
	).Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateCleanMP3(ctx, src); err == nil {
		t.Fatal("VBR source unexpectedly passed clean validation")
	}
	if err := TranscodeCleanMP3(ctx, src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateCleanMP3(ctx, dst); err != nil {
		t.Fatal(err)
	}
}

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe unavailable")
	}
}
