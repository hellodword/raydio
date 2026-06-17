package settings

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type File struct {
	DataDir   string
	GapFrames int64
	LogLevel  slog.Level
	Radios    []Radio
	Server    Server
	Suno      Suno
	Worker    Worker
}

type Radio struct {
	Alias string
	UUID  string
}

type Server struct {
	Addr               string
	ScheduleInterval   time.Duration
	StreamChunkWindow  time.Duration
	StreamBufferWindow time.Duration
	StreamWriteTimeout time.Duration
}

type Suno struct {
	SyncInterval time.Duration
	HTTPTimeout  time.Duration
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
		Suno: Suno{
			SyncInterval: 30 * time.Minute,
			HTTPTimeout:  30 * time.Second,
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
	if err := validateRadios(&cfg); err != nil {
		return File{}, fmt.Errorf("parse config %s: %w", path, err)
	}
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
	currentRadio := -1
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
		if indent != 0 && indent != 2 && indent != 4 {
			return fmt.Errorf("line %d: expected top-level key, two-space section indentation, or radio item indentation", lineNo)
		}

		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		if section == "radios" && indent == 2 {
			if !strings.HasPrefix(line, "- ") {
				return fmt.Errorf("line %d: expected radio list item", lineNo)
			}
			item := strings.TrimSpace(strings.TrimPrefix(line, "- "))
			cfg.Radios = append(cfg.Radios, Radio{})
			currentRadio = len(cfg.Radios) - 1
			if item == "" {
				continue
			}
			key, value, ok := strings.Cut(item, ":")
			if !ok {
				return fmt.Errorf("line %d: expected key: value", lineNo)
			}
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			if value == "" {
				return fmt.Errorf("line %d: missing value for %q", lineNo, key)
			}
			value, err := unquoteValue(value)
			if err != nil {
				return fmt.Errorf("line %d: %w", lineNo, err)
			}
			if err := assignRadio(&cfg.Radios[currentRadio], key, value); err != nil {
				return fmt.Errorf("line %d: %w", lineNo, err)
			}
			continue
		}
		if section == "radios" && indent == 4 {
			if currentRadio < 0 {
				return fmt.Errorf("line %d: radio field has no list item", lineNo)
			}
			key, value, ok := strings.Cut(line, ":")
			if !ok {
				return fmt.Errorf("line %d: expected key: value", lineNo)
			}
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			if value == "" {
				return fmt.Errorf("line %d: missing value for %q", lineNo, key)
			}
			value, err := unquoteValue(value)
			if err != nil {
				return fmt.Errorf("line %d: %w", lineNo, err)
			}
			if err := assignRadio(&cfg.Radios[currentRadio], key, value); err != nil {
				return fmt.Errorf("line %d: %w", lineNo, err)
			}
			continue
		}
		if section == "radios" && indent != 0 {
			return fmt.Errorf("line %d: invalid radios indentation", lineNo)
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
			if key != "server" && key != "worker" && key != "radios" && key != "suno" {
				return fmt.Errorf("line %d: unknown section %q", lineNo, key)
			}
			section = key
			currentRadio = -1
			continue
		}
		if indent == 0 {
			section = ""
			currentRadio = -1
		}
		if indent == 2 && section == "" {
			return fmt.Errorf("line %d: nested key %q has no section", lineNo, key)
		}
		if indent == 4 {
			return fmt.Errorf("line %d: four-space indentation is only valid in radios", lineNo)
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
	case "suno.sync_interval":
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("suno.sync_interval must be a Go duration")
		}
		cfg.Suno.SyncInterval = d
	case "suno.http_timeout":
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("suno.http_timeout must be a Go duration")
		}
		cfg.Suno.HTTPTimeout = d
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

func assignRadio(r *Radio, key, value string) error {
	switch key {
	case "alias":
		r.Alias = value
	case "uuid":
		r.UUID = value
	default:
		return fmt.Errorf("unknown radio key %q", key)
	}
	return nil
}

var (
	uuidPattern  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	aliasPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
)

func validateRadios(cfg *File) error {
	if len(cfg.Radios) == 0 {
		return fmt.Errorf("at least one radio is required")
	}
	aliases := map[string]struct{}{}
	uuids := map[string]struct{}{}
	for i := range cfg.Radios {
		r := &cfg.Radios[i]
		r.Alias = strings.TrimSpace(r.Alias)
		r.UUID = strings.ToLower(strings.TrimSpace(r.UUID))
		if r.Alias == "" {
			return fmt.Errorf("radio %d alias is required", i+1)
		}
		if !aliasPattern.MatchString(r.Alias) {
			return fmt.Errorf("radio %q alias must use lowercase letters, numbers, and hyphens", r.Alias)
		}
		if uuidPattern.MatchString(r.Alias) {
			return fmt.Errorf("radio alias %q must not look like a uuid", r.Alias)
		}
		if !uuidPattern.MatchString(r.UUID) {
			return fmt.Errorf("radio %q uuid must be a canonical uuid", r.Alias)
		}
		if _, ok := aliases[r.Alias]; ok {
			return fmt.Errorf("duplicate radio alias %q", r.Alias)
		}
		if _, ok := uuids[r.UUID]; ok {
			return fmt.Errorf("duplicate radio uuid %q", r.UUID)
		}
		aliases[r.Alias] = struct{}{}
		uuids[r.UUID] = struct{}{}
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
