package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadVerifiedChecksAssetDigestAndSize(t *testing.T) {
	content := []byte("verified component")
	digest := sha256.Sum256(content)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Length", "18")
		_, _ = writer.Write(content)
	}))
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "component.bin")
	asset := installerAsset{
		name: "component.bin", url: server.URL, hash: hex.EncodeToString(digest[:]), size: int64(len(content)),
	}
	var progressBytes int64
	if err := downloadVerified(t.Context(), asset, destination, func(received, _ int64) {
		progressBytes = received
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) || progressBytes != int64(len(content)) {
		t.Fatalf("download = %q (%d bytes reported), want %q (%d bytes)", got, progressBytes, content, len(content))
	}

	asset.hash = strings.Repeat("0", sha256.Size*2)
	if err := downloadVerified(t.Context(), asset, destination, func(int64, int64) {}); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("invalid digest error = %v, want SHA-256 mismatch", err)
	}
}

func TestExtractZipFilesRejectsTraversal(t *testing.T) {
	directory := t.TempDir()
	archivePath := filepath.Join(directory, "unsafe.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	entry, err := writer.Create("Release/../../escape.dll")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(entry, "unexpected"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(directory, "tools")
	err = extractZipFiles(archivePath, destination, func(name string) bool {
		return strings.HasPrefix(strings.ToLower(filepath.ToSlash(name)), "release/") &&
			strings.HasSuffix(strings.ToLower(name), ".dll")
	})
	if err == nil || !strings.Contains(err.Error(), "небезопасный путь") {
		t.Fatalf("traversal extraction error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "escape.dll")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("zip traversal wrote outside the target directory: %v", err)
	}
}

func TestPickPopularClipsUsesRequestedDuration(t *testing.T) {
	info := videoInfo{Duration: 180, Heatmap: []heatPoint{{Start: 60, Value: 10}}}
	got := pickPopularClips(info, 1, 30)
	want := []clipRange{{Start: 55, End: 85}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("requested 30-second clip = %#v, want %#v", got, want)
	}
}

func TestFindWhisperModelPrefersQuantizedModel(t *testing.T) {
	appDir := t.TempDir()
	modelDir := filepath.Join(appDir, "models")
	if err := os.MkdirAll(modelDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ggml-small.bin", "ggml-small-q5_1.bin"} {
		if err := os.WriteFile(filepath.Join(modelDir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := findWhisperModel(appDir)
	want := filepath.Join(modelDir, "ggml-small-q5_1.bin")
	if got != want {
		t.Fatalf("model = %q, want quantized model %q", got, want)
	}
}

func TestWhisperComponentsRejectMissingOrIncompleteFiles(t *testing.T) {
	directory := t.TempDir()
	missing := filepath.Join(directory, "missing.exe")
	if validWhisperExecutable(missing) {
		t.Fatal("missing Whisper executable was accepted")
	}
	shortExecutable := filepath.Join(directory, "whisper-cli.exe")
	if err := os.WriteFile(shortExecutable, []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	if validWhisperExecutable(shortExecutable) {
		t.Fatal("incomplete Whisper executable was accepted")
	}
	shortModel := filepath.Join(directory, "ggml-small-q5_1.bin")
	if err := os.WriteFile(shortModel, []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	if validInstalledWhisperModel(shortModel) {
		t.Fatal("incomplete Whisper model was accepted")
	}
}

func TestFFmpegVersionComparison(t *testing.T) {
	if !versionGreater(ffmpegVersion("ffmpeg-n10.1-latest-win64-gpl-shared.zip"), ffmpegVersion("ffmpeg-n9.0-latest-win64-gpl-shared.zip")) {
		t.Fatal("FFmpeg version sorting must compare numeric version components")
	}
}

func TestClipLibraryListsOnlyCompletedClipFiles(t *testing.T) {
	directory := t.TempDir()
	for name, content := range map[string]string{
		"clip1.mp4": "video-one",
		"clip2.mp4": "video-two",
		"raw1.mp4":  "temporary",
		"notes.txt": "not a clip",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	clips, err := listClips(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(clips) != 2 {
		t.Fatalf("library has %d entries, want 2: %#v", len(clips), clips)
	}
	for _, clip := range clips {
		if !clipFilePattern.MatchString(clip.Name) || clip.Size == 0 {
			t.Errorf("unexpected library item: %#v", clip)
		}
	}
}

func TestValidateClipPathRestrictsOperationsToOutputClips(t *testing.T) {
	directory := t.TempDir()
	clipPath := filepath.Join(directory, "clip14.mp4")
	if err := os.WriteFile(clipPath, []byte("clip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := validateClipPath(directory, clipPath); err != nil || got != clipPath {
		t.Fatalf("valid clip path = %q, %v", got, err)
	}
	for _, invalid := range []string{
		filepath.Join(directory, "raw14.mp4"),
		filepath.Join(t.TempDir(), "clip14.mp4"),
		directory,
	} {
		if _, err := validateClipPath(directory, invalid); err == nil {
			t.Errorf("validateClipPath(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestJobHistoryPersistsResultsAcrossUpdates(t *testing.T) {
	t.Setenv("APPDATA", t.TempDir())
	app := &App{}
	request := StartRequest{URL: "https://example.com/video", ClipCount: 4}
	id, err := app.addJobHistory(request)
	if err != nil {
		t.Fatal(err)
	}
	records, err := readJobHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != id || records[0].Status != "В работе" {
		t.Fatalf("new history record = %#v", records)
	}
	if err := app.finishJobHistory(id, "Готово", 3, "Создано три клипа"); err != nil {
		t.Fatal(err)
	}
	data, err := app.getJobHistory()
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(data), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Status != "Готово" || records[0].CreatedClips != 3 || records[0].Details != "Создано три клипа" {
		t.Fatalf("finished history record = %#v", records)
	}
}
