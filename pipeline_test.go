package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPickPopularClipsKeepsBestNonOverlappingMoments(t *testing.T) {
	info := videoInfo{
		Duration: 180,
		Heatmap: []heatPoint{
			{Start: 30, Value: 10},
			{Start: 40, Value: 9},
			{Start: 120, Value: 8},
		},
	}

	got := pickPopularClips(info, 3)
	want := []clipRange{{Start: 25, End: 70}, {Start: 115, End: 160}}
	if len(got) != len(want) {
		t.Fatalf("got %d clips, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("clip %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestPickPopularClipsCapsEndAtVideoDuration(t *testing.T) {
	got := pickPopularClips(videoInfo{
		Duration: 30,
		Heatmap:  []heatPoint{{Start: 20, Value: 5}},
	}, 1)
	if len(got) != 1 || got[0] != (clipRange{Start: 15, End: 30}) {
		t.Fatalf("got %#v, want one range from 15 to 30 seconds", got)
	}
}

func TestParseVTTDistributesWordsAcrossCue(t *testing.T) {
	input := strings.NewReader("WEBVTT\n\n1\n00:00:01.000 --> 00:00:03.000\nHELLO AMAZING\n")
	got, err := parseVTT(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d words, want 2: %#v", len(got), got)
	}
	if got[0].Text != "HELLO" || got[0].Start != 1 || got[1].Text != "AMAZING" || got[1].End != 3 {
		t.Fatalf("unexpected parsed words: %#v", got)
	}
	if got[0].End <= got[0].Start || got[1].Start < got[0].End {
		t.Fatalf("word timestamps are not ordered: %#v", got)
	}
}

func TestParseVTTSupportsHours(t *testing.T) {
	got, err := parseVTT(strings.NewReader("WEBVTT\n\n1:02:03.500 --> 1:02:04.500\nWORD\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Start != 3723.5 || got[0].End != 3724.5 {
		t.Fatalf("unexpected parsed timestamps: %#v", got)
	}
}

func TestParseVTTDecodesEntitiesAndShortTimestamps(t *testing.T) {
	got, err := parseVTT(strings.NewReader("WEBVTT\n\n00:01.000 --> 00:02.000\nTom &amp; Jerry\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Start != 1 || got[2].End != 2 || got[1].Text != "&" {
		t.Fatalf("unexpected parsed cue: %#v", got)
	}
}

func TestFormatASSTimeRoundsAcrossMinuteBoundary(t *testing.T) {
	if got := formatASSTime(59.999); got != "0:01:00.00" {
		t.Fatalf("formatASSTime(59.999) = %q, want 0:01:00.00", got)
	}
}

func TestWriteASSProducesKaraokeCaptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "captions.ass")
	err := writeASS([]captionWord{
		{Start: 1, End: 1.3, Text: "hello"},
		{Start: 1.4, End: 1.8, Text: "world"},
	}, path)
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	for _, expected := range []string{"PlayResX: 1080", `{\c&H00FFFF&}HELLO`, "WORLD"} {
		if !strings.Contains(text, expected) {
			t.Errorf("ASS output does not contain %q", expected)
		}
	}
}

func TestValidHTTPURL(t *testing.T) {
	tests := []struct {
		value string
		valid bool
	}{
		{"https://youtu.be/example", true},
		{"http://example.com/video", true},
		{"file:///video.mp4", false},
		{"https:/missing-host", false},
		{"not a url", false},
	}
	for _, test := range tests {
		if got := validHTTPURL(test.value); got != test.valid {
			t.Errorf("validHTTPURL(%q) = %t, want %t", test.value, got, test.valid)
		}
	}
}

func TestCleanupPartialPreservesCompletedClips(t *testing.T) {
	directory := t.TempDir()
	files := []string{"raw1.mp4", "clip1.mp4", "clip2.tmp.mp4", "raw3.part", "c1.ass"}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := cleanupPartial(directory); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"raw1.mp4", "clip1.mp4"} {
		if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
			t.Errorf("expected %s to be preserved: %v", name, err)
		}
	}
	for _, name := range []string{"clip2.tmp.mp4", "raw3.part", "c1.ass"} {
		if _, err := os.Stat(filepath.Join(directory, name)); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed", name)
		}
	}
}

func TestRenderClipCreatesVerticalVideo(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is not installed")
	}
	directory := t.TempDir()
	source := filepath.Join(directory, "source.mp4")
	background := filepath.Join(directory, "background.mp4")
	for path, color := range map[string]string{source: "red", background: "blue"} {
		command := exec.Command(ffmpeg,
			"-y", "-loglevel", "error",
			"-f", "lavfi", "-i", "color=c="+color+":s=160x90:r=10:d=1",
			"-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=44100:duration=1",
			"-shortest", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac",
			path,
		)
		if output, err := command.CombinedOutput(); err != nil {
			t.Skipf("FFmpeg does not support the test encoders: %v: %s", err, output)
		}
	}
	if err := writeASS([]captionWord{{Start: 0, End: 0.5, Text: "test"}}, filepath.Join(directory, "captions.ass")); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(directory, "clip.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := renderClip(ctx, ffmpeg, directory, source, background, 0, false, "captions.ass", output); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(output); err != nil || info.Size() == 0 {
		t.Fatalf("rendered clip was not created: info=%v, err=%v", info, err)
	}
	dimensions, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height", "-of", "csv=s=x:p=0", output).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(dimensions)); got != "1080x1920" {
		t.Fatalf("rendered video dimensions = %q, want 1080x1920", got)
	}
}
