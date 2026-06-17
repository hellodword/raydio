package audio

import (
	"context"
	"os"
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

func TestEnsureSilenceConcurrentSameOutput(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "silence.mp3")
	errCh := make(chan error, 8)
	start := make(chan struct{})
	for range cap(errCh) {
		go func() {
			<-start
			errCh <- EnsureSilence(ctx, path, 20)
		}()
	}

	close(start)
	for range cap(errCh) {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ValidateCleanMP3(ctx, path); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "silence.mp3.*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files left behind: %v", matches)
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

func TestTranscodeCleanMP3ConcurrentSameOutput(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "source.mp3")
	dst := filepath.Join(dir, "clean.mp3")
	if err := exec.CommandContext(ctx, "ffmpeg",
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-c:a", "libmp3lame", "-q:a", "4",
		"-f", "mp3", src,
	).Run(); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 6)
	start := make(chan struct{})
	for range cap(errCh) {
		go func() {
			<-start
			errCh <- TranscodeCleanMP3(ctx, src, dst)
		}()
	}

	close(start)
	for range cap(errCh) {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ValidateCleanMP3(ctx, dst); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "clean.mp3.*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files left behind: %v", matches)
	}
}

func TestValidateRejectsNonNormalizedMP3Parameters(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()
	cases := []struct {
		name string
		args []string
	}{
		{
			name: "sample-rate",
			args: []string{"-ac", "2", "-ar", "44100", "-b:a", "192k"},
		},
		{
			name: "channels",
			args: []string{"-ac", "1", "-ar", "48000", "-b:a", "192k"},
		},
		{
			name: "bitrate",
			args: []string{"-ac", "2", "-ar", "48000", "-b:a", "128k"},
		},
	}
	for _, tc := range cases {
		path := filepath.Join(dir, tc.name+".mp3")
		args := []string{
			"-nostdin", "-hide_banner", "-loglevel", "error",
			"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
			"-c:a", "libmp3lame", "-reservoir", "0",
			"-map_metadata", "-1",
			"-id3v2_version", "0", "-write_xing", "0",
		}
		args = append(args, tc.args...)
		args = append(args, "-f", "mp3", path)
		if err := exec.CommandContext(ctx, "ffmpeg", args...).Run(); err != nil {
			t.Fatal(err)
		}
		if _, err := ValidateCleanMP3(ctx, path); err == nil {
			t.Fatalf("%s unexpectedly passed validation", tc.name)
		}
	}

	id3Path := filepath.Join(dir, "id3.mp3")
	if err := os.WriteFile(id3Path, []byte("ID3bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateCleanMP3(ctx, id3Path); err == nil {
		t.Fatal("ID3 header unexpectedly passed validation")
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
