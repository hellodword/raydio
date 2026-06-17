package audio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	SampleRate      int64 = 48000
	Channels        int64 = 2
	Bitrate         int64 = 192000
	FrameSize       int64 = 576
	SamplesPerFrame int64 = 1152
	FrameDurationMs int64 = 24
)

type Probe struct {
	Format struct {
		Duration string `json:"duration"`
		BitRate  string `json:"bit_rate"`
	} `json:"format"`
	Streams []struct {
		CodecType     string `json:"codec_type"`
		CodecName     string `json:"codec_name"`
		SampleRate    string `json:"sample_rate"`
		Channels      int64  `json:"channels"`
		BitRate       string `json:"bit_rate"`
		Duration      string `json:"duration"`
		ChannelLayout string `json:"channel_layout"`
	} `json:"streams"`
}

type Validation struct {
	DurationMs int64
	FrameCount int64
	FrameSize  int64
	Bitrate    int64
	SampleRate int64
	Channels   int64
}

func FFprobe(ctx context.Context, path string) (Probe, error) {
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-of", "json", "-show_format", "-show_streams", path)
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return Probe{}, fmt.Errorf("ffprobe %s: %w: %s", path, err, strings.TrimSpace(stderr.String()))
	}
	var p Probe
	if err := json.Unmarshal(out.Bytes(), &p); err != nil {
		return Probe{}, err
	}
	return p, nil
}

func TranscodeCleanMP3(ctx context.Context, input, output string) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	tmp := output + ".tmp"
	_ = os.Remove(tmp)
	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-i", input,
		"-map", "0:a:0", "-vn", "-sn", "-dn",
		"-ac", "2", "-ar", "48000",
		"-c:a", "libmp3lame", "-b:a", "192k", "-reservoir", "0",
		"-map_metadata", "-1",
		"-id3v2_version", "0", "-write_xing", "0",
		"-f", "mp3", tmp,
	}
	if err := run(ctx, "ffmpeg", args...); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if _, err := ValidateCleanMP3(ctx, tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, output)
}

func EnsureSilence(ctx context.Context, output string, frames int64) error {
	if frames <= 0 {
		return errors.New("silence frame count must be positive")
	}
	if _, err := os.Stat(output); err == nil {
		v, err := ValidateCleanMP3(ctx, output)
		if err == nil && v.FrameCount >= frames {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	tmp := output + ".tmp"
	_ = os.Remove(tmp)
	duration := time.Duration(frames*SamplesPerFrame) * time.Second / time.Duration(SampleRate)
	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo",
		"-t", fmt.Sprintf("%.6f", duration.Seconds()),
		"-ac", "2", "-ar", "48000",
		"-c:a", "libmp3lame", "-b:a", "192k", "-reservoir", "0",
		"-map_metadata", "-1",
		"-id3v2_version", "0", "-write_xing", "0",
		"-f", "mp3", tmp,
	}
	if err := run(ctx, "ffmpeg", args...); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	v, err := ValidateCleanMP3(ctx, tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if v.FrameCount < frames {
		_ = os.Remove(tmp)
		return fmt.Errorf("silence has %d frames, want at least %d", v.FrameCount, frames)
	}
	return os.Rename(tmp, output)
}

func ValidateCleanMP3(ctx context.Context, path string) (Validation, error) {
	if err := validateNoID3(path); err != nil {
		return Validation{}, err
	}
	p, err := FFprobe(ctx, path)
	if err != nil {
		return Validation{}, err
	}
	st, err := audioStream(p)
	if err != nil {
		return Validation{}, err
	}
	sampleRate, _ := strconv.ParseInt(st.SampleRate, 10, 64)
	bitrate, _ := strconv.ParseInt(firstNonEmpty(st.BitRate, p.Format.BitRate), 10, 64)
	if sampleRate != SampleRate {
		return Validation{}, fmt.Errorf("sample rate %d, want %d", sampleRate, SampleRate)
	}
	if st.Channels != Channels {
		return Validation{}, fmt.Errorf("channels %d, want %d", st.Channels, Channels)
	}
	if bitrate != Bitrate {
		return Validation{}, fmt.Errorf("bitrate %d, want %d", bitrate, Bitrate)
	}
	frameCount, err := validateFrameSizes(ctx, path)
	if err != nil {
		return Validation{}, err
	}
	durationMs := frameCount * SamplesPerFrame * 1000 / SampleRate
	return Validation{
		DurationMs: durationMs,
		FrameCount: frameCount,
		FrameSize:  FrameSize,
		Bitrate:    bitrate,
		SampleRate: sampleRate,
		Channels:   st.Channels,
	}, nil
}

func validateNoID3(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	header := make([]byte, 3)
	n, err := io.ReadFull(f, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	if n == 3 && string(header) == "ID3" {
		return errors.New("mp3 starts with ID3 tag")
	}
	return nil
}

func validateFrameSizes(ctx context.Context, path string) (int64, error) {
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "a:0", "-show_frames", "-show_entries", "frame=pkt_size", "-of", "csv=p=0", path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	scanner := bufio.NewScanner(stdout)
	var count int64
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		size, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			_ = cmd.Wait()
			return 0, err
		}
		if size != FrameSize {
			_ = cmd.Wait()
			return 0, fmt.Errorf("frame size %d, want %d", size, FrameSize)
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		_ = cmd.Wait()
		return 0, err
	}
	if err := cmd.Wait(); err != nil {
		return 0, fmt.Errorf("ffprobe frames: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if count == 0 {
		return 0, errors.New("no audio frames")
	}
	return count, nil
}

func audioStream(p Probe) (struct {
	CodecType     string `json:"codec_type"`
	CodecName     string `json:"codec_name"`
	SampleRate    string `json:"sample_rate"`
	Channels      int64  `json:"channels"`
	BitRate       string `json:"bit_rate"`
	Duration      string `json:"duration"`
	ChannelLayout string `json:"channel_layout"`
}, error) {
	for _, st := range p.Streams {
		if st.CodecType == "audio" {
			return st, nil
		}
	}
	return struct {
		CodecType     string `json:"codec_type"`
		CodecName     string `json:"codec_name"`
		SampleRate    string `json:"sample_rate"`
		Channels      int64  `json:"channels"`
		BitRate       string `json:"bit_rate"`
		Duration      string `json:"duration"`
		ChannelLayout string `json:"channel_layout"`
	}{}, errors.New("no audio stream")
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
