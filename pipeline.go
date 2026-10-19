package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultClipLength = 45
	minClipLength     = 15
	maxClipLength     = 90
)

type videoInfo struct {
	Title    string      `json:"title"`
	Duration float64     `json:"duration"`
	Heatmap  []heatPoint `json:"heatmap"`
}

type heatPoint struct {
	Start float64 `json:"start_time"`
	Value float64 `json:"value"`
}

type clipRange struct {
	Start float64
	End   float64
}

type captionWord struct {
	Start float64
	End   float64
	Text  string
}

type tools struct {
	youtubeDL string
	ffmpeg    string
	ffprobe   string
	whisper   string
	model     string
}

type videoEncoder struct {
	name    string
	options []string
}

var renderEncoderCache sync.Map

func processVideo(
	ctx context.Context,
	request StartRequest,
	clipCount int,
	workDir string,
	progress func(UIEvent),
	onClip func(string) error,
) (int, error) {
	progress(UIEvent{Type: "progress", Message: "Проверяю Whisper и модель распознавания…"})
	if err := ensureWhisperComponents(ctx, func(message string, _ float64) {
		progress(UIEvent{Type: "progress", Message: message})
	}); err != nil {
		return 0, fmt.Errorf("не удалось подготовить Whisper: %w", err)
	}
	resolved, err := resolveTools()
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return 0, fmt.Errorf("не удалось создать папку клипов: %w", err)
	}
	if err := cleanupPartial(workDir); err != nil {
		return 0, err
	}

	progress(UIEvent{Type: "progress", Message: "Читаю данные исходного видео…"})
	info, err := getVideoInfo(ctx, resolved.youtubeDL, workDir, request.URL)
	if err != nil {
		return 0, err
	}
	if info.Title != "" {
		progress(UIEvent{Type: "progress", Message: "Видео: " + info.Title})
	}
	progress(UIEvent{Type: "progress", Message: "Ищу лучшие моменты…"})
	moments := pickPopularClips(info, clipCount, request.ClipDuration)
	if len(moments) == 0 {
		progress(UIEvent{Type: "progress", Message: "График просмотров недоступен, анализирую звук…"})
		moments, err = audioClips(ctx, resolved, workDir, request.URL, info.Duration, clipCount, request.ClipDuration)
		if err != nil {
			return 0, err
		}
	}
	if len(moments) == 0 {
		return 0, errors.New("не удалось найти подходящие моменты в видео")
	}
	progress(UIEvent{Type: "progress", Message: fmt.Sprintf("Найдено моментов: %d", len(moments)), Total: len(moments)})

	firstIndex, err := nextClipIndex(workDir)
	if err != nil {
		return 0, err
	}
	gameplayDuration := 0.0
	gameplayURL := request.GameplayURL
	gameplayPath := request.GameplayPath
	if gameplayURL != "" {
		progress(UIEvent{Type: "progress", Message: "Проверяю ссылку на игровой фон…"})
		gpInfo, err := getVideoInfo(ctx, resolved.youtubeDL, workDir, gameplayURL)
		if err != nil {
			if gameplayPath == "" {
				return 0, fmt.Errorf("не удалось прочитать видео для фона: %w", err)
			}
			progress(UIEvent{Type: "progress", Message: "Ссылка недоступна, использую локальный файл фона."})
			gameplayURL = ""
		} else {
			gameplayDuration = gpInfo.Duration
		}
	}
	if gameplayPath != "" {
		if _, err := os.Stat(gameplayPath); err != nil {
			if gameplayURL == "" {
				return 0, fmt.Errorf("не удалось открыть файл геймплея: %w", err)
			}
			gameplayPath = ""
		}
	}
	if gameplayURL == "" && gameplayPath == "" {
		return 0, errors.New("фоновое видео не выбрано или недоступно")
	}

	made := 0
	for index, moment := range moments {
		if err := ctx.Err(); err != nil {
			return made, err
		}
		clipIndex := firstIndex + index
		name := fmt.Sprintf("raw%d", clipIndex)
		clipName := fmt.Sprintf("clip%d.mp4", clipIndex)
		progress(UIEvent{
			Type: "progress", Message: fmt.Sprintf("Клип %d из %d · %s", index+1, len(moments), formatTime(moment.Start)),
			Current: index + 1, Total: len(moments),
		})
		rawPath, err := downloadVideoPart(ctx, resolved, workDir, request.URL, moment, name)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return made, err
			}
			progress(UIEvent{Type: "warning", Message: fmt.Sprintf("Момент %d пропущен: %s", index+1, shortError(err))})
			if logErr := appendErrorLog(workDir, fmt.Errorf("%s: %w", clipName, err)); logErr != nil {
				progress(UIEvent{Type: "warning", Message: "Не удалось записать error.log: " + shortError(logErr)})
			}
			continue
		}

		progress(UIEvent{Type: "progress", Message: "Распознаю речь и готовлю субтитры…", Current: index + 1, Total: len(moments)})
		words, err := transcribe(ctx, resolved, workDir, rawPath, clipIndex)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return made, err
			}
			progress(UIEvent{Type: "warning", Message: fmt.Sprintf("Клип %d пропущен: %s", index+1, shortError(err))})
			appendErrorLog(workDir, fmt.Errorf("%s: %w", clipName, err))
			continue
		}
		assName := fmt.Sprintf("c%d.ass", clipIndex)
		if err := writeASS(words, filepath.Join(workDir, assName)); err != nil {
			progress(UIEvent{Type: "warning", Message: fmt.Sprintf("Не удалось создать субтитры для клипа %d: %s", index+1, shortError(err))})
			continue
		}

		var backgroundPath string
		backgroundStart := 0.0
		localBackground := false
		downloadedBackground := false
		if gameplayURL != "" {
			progress(UIEvent{Type: "progress", Message: "Скачиваю случайный фрагмент геймплея…", Current: index + 1, Total: len(moments)})
			backgroundPath, backgroundStart, err = downloadGameplayPart(
				ctx, resolved, workDir, gameplayURL, gameplayDuration, moment.End-moment.Start, fmt.Sprintf("gp%d", clipIndex),
			)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return made, err
				}
				if gameplayPath == "" {
					progress(UIEvent{Type: "warning", Message: fmt.Sprintf("Не удалось скачать фон для клипа %d: %s", index+1, shortError(err))})
					continue
				}
				progress(UIEvent{Type: "warning", Message: "Не удалось скачать фон по ссылке, использую локальное видео."})
				backgroundPath = ""
			} else {
				downloadedBackground = true
			}
		}
		if backgroundPath == "" {
			backgroundPath = gameplayPath
			localBackground = true
			if localBackground {
				backgroundStart, err = randomGameplayStart(ctx, resolved.ffprobe, gameplayPath, moment.End-moment.Start)
				if err != nil {
					progress(UIEvent{Type: "warning", Message: fmt.Sprintf("Не удалось подготовить локальный фон: %s", shortError(err))})
					continue
				}
			}
		}

		progress(UIEvent{Type: "progress", Message: "Собираю вертикальное видео…", Current: index + 1, Total: len(moments)})
		outPath := filepath.Join(workDir, clipName)
		if err := renderClip(ctx, resolved.ffmpeg, workDir, rawPath, backgroundPath, backgroundStart, localBackground, assName, outPath); err != nil {
			_ = os.Remove(filepath.Join(workDir, assName))
			if errors.Is(err, context.Canceled) {
				return made, err
			}
			progress(UIEvent{Type: "warning", Message: fmt.Sprintf("Не удалось собрать %s: %s", clipName, shortError(err))})
			if logErr := appendErrorLog(workDir, fmt.Errorf("%s: %w", clipName, err)); logErr != nil {
				progress(UIEvent{Type: "warning", Message: "Не удалось записать error.log: " + shortError(logErr)})
			}
			continue
		}
		if downloadedBackground {
			_ = os.Remove(backgroundPath)
		}
		_ = os.Remove(filepath.Join(workDir, assName))
		made++
		progress(UIEvent{Type: "clip", Message: "Готов " + clipName, Current: index + 1, Total: len(moments), File: outPath})
		if onClip != nil {
			if err := onClip(outPath); err != nil {
				return made, err
			}
		}
	}
	if made == 0 {
		return 0, errors.New("ни один клип не удалось создать; подробности смотрите в error.log")
	}
	return made, nil
}

func resolveTools() (tools, error) {
	appDir, err := executableDir()
	if err != nil {
		return tools{}, err
	}
	resolved := tools{
		youtubeDL: findTool(appDir, []string{"yt-dlp.exe", "yt-dlp"}, []string{"yt-dlp.exe", "yt-dlp"}),
		ffmpeg:    findTool(appDir, []string{"ffmpeg.exe", "ffmpeg"}, []string{"ffmpeg.exe", "ffmpeg"}),
		ffprobe:   findTool(appDir, []string{"ffprobe.exe", "ffprobe"}, []string{"ffprobe.exe", "ffprobe"}),
		whisper:   findTool(appDir, []string{"whisper-cli.exe", "whisper-cli", "main.exe"}, []string{"whisper-cli.exe", "whisper-cli", "main.exe"}),
	}
	if resolved.youtubeDL == "" {
		return tools{}, errors.New("не найден yt-dlp. Установите его или поместите yt-dlp.exe в папку tools рядом с Clipper.")
	}
	if resolved.ffmpeg == "" {
		return tools{}, errors.New("не найден FFmpeg. Установите FFmpeg или поместите ffmpeg.exe и ffprobe.exe в папку tools рядом с Clipper.")
	}
	if resolved.ffprobe == "" {
		return tools{}, errors.New("не найден ffprobe. Установите FFmpeg или поместите ffprobe.exe в папку tools рядом с Clipper.")
	}
	pathValue := strings.Join([]string{
		filepath.Dir(resolved.ffmpeg),
		filepath.Dir(resolved.youtubeDL),
		os.Getenv("PATH"),
	}, string(os.PathListSeparator))
	if err := os.Setenv("PATH", pathValue); err != nil {
		return tools{}, fmt.Errorf("не удалось добавить FFmpeg в PATH для yt-dlp: %w", err)
	}
	if resolved.whisper == "" {
		return tools{}, errors.New("не найден whisper-cli. Повторите установку Clipper, чтобы восстановить Whisper.")
	}
	resolved.model = findWhisperModel(appDir)
	if resolved.model == "" {
		return tools{}, errors.New("не найдена модель Whisper. Повторите установку Clipper, чтобы восстановить модель.")
	}
	return resolved, nil
}

func executableDir() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("не удалось определить папку приложения: %w", err)
	}
	return filepath.Dir(executable), nil
}

func findTool(appDir string, localNames, pathNames []string) string {
	for _, folder := range []string{"tools", "bin"} {
		for _, name := range localNames {
			candidate := filepath.Join(appDir, folder, name)
			if fileExists(candidate) {
				return candidate
			}
		}
	}
	for _, name := range pathNames {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}

func findWhisperModel(appDir string) string {
	if fromEnv := strings.TrimSpace(os.Getenv("CLIPPER_WHISPER_MODEL")); fromEnv != "" && fileExists(fromEnv) {
		return fromEnv
	}
	for _, candidate := range []string{
		filepath.Join(appDir, "models", "ggml-small-q5_1.bin"),
		filepath.Join(appDir, "tools", "models", "ggml-small-q5_1.bin"),
		filepath.Join(appDir, "models", "ggml-small.bin"),
		filepath.Join(appDir, "tools", "models", "ggml-small.bin"),
	} {
		if fileExists(candidate) {
			return candidate
		}
	}
	if configDir, err := os.UserConfigDir(); err == nil {
		for _, modelName := range []string{"ggml-small-q5_1.bin", "ggml-small.bin"} {
			candidate := filepath.Join(configDir, "Clipper", "models", modelName)
			if fileExists(candidate) {
				return candidate
			}
		}
	}
	return ""
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func runCommand(ctx context.Context, directory, executable string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, executable, args...)
	hideConsoleWindow(command)
	command.Dir = directory
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		if runtime.GOOS == "windows" {
			kill := exec.Command("taskkill.exe", "/PID", strconv.Itoa(command.Process.Pid), "/T", "/F")
			hideConsoleWindow(kill)
			_ = kill.Run()
		}
		return command.Process.Kill()
	}
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		message := strings.TrimSpace(stderr.String())
		if len(message) > 1200 {
			message = message[len(message)-1200:]
		}
		if message != "" {
			return nil, fmt.Errorf("%s: %w: %s", filepath.Base(executable), err, message)
		}
		return nil, fmt.Errorf("%s: %w", filepath.Base(executable), err)
	}
	return stdout.Bytes(), nil
}

func getVideoInfo(ctx context.Context, youtubeDL, workDir, url string) (videoInfo, error) {
	var best videoInfo
	var lastError error
	for attempt := 0; attempt < 3; attempt++ {
		args := []string{"--dump-single-json", "--no-warnings", "--no-playlist", "--no-call-home"}
		if attempt == 1 {
			args = append(args, "--extractor-args", "youtube:player_client=web")
		} else if attempt == 2 {
			args = append(args, "--extractor-args", "youtube:player_client=ios")
		}
		args = append(args, "--", url)
		output, err := runCommand(ctx, workDir, youtubeDL, args...)
		if err != nil {
			lastError = err
			continue
		}
		var current videoInfo
		if err := json.Unmarshal(output, &current); err != nil {
			lastError = fmt.Errorf("yt-dlp вернул некорректные данные: %w", err)
			continue
		}
		if best.Duration == 0 {
			best = current
		}
		if len(current.Heatmap) > 0 {
			return current, nil
		}
	}
	if best.Duration > 0 {
		return best, nil
	}
	if lastError != nil {
		return videoInfo{}, fmt.Errorf("не удалось прочитать видео: %w", lastError)
	}
	return videoInfo{}, errors.New("yt-dlp не вернул данные о видео")
}

func pickPopularClips(info videoInfo, maxClips int, clipDurations ...int) []clipRange {
	if maxClips <= 0 {
		return nil
	}
	length := defaultClipLength
	if len(clipDurations) > 0 && clipDurations[0] >= minClipLength && clipDurations[0] <= maxClipLength {
		length = clipDurations[0]
	}
	points := append([]heatPoint(nil), info.Heatmap...)
	sort.Slice(points, func(i, j int) bool { return points[i].Value > points[j].Value })
	clips := make([]clipRange, 0, maxClips)
	for _, point := range points {
		start := math.Max(0, point.Start-5)
		end := math.Min(info.Duration, start+float64(length))
		if end-start < minClipLength {
			continue
		}
		candidate := clipRange{Start: start, End: end}
		if nonOverlapping(candidate, clips) {
			clips = append(clips, candidate)
		}
		if len(clips) == maxClips {
			break
		}
	}
	return clips
}

func audioClips(ctx context.Context, resolved tools, workDir, url string, duration float64, maxClips int, clipDurations ...int) ([]clipRange, error) {
	outputTemplate := filepath.Join(workDir, "audio_full.%(ext)s")
	_, err := runCommand(ctx, workDir, resolved.youtubeDL,
		"--format", "ba", "--quiet", "--no-warnings", "--no-playlist",
		"--output", outputTemplate, "--", url,
	)
	if err != nil {
		return nil, fmt.Errorf("не удалось скачать звук для поиска моментов: %w", err)
	}
	audioPath, err := findOutputFile(workDir, "audio_full.")
	if err != nil {
		return nil, err
	}
	defer os.Remove(audioPath)

	rms, err := decodeRMS(ctx, resolved.ffmpeg, workDir, audioPath)
	if err != nil {
		return nil, fmt.Errorf("не удалось проанализировать звуковую дорожку: %w", err)
	}
	if duration > 0 && duration < float64(len(rms)) {
		rms = rms[:int(math.Max(0, duration))]
	}
	length := defaultClipLength
	if len(clipDurations) > 0 && clipDurations[0] >= minClipLength && clipDurations[0] <= maxClipLength {
		length = clipDurations[0]
	}
	window := min(length, len(rms))
	if window < 15 {
		return nil, errors.New("в видео недостаточно звука для поиска моментов")
	}
	prefix := make([]float64, len(rms)+1)
	for i, value := range rms {
		prefix[i+1] = prefix[i] + value
	}
	type score struct {
		start int
		value float64
	}
	scores := make([]score, len(rms)-window+1)
	for i := range scores {
		scores[i] = score{start: i, value: prefix[i+window] - prefix[i]}
	}
	sort.Slice(scores, func(i, j int) bool { return scores[i].value > scores[j].value })

	clips := make([]clipRange, 0, maxClips)
	for _, candidate := range scores {
		clip := clipRange{Start: float64(candidate.start), End: float64(candidate.start + window)}
		if nonOverlapping(clip, clips) {
			clips = append(clips, clip)
		}
		if len(clips) == maxClips {
			break
		}
	}
	sort.Slice(clips, func(i, j int) bool { return clips[i].Start < clips[j].Start })
	return clips, nil
}

func decodeRMS(ctx context.Context, ffmpeg, workDir, path string) ([]float64, error) {
	command := exec.CommandContext(ctx, ffmpeg, "-i", path, "-ac", "1", "-ar", "8000", "-f", "s16le", "-loglevel", "error", "-")
	hideConsoleWindow(command)
	command.Dir = workDir
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		if runtime.GOOS == "windows" {
			kill := exec.Command("taskkill.exe", "/PID", strconv.Itoa(command.Process.Pid), "/T", "/F")
			hideConsoleWindow(kill)
			_ = kill.Run()
		}
		return command.Process.Kill()
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	const samplesPerSecond = 8000
	pcm := make([]byte, samplesPerSecond*2)
	values := make([]float64, 0, 3600)
	for {
		count, readErr := io.ReadFull(stdout, pcm)
		if count > 0 {
			squares := 0.0
			samples := count / 2
			for i := 0; i < samples; i++ {
				value := float64(int16(uint16(pcm[2*i])|uint16(pcm[2*i+1])<<8)) / 32768.0
				squares += value * value
			}
			values = append(values, math.Sqrt(squares/float64(samples)))
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
				_ = command.Process.Kill()
				_ = command.Wait()
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, readErr
			}
			break
		}
	}
	if err := command.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return values, nil
}

func nonOverlapping(candidate clipRange, selected []clipRange) bool {
	for _, clip := range selected {
		if candidate.Start < clip.End && candidate.End > clip.Start {
			return false
		}
	}
	return true
}

func downloadVideoPart(ctx context.Context, resolved tools, workDir, url string, clip clipRange, name string) (string, error) {
	template := filepath.Join(workDir, name+".%(ext)s")
	_, err := runCommand(ctx, workDir, resolved.youtubeDL,
		"--quiet", "--no-warnings", "--no-playlist",
		"--format", "bv*[height<=1080]+ba/bv*+ba",
		"--format-sort", "res:1080,fps,vcodec:h264",
		"--merge-output-format", "mp4",
		"--download-sections", fmt.Sprintf("*%.2f-%.2f", clip.Start, clip.End),
		"--force-keyframes-at-cuts",
		"--output", template,
		"--", url,
	)
	if err != nil {
		return "", fmt.Errorf("не удалось скачать фрагмент: %w", err)
	}
	return findOutputFile(workDir, name+".")
}

func downloadGameplayPart(ctx context.Context, resolved tools, workDir, url string, duration, length float64, name string) (string, float64, error) {
	need := length + 3
	start := 0.0
	if duration > need {
		start = randomFloat(duration - need)
	}
	template := filepath.Join(workDir, name+".%(ext)s")
	_, err := runCommand(ctx, workDir, resolved.youtubeDL,
		"--quiet", "--no-warnings", "--no-playlist",
		"--format", "bv*[height<=1080]/bv*/b",
		"--format-sort", "res:1080,fps,vcodec:h264",
		"--merge-output-format", "mp4",
		"--download-sections", fmt.Sprintf("*%.2f-%.2f", start, start+need),
		"--force-keyframes-at-cuts",
		"--output", template,
		"--", url,
	)
	if err != nil {
		return "", 0, fmt.Errorf("не удалось скачать видео для фона: %w", err)
	}
	path, err := findOutputFile(workDir, name+".")
	return path, start, err
}

func findOutputFile(directory, prefix string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(directory, prefix+"*"))
	if err != nil {
		return "", fmt.Errorf("не удалось найти скачанный файл: %w", err)
	}
	for _, path := range matches {
		if strings.HasSuffix(path, ".part") || strings.HasSuffix(path, ".ytdl") || strings.HasSuffix(path, ".temp") {
			continue
		}
		info, statErr := os.Stat(path)
		if statErr == nil && !info.IsDir() {
			return path, nil
		}
	}
	return "", fmt.Errorf("скачанный файл %q не найден", prefix)
}

func transcribe(ctx context.Context, resolved tools, workDir, source string, index int) ([]captionWord, error) {
	base := filepath.Join(workDir, fmt.Sprintf("transcript%d", index))
	audioPath := base + ".wav"
	defer os.Remove(audioPath)
	_, err := runCommand(ctx, workDir, resolved.ffmpeg,
		"-y", "-loglevel", "error", "-i", source,
		"-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", audioPath,
	)
	if err != nil {
		return nil, fmt.Errorf("не удалось подготовить звук для распознавания: %w", err)
	}
	_, err = runCommand(ctx, workDir, resolved.whisper,
		"--model", resolved.model,
		"--file", audioPath,
		"--language", "auto",
		"--threads", strconv.Itoa(processingThreads()),
		"--output-vtt",
		"--output-file", base,
		"--no-prints",
		"--max-len", "1",
		"--split-on-word",
	)
	if err != nil {
		return nil, fmt.Errorf("ошибка распознавания речи: %w", err)
	}
	vttPath := base + ".vtt"
	defer os.Remove(vttPath)
	file, err := os.Open(vttPath)
	if err != nil {
		return nil, fmt.Errorf("whisper-cli не создал файл субтитров: %w", err)
	}
	defer file.Close()
	return parseVTT(file)
}

var vttTimePattern = regexp.MustCompile(`(?m)^(?:\d+:)?\d{2}:\d{2}\.\d{3}\s+-->\s+(?:\d+:)?\d{2}:\d{2}\.\d{3}`)
var vttMarkupPattern = regexp.MustCompile(`<[^>]*>`)

func parseVTT(input io.Reader) ([]captionWord, error) {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var words []captionWord
	var start, end float64
	var textLines []string
	hasCue := false
	flush := func() {
		if !hasCue {
			return
		}
		text := strings.Join(textLines, " ")
		text = html.UnescapeString(vttMarkupPattern.ReplaceAllString(text, ""))
		tokens := strings.Fields(text)
		if len(tokens) > 0 && end > start {
			totalChars := 0
			for _, token := range tokens {
				totalChars += max(1, len([]rune(token)))
			}
			elapsed := start
			for i, token := range tokens {
				ratio := float64(max(1, len([]rune(token)))) / float64(totalChars)
				wordEnd := end
				if i < len(tokens)-1 {
					wordEnd = elapsed + (end-start)*ratio
				}
				clean := strings.Trim(token, " \t\r\n")
				if clean != "" {
					words = append(words, captionWord{Start: elapsed, End: wordEnd, Text: clean})
				}
				elapsed = wordEnd
			}
		}
		hasCue = false
		textLines = nil
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			flush()
			continue
		}
		if match := vttTimePattern.FindString(line); match != "" {
			flush()
			parts := strings.Split(match, "-->")
			start, _ = parseVTTTime(strings.TrimSpace(parts[0]))
			end, _ = parseVTTTime(strings.TrimSpace(parts[1]))
			hasCue = true
			continue
		}
		if line == "WEBVTT" || strings.HasPrefix(line, "NOTE") || !hasCue {
			continue
		}
		textLines = append(textLines, line)
	}
	flush()
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("не удалось прочитать субтитры: %w", err)
	}
	return words, nil
}

func parseVTTTime(value string) (float64, error) {
	value = strings.TrimSpace(value)
	parts := strings.Split(value, ":")
	var hours, minutes int
	var seconds float64
	var err error
	if len(parts) == 3 {
		hours, err = strconv.Atoi(parts[0])
		if err != nil {
			return 0, err
		}
		minutes, err = strconv.Atoi(parts[1])
		if err != nil {
			return 0, err
		}
		seconds, err = strconv.ParseFloat(parts[2], 64)
	} else if len(parts) == 2 {
		minutes, err = strconv.Atoi(parts[0])
		if err != nil {
			return 0, err
		}
		seconds, err = strconv.ParseFloat(parts[1], 64)
	} else {
		return 0, fmt.Errorf("неверный формат времени %q", value)
	}
	if err != nil {
		return 0, err
	}
	return float64(hours*3600+minutes*60) + seconds, nil
}

func writeASS(words []captionWord, path string) error {
	const header = `[Script Info]
ScriptType: v4.00+
PlayResX: 1080
PlayResY: 1920

[V4+ Styles]
Format: Name,Fontname,Fontsize,PrimaryColour,OutlineColour,BorderStyle,Outline,Shadow,Alignment,MarginV
Style: Default,Arial Black,95,&H00FFFFFF,&H00000000,1,8,0,5,0

[Events]
Format: Layer,Start,End,Style,Text
`
	clean := make([]captionWord, 0, len(words))
	for _, word := range words {
		word.Text = strings.TrimSpace(word.Text)
		if word.Text != "" && word.End > word.Start {
			clean = append(clean, word)
		}
	}
	lines := make([]string, 0, len(clean))
	for i, word := range clean {
		group := []captionWord{word}
		if i+1 < len(clean) && clean[i+1].Start-word.End <= 0.6 {
			group = append(group, clean[i+1])
		}
		var text strings.Builder
		for j, item := range group {
			if j > 0 {
				text.WriteByte(' ')
			}
			if j == 0 {
				text.WriteString(`{\c&H00FFFF&}`)
			}
			text.WriteString(escapeASSText(strings.ToUpper(item.Text)))
			if j == 0 {
				text.WriteString(`{\c&HFFFFFF&}`)
			}
		}
		end := minFloat(word.End+0.3, word.Start+1.5)
		if i+1 < len(clean) {
			end = math.Min(end, clean[i+1].Start)
		}
		if end < word.Start+0.1 {
			end = word.Start + 0.1
		}
		lines = append(lines, fmt.Sprintf("Dialogue: 0,%s,%s,Default,%s",
			formatASSTime(word.Start), formatASSTime(end), text.String()))
	}
	return os.WriteFile(path, []byte(header+strings.Join(lines, "\n")), 0o644)
}

func escapeASSText(text string) string {
	text = strings.ReplaceAll(text, `\`, `\\`)
	text = strings.ReplaceAll(text, "{", "")
	return strings.ReplaceAll(text, "}", "")
}

func formatASSTime(value float64) string {
	if value < 0 {
		value = 0
	}
	centiseconds := int(math.Round(value * 100))
	hours := centiseconds / 360000
	minutes := centiseconds / 6000 % 60
	seconds := float64(centiseconds%6000) / 100
	return fmt.Sprintf("%d:%02d:%05.2f", hours, minutes, seconds)
}

func renderClip(ctx context.Context, ffmpeg, workDir, source, background string, backgroundStart float64, localBackground bool, assName, output string) error {
	encoder, err := preferredVideoEncoder(ctx, ffmpeg, workDir)
	if err != nil {
		return err
	}
	graph := fmt.Sprintf(
		"[0:v]scale=1080:960:force_original_aspect_ratio=increase,crop=1080:960[top];"+
			"[1:v]scale=1080:960:force_original_aspect_ratio=increase,crop=1080:960[bottom];"+
			"[top][bottom]vstack[stack];[stack]ass=%s[out]",
		assName,
	)
	args := []string{"-y", "-loglevel", "error", "-i", source}
	if localBackground {
		args = append(args, "-ss", fmt.Sprintf("%.2f", backgroundStart), "-stream_loop", "-1", "-i", background)
	} else {
		args = append(args, "-stream_loop", "-1", "-i", background)
	}
	temporary := strings.TrimSuffix(output, ".mp4") + ".tmp.mp4"
	args = append(args,
		"-filter_complex_threads", strconv.Itoa(processingThreads()),
		"-filter_complex", graph,
		"-map", "[out]", "-map", "0:a?", "-shortest",
	)
	args = append(args, encoder.options...)
	args = append(args, "-c:a", "aac", temporary)
	if _, err := runCommand(ctx, workDir, ffmpeg, args...); err != nil {
		_ = os.Remove(temporary)
		if encoder.name != "libx264" {
			args = args[:len(args)-len(encoder.options)-3]
			args = append(args, softwareEncoderOptions()...)
			args = append(args, "-c:a", "aac", temporary)
			if _, fallbackErr := runCommand(ctx, workDir, ffmpeg, args...); fallbackErr == nil {
				encoder = videoEncoder{name: "libx264", options: softwareEncoderOptions()}
				renderEncoderCache.Store(ffmpeg, encoder)
			} else {
				_ = os.Remove(temporary)
				return fmt.Errorf("аппаратное кодирование завершилось ошибкой (%v); программное кодирование тоже не удалось: %w", err, fallbackErr)
			}
		} else {
			return fmt.Errorf("ошибка FFmpeg: %w", err)
		}
	}
	if err := os.Rename(temporary, output); err != nil {
		return fmt.Errorf("не удалось сохранить видео: %w", err)
	}
	return nil
}

func preferredVideoEncoder(ctx context.Context, ffmpeg, workDir string) (videoEncoder, error) {
	if cached, ok := renderEncoderCache.Load(ffmpeg); ok {
		return cached.(videoEncoder), nil
	}
	software := videoEncoder{name: "libx264", options: softwareEncoderOptions()}
	available, err := runCommand(ctx, workDir, ffmpeg, "-hide_banner", "-encoders")
	if err != nil {
		renderEncoderCache.Store(ffmpeg, software)
		return software, nil
	}
	for _, encoder := range []videoEncoder{
		{name: "h264_nvenc", options: []string{"-c:v", "h264_nvenc", "-preset", "p4", "-cq", "23", "-b:v", "0"}},
		{name: "h264_qsv", options: []string{"-c:v", "h264_qsv", "-preset", "faster", "-global_quality", "23"}},
		{name: "h264_amf", options: []string{"-c:v", "h264_amf", "-quality", "speed", "-qp_i", "23", "-qp_p", "23"}},
	} {
		if !strings.Contains(string(available), encoder.name) {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		args := []string{
			"-hide_banner", "-loglevel", "error",
			"-f", "lavfi", "-i", "color=c=black:s=64x64:r=1:d=1",
			"-frames:v", "1", "-an",
		}
		args = append(args, encoder.options...)
		args = append(args, "-f", "null", "-")
		_, probeErr := runCommand(probeCtx, workDir, ffmpeg, args...)
		cancel()
		if probeErr == nil {
			renderEncoderCache.Store(ffmpeg, encoder)
			return encoder, nil
		}
		if ctx.Err() != nil {
			return videoEncoder{}, ctx.Err()
		}
	}
	renderEncoderCache.Store(ffmpeg, software)
	return software, nil
}

func softwareEncoderOptions() []string {
	return []string{
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-crf", "21",
		"-threads:v", strconv.Itoa(processingThreads()),
	}
}

func processingThreads() int {
	return max(1, min(runtime.NumCPU()/2, 4))
}

func randomGameplayStart(ctx context.Context, ffprobe, path string, clipDuration float64) (float64, error) {
	output, err := runCommand(ctx, filepath.Dir(path), ffprobe,
		"-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path,
	)
	if err != nil {
		return 0, fmt.Errorf("ffprobe не смог прочитать длительность фона: %w", err)
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil || duration <= 0 {
		return 0, errors.New("у видео для фона неизвестна длительность")
	}
	margin := clipDuration + 5
	if duration > margin {
		return randomFloat(duration - margin), nil
	}
	return randomFloat(duration), nil
}

func nextClipIndex(workDir string) (int, error) {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return 0, fmt.Errorf("не удалось прочитать папку клипов: %w", err)
	}
	maxIndex := 0
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		for _, prefix := range []string{"raw", "clip"} {
			if strings.HasPrefix(name, prefix) {
				value := strings.TrimPrefix(name, prefix)
				index, err := strconv.Atoi(value)
				if err == nil && index > maxIndex {
					maxIndex = index
				}
				break
			}
		}
	}
	return maxIndex + 1, nil
}

func cleanupPartial(workDir string) error {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return fmt.Errorf("не удалось прочитать папку временных файлов: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		remove := strings.HasSuffix(name, ".part") ||
			strings.HasSuffix(name, ".ytdl") ||
			strings.HasSuffix(name, ".tmp.mp4") ||
			strings.HasPrefix(name, "audio_full.") ||
			strings.HasPrefix(name, "transcript") ||
			regexp.MustCompile(`^c\d+\.ass$`).MatchString(name) ||
			regexp.MustCompile(`^gp\d+\..+`).MatchString(name)
		if remove {
			if err := os.Remove(filepath.Join(workDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("не удалось удалить временный файл %q: %w", name, err)
			}
			continue
		}
		if strings.HasPrefix(name, "raw") {
			index, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSuffix(name, filepath.Ext(name)), "raw"))
			if err == nil && !fileExists(filepath.Join(workDir, fmt.Sprintf("clip%d.mp4", index))) {
				if err := os.Remove(filepath.Join(workDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("не удалось удалить незаконченный фрагмент %q: %w", name, err)
				}
			}
		}
	}
	return nil
}

func appendErrorLog(workDir string, err error) error {
	path := filepath.Join(workDir, "error.log")
	file, openErr := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if openErr != nil {
		return fmt.Errorf("не удалось открыть %q: %w", path, openErr)
	}
	defer file.Close()
	if _, err := fmt.Fprintf(file, "%s %v\n", time.Now().Format(time.RFC3339), err); err != nil {
		return fmt.Errorf("не удалось записать %q: %w", path, err)
	}
	return nil
}

func shortError(err error) string {
	message := strings.TrimSpace(err.Error())
	if len(message) > 260 {
		return message[:260] + "…"
	}
	return message
}

func formatTime(seconds float64) string {
	value := int(seconds)
	if value >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", value/3600, value%3600/60, value%60)
	}
	return fmt.Sprintf("%d:%02d", value/60, value%60)
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func randomFloat(maximum float64) float64 {
	if maximum <= 0 {
		return 0
	}
	return rand.Float64() * maximum
}
