package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const maxHistoryEntries = 200

var (
	clipFilePattern = regexp.MustCompile(`(?i)^clip[0-9]+\.mp4$`)
	jobHistoryMu    sync.Mutex
)

type ClipRecord struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

type JobHistoryRecord struct {
	ID             string    `json:"id"`
	Source         string    `json:"source"`
	StartedAt      time.Time `json:"startedAt"`
	FinishedAt     time.Time `json:"finishedAt,omitempty"`
	RequestedClips int       `json:"requestedClips"`
	CreatedClips   int       `json:"createdClips"`
	Status         string    `json:"status"`
	Details        string    `json:"details,omitempty"`
}

func listClips(directory string) ([]ClipRecord, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать папку клипов: %w", err)
	}
	clips := make([]ClipRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !clipFilePattern.MatchString(entry.Name()) {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("не удалось прочитать клип %q: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		clips = append(clips, ClipRecord{
			Name:       entry.Name(),
			Path:       path,
			Size:       info.Size(),
			ModifiedAt: info.ModTime(),
		})
	}
	sort.Slice(clips, func(i, j int) bool {
		if clips[i].ModifiedAt.Equal(clips[j].ModifiedAt) {
			return strings.ToLower(clips[i].Name) < strings.ToLower(clips[j].Name)
		}
		return clips[i].ModifiedAt.After(clips[j].ModifiedAt)
	})
	return clips, nil
}

func validateClipPath(directory, path string) (string, error) {
	directoryAbs, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("не удалось определить папку результатов: %w", err)
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("не удалось определить путь к клипу: %w", err)
	}
	if !strings.EqualFold(filepath.Dir(pathAbs), directoryAbs) || !clipFilePattern.MatchString(filepath.Base(pathAbs)) {
		return "", errors.New("доступ разрешён только к готовым клипам Clipper в папке результатов")
	}
	info, err := os.Lstat(pathAbs)
	if err != nil {
		return "", fmt.Errorf("не удалось открыть клип: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("указанный путь не является обычным видеофайлом")
	}
	return pathAbs, nil
}

func (a *App) addJobHistory(request StartRequest) (string, error) {
	jobHistoryMu.Lock()
	defer jobHistoryMu.Unlock()
	records, err := readJobHistoryFile()
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	id := now.Format("20060102T150405.000000000")
	for _, record := range records {
		if record.ID == id {
			id += "-1"
			break
		}
	}
	records = append([]JobHistoryRecord{{
		ID: id, Source: request.URL, StartedAt: now,
		RequestedClips: request.ClipCount, Status: "В работе",
	}}, records...)
	if len(records) > maxHistoryEntries {
		records = records[:maxHistoryEntries]
	}
	if err := writeJobHistoryFile(records); err != nil {
		return "", err
	}
	return id, nil
}

func (a *App) finishJobHistory(id, status string, created int, details string) error {
	jobHistoryMu.Lock()
	defer jobHistoryMu.Unlock()
	records, err := readJobHistoryFile()
	if err != nil {
		return err
	}
	for index := range records {
		if records[index].ID == id {
			records[index].FinishedAt = time.Now().UTC()
			records[index].CreatedClips = created
			records[index].Status = status
			records[index].Details = details
			return writeJobHistoryFile(records)
		}
	}
	return fmt.Errorf("запись истории %q не найдена", id)
}

func (a *App) getJobHistory() (string, error) {
	records, err := readJobHistory()
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(records)
	if err != nil {
		return "", fmt.Errorf("не удалось подготовить историю обработок: %w", err)
	}
	return string(data), nil
}

func (a *App) getDiagnostics() (string, error) {
	appDir, err := executableDir()
	if err != nil {
		return "", err
	}
	model := findWhisperModel(appDir)
	entries := []struct {
		Name      string `json:"name"`
		Path      string `json:"path,omitempty"`
		Available bool   `json:"available"`
	}{
		{Name: "yt-dlp", Path: findTool(appDir, []string{"yt-dlp.exe", "yt-dlp"}, []string{"yt-dlp.exe", "yt-dlp"})},
		{Name: "FFmpeg", Path: findTool(appDir, []string{"ffmpeg.exe", "ffmpeg"}, []string{"ffmpeg.exe", "ffmpeg"})},
		{Name: "ffprobe", Path: findTool(appDir, []string{"ffprobe.exe", "ffprobe"}, []string{"ffprobe.exe", "ffprobe"})},
		{Name: "Whisper CLI", Path: findTool(appDir, []string{"whisper-cli.exe", "whisper-cli", "main.exe"}, []string{"whisper-cli.exe", "whisper-cli", "main.exe"})},
		{Name: "Deno", Path: findTool(appDir, []string{"deno.exe", "deno"}, []string{"deno.exe", "deno"})},
		{Name: "Модель распознавания", Path: model},
	}
	for index := range entries {
		entries[index].Available = entries[index].Path != ""
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("не удалось подготовить проверку компонентов: %w", err)
	}
	return string(data), nil
}

func readJobHistory() ([]JobHistoryRecord, error) {
	jobHistoryMu.Lock()
	defer jobHistoryMu.Unlock()
	return readJobHistoryFile()
}

func readJobHistoryFile() ([]JobHistoryRecord, error) {
	path, err := jobHistoryPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []JobHistoryRecord{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать историю обработок: %w", err)
	}
	var records []JobHistoryRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("история обработок повреждена (%s): %w", path, err)
	}
	if records == nil {
		records = []JobHistoryRecord{}
	}
	return records, nil
}

func writeJobHistoryFile(records []JobHistoryRecord) error {
	path, err := jobHistoryPath()
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("не удалось создать папку истории: %w", err)
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return fmt.Errorf("не удалось подготовить историю обработок: %w", err)
	}
	if err := writeFileAtomically(path, data, 0o600); err != nil {
		return fmt.Errorf("не удалось сохранить историю обработок: %w", err)
	}
	return nil
}

func jobHistoryPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("не удалось определить папку истории: %w", err)
	}
	return filepath.Join(configDir, "Clipper", "history.json"), nil
}
