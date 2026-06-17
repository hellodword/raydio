package settings

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type File struct {
	DataDir   string
	GapFrames int64
	LogLevel  slog.Level
	Server    Server
	Worker    Worker
}

type Server struct {
	Addr               string
	ScheduleInterval   time.Duration
	StreamChunkWindow  time.Duration
	StreamBufferWindow time.Duration
	StreamWriteTimeout time.Duration
}

type Worker struct {
	InboxDir       string
	RescanInterval time.Duration
}

func Defaults() File {
	return File{
		DataDir:   "./data",
		GapFrames: 209,
		LogLevel:  slog.LevelDebug,
		Server: Server{
			Addr:               ":8080",
			ScheduleInterval:   time.Minute,
			StreamChunkWindow:  480 * time.Millisecond,
			StreamBufferWindow: 2 * time.Second,
			StreamWriteTimeout: 5 * time.Second,
		},
		Worker: Worker{
			RescanInterval: 30 * time.Second,
		},
	}
}

func Load(path string) (File, error) {
	cfg := Defaults()
	b, err := os.ReadFile(path)
	if err != nil {
		return File{}, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := parseYAMLSubset(b, &cfg); err != nil {
		return File{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	resolvePaths(filepath.Dir(path), &cfg)
	return cfg, nil
}

func resolvePaths(base string, cfg *File) {
	cfg.DataDir = resolvePath(base, cfg.DataDir)
	cfg.Worker.InboxDir = resolvePath(base, cfg.Worker.InboxDir)
}

func resolvePath(base, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Clean(filepath.Join(base, path))
}

func parseYAMLSubset(b []byte, cfg *File) error {
	scanner := bufio.NewScanner(bytes.NewReader(b))
	section := ""
	for lineNo := 1; scanner.Scan(); lineNo++ {
		raw := scanner.Text()
		if lineNo == 1 {
			raw = strings.TrimPrefix(raw, "\ufeff")
		}
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(raw, "\t") {
			return fmt.Errorf("line %d: tabs are not supported for indentation", lineNo)
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		if indent != 0 && indent != 2 {
			return fmt.Errorf("line %d: expected top-level key or two-space section indentation", lineNo)
		}

		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return fmt.Errorf("line %d: expected key: value", lineNo)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return fmt.Errorf("line %d: empty key", lineNo)
		}

		if indent == 0 && value == "" {
			if key != "server" && key != "worker" {
				return fmt.Errorf("line %d: unknown section %q", lineNo, key)
			}
			section = key
			continue
		}
		if indent == 0 {
			section = ""
		}
		if indent == 2 && section == "" {
			return fmt.Errorf("line %d: nested key %q has no section", lineNo, key)
		}
		if value == "" {
			return fmt.Errorf("line %d: missing value for %q", lineNo, key)
		}
		value, err := unquoteValue(value)
		if err != nil {
			return fmt.Errorf("line %d: %w", lineNo, err)
		}

		fullKey := key
		if indent == 2 {
			fullKey = section + "." + key
		}
		if err := assign(cfg, fullKey, value); err != nil {
			return fmt.Errorf("line %d: %w", lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func stripComment(s string) string {
	inSingle := false
	inDouble := false
	escaped := false
	for i, r := range s {
		switch {
		case escaped:
			escaped = false
		case inDouble && r == '\\':
			escaped = true
		case !inDouble && r == '\'':
			inSingle = !inSingle
		case !inSingle && r == '"':
			inDouble = !inDouble
		case !inSingle && !inDouble && r == '#':
			return s[:i]
		}
	}
	return s
}

func unquoteValue(value string) (string, error) {
	if strings.HasPrefix(value, `"`) {
		out, err := strconv.Unquote(value)
		if err != nil {
			return "", fmt.Errorf("invalid quoted value %q", value)
		}
		return out, nil
	}
	if strings.HasPrefix(value, "'") {
		if !strings.HasSuffix(value, "'") || len(value) == 1 {
			return "", fmt.Errorf("invalid quoted value %q", value)
		}
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'"), nil
	}
	return value, nil
}

func assign(cfg *File, key, value string) error {
	switch key {
	case "data_dir":
		cfg.DataDir = value
	case "gap_frames":
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("gap_frames must be an integer")
		}
		cfg.GapFrames = n
	case "log_level":
		level, err := parseLogLevel(value)
		if err != nil {
			return err
		}
		cfg.LogLevel = level
	case "server.addr":
		cfg.Server.Addr = value
	case "server.schedule_interval":
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("server.schedule_interval must be a Go duration")
		}
		cfg.Server.ScheduleInterval = d
	case "server.stream_chunk_window":
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("server.stream_chunk_window must be a Go duration")
		}
		cfg.Server.StreamChunkWindow = d
	case "server.stream_buffer_window":
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("server.stream_buffer_window must be a Go duration")
		}
		cfg.Server.StreamBufferWindow = d
	case "server.stream_write_timeout":
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("server.stream_write_timeout must be a Go duration")
		}
		cfg.Server.StreamWriteTimeout = d
	case "worker.inbox_dir":
		cfg.Worker.InboxDir = value
	case "worker.rescan_interval":
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("worker.rescan_interval must be a Go duration")
		}
		cfg.Worker.RescanInterval = d
	default:
		return fmt.Errorf("unknown key %q", key)
	}
	return nil
}

func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "DEBUG":
		return slog.LevelDebug, nil
	case "INFO":
		return slog.LevelInfo, nil
	case "WARN", "WARNING":
		return slog.LevelWarn, nil
	case "ERROR":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("log_level must be DEBUG, INFO, WARN, or ERROR")
	}
}
