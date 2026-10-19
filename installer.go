package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
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
	"unicode/utf16"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows/registry"
)

//go:embed installer.html
var installerHTML string

const (
	whisperArchiveURL = "https://github.com/ggml-org/whisper.cpp/releases/download/b5130/whisper-bin-x64.zip"
	whisperArchiveSHA = "f9ec6c52a2e949b62ab51fa21d0d497958f9e41c3010c157c4e42932d5316f3c"
	whisperModelURL   = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-small-q5_1.bin"
	whisperModelSHA   = "ae85e4a935d7a567bd102fe55afc16bb595bdb618e11b2fc7591bc08120411bb"
	whisperModelSize  = int64(190085487)
	webView2SetupURL  = "https://go.microsoft.com/fwlink/p/?LinkId=2124703"
	clipperReleaseURL = "https://api.github.com/repos/karvikk/clipper/releases/latest"
)

type installerAsset struct {
	name string
	url  string
	hash string
	size int64
}

type releaseAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

type githubRelease struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

type latestClipperRelease struct {
	Version   string
	Installer installerAsset
}

type installerState struct {
	Installed        bool   `json:"installed"`
	InstalledVersion string `json:"installedVersion"`
	LatestVersion    string `json:"latestVersion"`
	CurrentVersion   string `json:"currentVersion"`
	UpdateAvailable  bool   `json:"updateAvailable"`
}

type installerEvent struct {
	Message  string  `json:"message"`
	Progress float64 `json:"progress"`
	Done     bool    `json:"done,omitempty"`
	Error    bool    `json:"error,omitempty"`
}

type installerApp struct {
	window webview2.WebView
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	busy   bool
	closed bool
}

func isSetupExecutable() bool {
	executable, err := os.Executable()
	return err == nil && strings.EqualFold(filepath.Base(executable), "ClipperSetup.exe")
}

func runInstaller() error {
	runtimeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := ensureWebView2Runtime(runtimeCtx); err != nil {
		return fmt.Errorf("не удалось подготовить Microsoft Edge WebView2 Runtime: %w", err)
	}
	window := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  "Установка Clipper",
			Width:  760,
			Height: 560,
			Center: true,
		},
	})
	if window == nil {
		return errors.New("не удалось открыть окно установщика. Проверьте, установлен ли Microsoft Edge WebView2 Runtime, и перезапустите ClipperSetup.exe.")
	}
	defer window.Destroy()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app := &installerApp{window: window, ctx: ctx, cancel: cancel}
	bindings := []struct {
		name string
		fn   any
	}{
		{"getInstallerState", app.getInstallerState},
		{"startInstall", app.startInstall},
		{"launchInstalledClipper", app.launchInstalledClipper},
		{"closeInstaller", app.closeInstaller},
		{"beginWindowDrag", beginWindowDrag},
		{"minimizeWindow", minimizeWindow},
		{"toggleMaximizeWindow", toggleMaximizeWindow},
	}
	for _, binding := range bindings {
		if err := window.Bind(binding.name, binding.fn); err != nil {
			return fmt.Errorf("не удалось подключить установщик: %w", err)
		}
	}
	window.SetSize(760, 560, webview2.HintMin)
	if err := applyBorderlessWindow(window.Window()); err != nil {
		return fmt.Errorf("не удалось создать окно установщика без рамки: %w", err)
	}
	window.SetHtml(installerHTML)
	window.Run()
	return nil
}

func (a *installerApp) getInstallerState() (string, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	release, err := getLatestClipperRelease(ctx)
	if err != nil {
		return "", err
	}
	installDir, err := clipperInstallDir()
	if err != nil {
		return "", err
	}
	installedVersion, err := readInstalledVersion(installDir)
	if err != nil {
		return "", err
	}
	state := installerState{
		Installed:        fileExists(filepath.Join(installDir, "Clipper.exe")),
		InstalledVersion: installedVersion,
		LatestVersion:    release.Version,
		CurrentVersion:   appVersion,
		UpdateAvailable:  installedVersion == "" || versionCompare(release.Version, installedVersion) > 0,
	}
	data, err := json.Marshal(state)
	if err != nil {
		return "", fmt.Errorf("не удалось подготовить сведения об обновлениях: %w", err)
	}
	return string(data), nil
}

func ensureWebView2Runtime(ctx context.Context) error {
	if hasWebView2Runtime() {
		return nil
	}
	tempDir, err := os.MkdirTemp("", "clipper-webview2-*")
	if err != nil {
		return fmt.Errorf("не удалось создать временную папку: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			log.Printf("Не удалось очистить временный установщик WebView2 %q: %v", tempDir, err)
		}
	}()
	setupPath := filepath.Join(tempDir, "MicrosoftEdgeWebView2Setup.exe")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, webView2SetupURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "Clipper-Setup")
	response, err := installerHTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("не удалось скачать установщик WebView2: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("сервер Microsoft ответил HTTP %d", response.StatusCode)
	}
	file, err := os.OpenFile(setupPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return fmt.Errorf("не удалось сохранить установщик WebView2: %w", err)
	}
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, 32<<20))
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("не удалось скачать установщик WebView2: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("не удалось сохранить установщик WebView2: %w", closeErr)
	}
	if written < 1024 || written >= 32<<20 {
		return fmt.Errorf("получен некорректный установщик WebView2 (%d байт)", written)
	}
	script := "$ErrorActionPreference = 'Stop'; " +
		"$setup = " + quotePowerShell(setupPath) + "; " +
		"$signature = Get-AuthenticodeSignature -LiteralPath $setup; " +
		"if ($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Subject -notmatch 'Microsoft') { throw 'Подпись Microsoft у установщика WebView2 недействительна.' }; " +
		"$process = Start-Process -FilePath $setup -ArgumentList '/silent','/install' -Wait -PassThru; " +
		"if ($process.ExitCode -ne 0 -and $process.ExitCode -ne 3010) { throw \"Установщик WebView2 завершился с кодом $($process.ExitCode).\" }"
	command := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", encodePowerShell(script))
	hideConsoleWindow(command)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("проверка или установка WebView2 не удалась: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if !hasWebView2Runtime() {
		return errors.New("установщик завершился, но Microsoft Edge WebView2 Runtime не обнаружен")
	}
	return nil
}

func hasWebView2Runtime() bool {
	roots := []struct {
		root registry.Key
		view uint32
	}{
		{registry.LOCAL_MACHINE, registry.WOW64_64KEY},
		{registry.LOCAL_MACHINE, registry.WOW64_32KEY},
		{registry.CURRENT_USER, registry.WOW64_64KEY},
		{registry.CURRENT_USER, registry.WOW64_32KEY},
	}
	const clientsPath = `SOFTWARE\Microsoft\EdgeUpdate\Clients`
	for _, location := range roots {
		clients, err := registry.OpenKey(location.root, clientsPath, registry.READ|location.view)
		if err != nil {
			continue
		}
		names, err := clients.ReadSubKeyNames(-1)
		_ = clients.Close()
		if err != nil {
			continue
		}
		for _, name := range names {
			client, err := registry.OpenKey(location.root, clientsPath+`\`+name, registry.QUERY_VALUE|location.view)
			if err != nil {
				continue
			}
			displayName, _, nameErr := client.GetStringValue("name")
			version, _, versionErr := client.GetStringValue("pv")
			_ = client.Close()
			if nameErr == nil && versionErr == nil &&
				strings.Contains(strings.ToLower(displayName), "webview2") &&
				version != "" && version != "0.0.0.0" {
				return true
			}
		}
	}
	return false
}

func (a *installerApp) startInstall() error {
	if runtime.GOARCH != "amd64" {
		return errors.New("установщик Clipper рассчитан на Windows x64")
	}
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		return errors.New("установка уже выполняется")
	}
	a.busy = true
	a.mu.Unlock()
	a.emit(installerEvent{Message: "Подключаюсь к официальным источникам…", Progress: 1})
	go func() {
		alreadyCurrent, err := installOrUpdateClipper(a.ctx, func(message string, progress float64) {
			a.emit(installerEvent{Message: message, Progress: progress})
		})
		a.mu.Lock()
		a.busy = false
		a.mu.Unlock()
		if err != nil {
			a.emit(installerEvent{Message: err.Error(), Error: true})
			return
		}
		message := "Clipper установлен или обновлён. Все загруженные компоненты проверены по SHA-256."
		if alreadyCurrent {
			message = "Установлена последняя версия Clipper."
		}
		a.emit(installerEvent{Message: message, Progress: 100, Done: true})
	}()
	return nil
}

func (a *installerApp) launchInstalledClipper() error {
	if err := startInstalledClipper(); err != nil {
		return err
	}
	return a.closeInstaller()
}

func (a *installerApp) closeInstaller() error {
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
	a.cancel()
	return closeWindow()
}

func (a *installerApp) emit(event installerEvent) {
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if closed {
		return
	}
	data, err := json.Marshal(event)
	if err != nil {
		log.Printf("Не удалось подготовить сообщение установщика: %v", err)
		return
	}
	a.window.Dispatch(func() {
		a.window.Eval("window.onInstallerEvent(" + string(data) + ")")
	})
}

func installOrUpdateClipper(ctx context.Context, progress func(string, float64)) (bool, error) {
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		return false, errors.New("установка поддерживает только Windows x64")
	}
	release, err := getLatestClipperRelease(ctx)
	if err != nil {
		return false, fmt.Errorf("не удалось проверить последнюю версию Clipper: %w", err)
	}
	installDir, err := clipperInstallDir()
	if err != nil {
		return false, err
	}
	installedExecutable := filepath.Join(installDir, "Clipper.exe")
	installed := fileExists(installedExecutable)
	installedVersion, err := readInstalledVersion(installDir)
	if err != nil {
		return false, err
	}
	if installed && installedVersion != "" &&
		versionCompare(installedVersion, release.Version) >= 0 &&
		installedComponentsReady(installDir) {
		return true, nil
	}
	targetVersion := release.Version
	if versionCompare(appVersion, targetVersion) > 0 {
		targetVersion = appVersion
	}

	tempDir, err := os.MkdirTemp("", "clipper-update-check-*")
	if err != nil {
		return false, fmt.Errorf("не удалось создать временную папку обновления: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			log.Printf("Не удалось очистить временные файлы обновления %q: %v", tempDir, err)
		}
	}()
	sourceExecutable, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("не удалось определить файл установщика: %w", err)
	}
	if versionCompare(appVersion, release.Version) < 0 {
		sourceExecutable = filepath.Join(tempDir, "ClipperSetup.exe")
		progress("Найдена новая версия установщика "+release.Version+"; скачиваю её…", 4)
		if err := downloadVerified(ctx, release.Installer, sourceExecutable, func(received, total int64) {
			fraction := float64(0)
			if total > 0 {
				fraction = float64(received) / float64(total)
			}
			progress("Скачиваю Clipper ("+formatBytes(received)+")…", 4+fraction*15)
		}); err != nil {
			return false, fmt.Errorf("не удалось скачать актуальную версию Clipper: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if installed && installedComponentsReady(installDir) {
		progress("Обновляю только приложение; инструменты, ключ и клипы сохраняются…", 75)
		if err := copyFile(sourceExecutable, installedExecutable, 0o755); err != nil {
			return false, fmt.Errorf("не удалось заменить приложение. Закройте Clipper, если он запущен, и повторите: %w", err)
		}
		if err := writeInstalledVersion(installDir, targetVersion); err != nil {
			return false, fmt.Errorf("приложение обновлено, но не удалось сохранить номер версии: %w", err)
		}
		progress("Обновление завершено.", 100)
		return false, nil
	}
	progress("Нужна установка или восстановление компонентов; подготавливаю полный комплект…", 20)
	if err := installClipper(ctx, sourceExecutable, targetVersion, progress); err != nil {
		return false, err
	}
	return false, nil
}

func clipperInstallDir() (string, error) {
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		return "", errors.New("Windows не сообщил путь LOCALAPPDATA")
	}
	return filepath.Join(localAppData, "Programs", "Clipper"), nil
}

func readInstalledVersion(installDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(installDir, "version.txt"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("не удалось прочитать версию установленного Clipper: %w", err)
	}
	version := strings.TrimSpace(string(data))
	if version != "" && !validReleaseVersion(version) {
		return "", fmt.Errorf("файл версии установленного Clipper содержит неверное значение %q", version)
	}
	return version, nil
}

func writeInstalledVersion(installDir, version string) error {
	if !validReleaseVersion(version) {
		return fmt.Errorf("неверный номер версии Clipper %q", version)
	}
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return err
	}
	return writeFileAtomically(filepath.Join(installDir, "version.txt"), []byte(version+"\n"), 0o644)
}

func installedComponentsReady(installDir string) bool {
	for _, name := range []string{"ffmpeg.exe", "ffprobe.exe", "yt-dlp.exe", "deno.exe"} {
		if !fileExists(filepath.Join(installDir, "tools", name)) {
			return false
		}
	}
	if !validWhisperExecutable(filepath.Join(installDir, "tools", "whisper-cli.exe")) {
		return false
	}
	model, err := os.UserConfigDir()
	if err != nil {
		return false
	}
	return validInstalledWhisperModel(filepath.Join(model, "Clipper", "models", "ggml-small-q5_1.bin"))
}

func installClipper(ctx context.Context, executable, version string, progress func(string, float64)) error {
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		return errors.New("установка поддерживает только Windows x64")
	}
	assets, err := resolveInstallerAssets(ctx)
	if err != nil {
		return fmt.Errorf("не удалось получить официальные файлы для установки: %w", err)
	}
	tempDir, err := os.MkdirTemp("", "clipper-setup-*")
	if err != nil {
		return fmt.Errorf("не удалось создать временную папку установщика: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			log.Printf("Не удалось очистить временные файлы установщика %q: %v", tempDir, err)
		}
	}()

	downloadsDir := filepath.Join(tempDir, "downloads")
	stageTools := filepath.Join(tempDir, "tools")
	if err := os.MkdirAll(downloadsDir, 0o700); err != nil {
		return fmt.Errorf("не удалось подготовить временную папку загрузок: %w", err)
	}
	if err := os.MkdirAll(stageTools, 0o700); err != nil {
		return fmt.Errorf("не удалось подготовить компоненты Clipper: %w", err)
	}

	for index, asset := range assets {
		destination := filepath.Join(downloadsDir, asset.name)
		progress(fmt.Sprintf("Скачиваю и проверяю %s…", asset.name), float64(index)/float64(len(assets))*45)
		err := downloadVerified(ctx, asset, destination, func(received, total int64) {
			fraction := float64(0)
			if total > 0 {
				fraction = float64(received) / float64(total)
			}
			progress(fmt.Sprintf("Скачиваю %s (%s)…", asset.name, formatBytes(received)), (float64(index)+fraction)/float64(len(assets))*45)
		})
		if err != nil {
			return fmt.Errorf("не удалось скачать %s: %w", asset.name, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		progress("Проверка целостности завершена: "+asset.name, float64(index+1)/float64(len(assets))*45)
	}

	progress("Распаковываю FFmpeg…", 48)
	if err := extractZipFiles(filepath.Join(downloadsDir, "ffmpeg.zip"), stageTools, func(entry string) bool {
		clean := strings.ToLower(filepath.ToSlash(entry))
		if !strings.Contains(clean, "/bin/") {
			return false
		}
		name := strings.ToLower(filepath.Base(clean))
		return name == "ffmpeg.exe" || name == "ffprobe.exe" || strings.HasSuffix(name, ".dll")
	}); err != nil {
		return fmt.Errorf("не удалось распаковать FFmpeg: %w", err)
	}

	progress("Распаковываю Whisper и Deno…", 54)
	if err := extractZipFiles(filepath.Join(downloadsDir, "whisper.zip"), stageTools, func(entry string) bool {
		clean := strings.ToLower(filepath.ToSlash(entry))
		if !strings.HasPrefix(clean, "release/") {
			return false
		}
		name := strings.ToLower(filepath.Base(clean))
		return name == "whisper-cli.exe" || strings.HasSuffix(name, ".dll")
	}); err != nil {
		return fmt.Errorf("не удалось распаковать Whisper: %w", err)
	}
	if err := extractZipFiles(filepath.Join(downloadsDir, "deno.zip"), stageTools, func(entry string) bool {
		return strings.EqualFold(filepath.Base(entry), "deno.exe")
	}); err != nil {
		return fmt.Errorf("не удалось распаковать Deno: %w", err)
	}
	if !fileExists(filepath.Join(stageTools, "ffmpeg.exe")) ||
		!fileExists(filepath.Join(stageTools, "ffprobe.exe")) ||
		!fileExists(filepath.Join(stageTools, "whisper-cli.exe")) ||
		!fileExists(filepath.Join(stageTools, "deno.exe")) {
		return errors.New("официальные архивы не содержат все необходимые программы или библиотеки")
	}
	if err := copyFile(filepath.Join(downloadsDir, "yt-dlp.exe"), filepath.Join(stageTools, "yt-dlp.exe"), 0o755); err != nil {
		return fmt.Errorf("не удалось подготовить yt-dlp: %w", err)
	}

	installDir, err := clipperInstallDir()
	if err != nil {
		return err
	}
	modelDir, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("не удалось определить папку модели Whisper: %w", err)
	}
	modelDir = filepath.Join(modelDir, "Clipper", "models")
	if err := os.MkdirAll(filepath.Join(installDir, "tools"), 0o755); err != nil {
		return fmt.Errorf("не удалось создать папку программы (закройте запущенный Clipper): %w", err)
	}
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		return fmt.Errorf("не удалось создать папку модели Whisper: %w", err)
	}

	progress("Устанавливаю программы и сохраняю настройки пользователя…", 65)
	if err := copyFile(executable, filepath.Join(installDir, "Clipper.exe"), 0o755); err != nil {
		return fmt.Errorf("не удалось установить Clipper (если он уже открыт, закройте его и повторите): %w", err)
	}
	if err := copyDirectoryFiles(stageTools, filepath.Join(installDir, "tools")); err != nil {
		return fmt.Errorf("не удалось установить инструменты: %w", err)
	}
	if err := copyFile(filepath.Join(downloadsDir, "ggml-small-q5_1.bin"), filepath.Join(modelDir, "ggml-small-q5_1.bin"), 0o644); err != nil {
		return fmt.Errorf("не удалось установить модель Whisper: %w", err)
	}
	if err := writeInstalledVersion(installDir, version); err != nil {
		return fmt.Errorf("не удалось сохранить версию Clipper: %w", err)
	}
	if err := createClipperShortcuts(installDir); err != nil {
		return fmt.Errorf("файлы установлены, но не удалось создать ярлыки: %w", err)
	}
	progress("Установка завершена.", 100)
	return nil
}

func ensureWhisperComponents(ctx context.Context, progress func(string, float64)) error {
	appDir, err := executableDir()
	if err != nil {
		return err
	}
	whisperPath := findTool(appDir, []string{"whisper-cli.exe", "whisper-cli", "main.exe"}, []string{"whisper-cli.exe", "whisper-cli", "main.exe"})
	whisperValid := validWhisperExecutable(whisperPath)
	modelPath := findWhisperModel(appDir)
	modelValid := modelPath != "" && validInstalledWhisperModel(modelPath)
	if whisperValid && modelValid {
		return nil
	}

	assets := make([]installerAsset, 0, 2)
	if !whisperValid {
		assets = append(assets, installerAsset{name: "whisper.zip", url: whisperArchiveURL, hash: whisperArchiveSHA})
	}
	if !modelValid {
		assets = append(assets, installerAsset{
			name: "ggml-small-q5_1.bin", url: whisperModelURL, hash: whisperModelSHA, size: whisperModelSize,
		})
	}
	tempDir, err := os.MkdirTemp("", "clipper-whisper-repair-*")
	if err != nil {
		return fmt.Errorf("не удалось создать временную папку для Whisper: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			log.Printf("Не удалось очистить временные файлы Whisper %q: %v", tempDir, err)
		}
	}()
	toolsStage := filepath.Join(tempDir, "tools")
	if err := os.MkdirAll(toolsStage, 0o700); err != nil {
		return fmt.Errorf("не удалось подготовить восстановление Whisper: %w", err)
	}
	for index, asset := range assets {
		progress(fmt.Sprintf("Скачиваю компонент Whisper: %s…", asset.name), float64(index)/float64(len(assets))*70)
		destination := filepath.Join(tempDir, asset.name)
		err := downloadVerified(ctx, asset, destination, func(received, total int64) {
			fraction := float64(0)
			if total > 0 {
				fraction = float64(received) / float64(total)
			}
			progress(fmt.Sprintf("Скачиваю Whisper (%s)…", formatBytes(received)), (float64(index)+fraction)/float64(len(assets))*70)
		})
		if err != nil {
			return fmt.Errorf("не удалось скачать проверенный компонент %s: %w", asset.name, err)
		}
	}
	if !whisperValid {
		progress("Распаковываю Whisper…", 76)
		if err := extractZipFiles(filepath.Join(tempDir, "whisper.zip"), toolsStage, func(entry string) bool {
			clean := strings.ToLower(filepath.ToSlash(entry))
			if !strings.HasPrefix(clean, "release/") {
				return false
			}
			name := strings.ToLower(filepath.Base(clean))
			return name == "whisper-cli.exe" || strings.HasSuffix(name, ".dll")
		}); err != nil {
			return fmt.Errorf("не удалось распаковать Whisper: %w", err)
		}
		if !fileExists(filepath.Join(toolsStage, "whisper-cli.exe")) {
			return errors.New("проверенный архив Whisper не содержит whisper-cli.exe")
		}
		if err := copyDirectoryFiles(toolsStage, filepath.Join(appDir, "tools")); err != nil {
			return fmt.Errorf("не удалось восстановить Whisper в папке приложения: %w", err)
		}
	}
	if !modelValid {
		modelDir, err := os.UserConfigDir()
		if err != nil {
			return fmt.Errorf("не удалось определить папку модели Whisper: %w", err)
		}
		modelDir = filepath.Join(modelDir, "Clipper", "models")
		if err := copyFile(filepath.Join(tempDir, "ggml-small-q5_1.bin"), filepath.Join(modelDir, "ggml-small-q5_1.bin"), 0o644); err != nil {
			return fmt.Errorf("не удалось восстановить модель Whisper: %w", err)
		}
	}
	progress("Whisper и модель проверены; можно продолжать обработку.", 100)
	return nil
}

func validWhisperExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() >= 1<<20
}

func validInstalledWhisperModel(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if filepath.Base(path) != "ggml-small-q5_1.bin" {
		return info.Size() >= 100<<20
	}
	if info.Size() != whisperModelSize {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return false
	}
	return hex.EncodeToString(hasher.Sum(nil)) == whisperModelSHA
}

func resolveInstallerAssets(ctx context.Context) ([]installerAsset, error) {
	ffmpeg, err := getReleaseAsset(ctx, "https://api.github.com/repos/BtbN/FFmpeg-Builds/releases/latest", func(name string) bool {
		return strings.Contains(name, "ffmpeg-n") &&
			strings.Contains(name, "latest-win64-gpl-shared") &&
			strings.HasSuffix(strings.ToLower(name), ".zip")
	}, "ffmpeg.zip")
	if err != nil {
		return nil, fmt.Errorf("не удалось определить актуальный архив FFmpeg: %w", err)
	}
	ytdlp, err := getReleaseAsset(ctx, "https://api.github.com/repos/yt-dlp/yt-dlp/releases/latest", func(name string) bool {
		return name == "yt-dlp.exe"
	}, "yt-dlp.exe")
	if err != nil {
		return nil, fmt.Errorf("не удалось определить актуальный yt-dlp: %w", err)
	}
	deno, err := getReleaseAsset(ctx, "https://api.github.com/repos/denoland/deno/releases/latest", func(name string) bool {
		return name == "deno-x86_64-pc-windows-msvc.zip"
	}, "deno.zip")
	if err != nil {
		return nil, fmt.Errorf("не удалось определить актуальный Deno: %w", err)
	}
	model := installerAsset{
		name: "ggml-small-q5_1.bin", url: whisperModelURL, hash: whisperModelSHA,
	}
	whisper := installerAsset{
		name: "whisper.zip", url: whisperArchiveURL, hash: whisperArchiveSHA,
	}
	return []installerAsset{whisper, ffmpeg, ytdlp, deno, model}, nil
}

func getLatestClipperRelease(ctx context.Context) (latestClipperRelease, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, clipperReleaseURL, nil)
	if err != nil {
		return latestClipperRelease{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "Clipper-Installer")
	response, err := installerHTTPClient.Do(request)
	if err != nil {
		return latestClipperRelease{}, fmt.Errorf("проверьте подключение к интернету: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return latestClipperRelease{}, fmt.Errorf("GitHub Releases ответил HTTP %d", response.StatusCode)
	}
	var release githubRelease
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&release); err != nil {
		return latestClipperRelease{}, fmt.Errorf("не удалось прочитать последний релиз Clipper: %w", err)
	}
	return parseLatestClipperRelease(release)
}

func parseLatestClipperRelease(release githubRelease) (latestClipperRelease, error) {
	if !validReleaseVersion(release.TagName) {
		return latestClipperRelease{}, fmt.Errorf("GitHub вернул неверную версию Clipper %q", release.TagName)
	}
	for _, asset := range release.Assets {
		if asset.Name != "ClipperSetup.exe" {
			continue
		}
		const prefix = "sha256:"
		if !strings.HasPrefix(asset.Digest, prefix) || !validSHA256(strings.TrimPrefix(asset.Digest, prefix)) ||
			asset.URL == "" || asset.Size <= 0 {
			return latestClipperRelease{}, errors.New("GitHub вернул неверные контрольную сумму или размер ClipperSetup.exe")
		}
		return latestClipperRelease{
			Version: release.TagName,
			Installer: installerAsset{
				name: "ClipperSetup.exe",
				url:  asset.URL,
				hash: strings.TrimPrefix(asset.Digest, prefix),
				size: asset.Size,
			},
		}, nil
	}
	return latestClipperRelease{}, errors.New("в последнем релизе GitHub не найден ClipperSetup.exe")
}

func getReleaseAsset(ctx context.Context, releaseURL string, match func(string) bool, name string) (installerAsset, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseURL, nil)
	if err != nil {
		return installerAsset{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "Clipper-Setup")
	response, err := installerHTTPClient.Do(request)
	if err != nil {
		return installerAsset{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return installerAsset{}, fmt.Errorf("GitHub API ответил HTTP %d", response.StatusCode)
	}
	var release githubRelease
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&release); err != nil {
		return installerAsset{}, fmt.Errorf("не удалось прочитать список официальных файлов: %w", err)
	}
	candidates := make([]releaseAsset, 0, len(release.Assets))
	for _, asset := range release.Assets {
		if match(asset.Name) {
			candidates = append(candidates, asset)
		}
	}
	if len(candidates) == 0 {
		return installerAsset{}, errors.New("подходящий файл не найден в последнем релизе")
	}
	if strings.Contains(releaseURL, "BtbN/FFmpeg-Builds") {
		sort.Slice(candidates, func(i, j int) bool {
			return versionGreater(ffmpegVersion(candidates[i].Name), ffmpegVersion(candidates[j].Name))
		})
	}
	selected := candidates[0]
	const digestPrefix = "sha256:"
	if !strings.HasPrefix(selected.Digest, digestPrefix) {
		return installerAsset{}, fmt.Errorf("GitHub не предоставил SHA-256 для %s", selected.Name)
	}
	digest := strings.TrimPrefix(selected.Digest, digestPrefix)
	if !validSHA256(digest) || selected.URL == "" || selected.Size <= 0 {
		return installerAsset{}, fmt.Errorf("GitHub предоставил некорректные сведения о %s", selected.Name)
	}
	return installerAsset{name: name, url: selected.URL, hash: digest, size: selected.Size}, nil
}

func downloadVerified(ctx context.Context, asset installerAsset, destination string, progress func(int64, int64)) error {
	if !validSHA256(asset.hash) {
		return errors.New("неверный SHA-256 в манифесте установки")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "Clipper-Setup")
	response, err := installerHTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("сервер загрузки ответил HTTP %d", response.StatusCode)
	}
	if asset.size > 0 && response.ContentLength >= 0 && response.ContentLength != asset.size {
		return fmt.Errorf("размер файла не совпадает с манифестом: %d вместо %d байт", response.ContentLength, asset.size)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	file, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer file.Close()
	hasher := sha256.New()
	written, err := copyWithProgress(io.MultiWriter(file, hasher), response.Body, response.ContentLength, progress)
	if err != nil {
		return err
	}
	if asset.size > 0 && written != asset.size {
		return fmt.Errorf("получен файл неполного размера: %d вместо %d байт", written, asset.size)
	}
	actualHash := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(actualHash, asset.hash) {
		return fmt.Errorf("SHA-256 не совпадает: получен %s", actualHash)
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return file.Close()
}

var installerHTTPClient = &http.Client{
	Timeout: 30 * time.Minute,
	CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("слишком много перенаправлений при загрузке")
		}
		if request.URL.Scheme != "https" {
			return errors.New("перенаправление загрузки на незащищённое соединение запрещено")
		}
		return nil
	},
}

func copyWithProgress(destination io.Writer, source io.Reader, total int64, progress func(int64, int64)) (int64, error) {
	buffer := make([]byte, 128*1024)
	var written int64
	for {
		count, readErr := source.Read(buffer)
		if count > 0 {
			n, writeErr := destination.Write(buffer[:count])
			written += int64(n)
			if writeErr != nil {
				return written, writeErr
			}
			if n != count {
				return written, io.ErrShortWrite
			}
			progress(written, total)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return written, nil
			}
			return written, readErr
		}
	}
}

func extractZipFiles(archivePath, destination string, selectFile func(string) bool) error {
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	extracted := 0
	for _, entry := range archive.File {
		if entry.FileInfo().IsDir() || !selectFile(entry.Name) {
			continue
		}
		name := filepath.ToSlash(entry.Name)
		if strings.HasPrefix(name, "/") || strings.Contains(name, ":") {
			return fmt.Errorf("небезопасный путь в архиве: %q", entry.Name)
		}
		clean := filepath.Clean(filepath.FromSlash(name))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("небезопасный путь в архиве: %q", entry.Name)
		}
		if entry.UncompressedSize64 > 1<<30 {
			return fmt.Errorf("файл в архиве превышает допустимый размер: %q", entry.Name)
		}
		target := filepath.Join(destination, filepath.Base(clean))
		source, err := entry.Open()
		if err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			source.Close()
			return err
		}
		written, copyErr := io.Copy(output, source)
		sourceCloseErr := source.Close()
		outputCloseErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if sourceCloseErr != nil {
			return sourceCloseErr
		}
		if outputCloseErr != nil {
			return outputCloseErr
		}
		if uint64(written) != entry.UncompressedSize64 {
			return fmt.Errorf("файл в архиве распакован не полностью: %q", entry.Name)
		}
		extracted++
	}
	if extracted == 0 {
		return errors.New("архив не содержит ожидаемых файлов")
	}
	return nil
}

func copyDirectoryFiles(sourceDir, destinationDir string) error {
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		mode := os.FileMode(0o644)
		if strings.EqualFold(filepath.Ext(entry.Name()), ".exe") {
			mode = 0o755
		}
		if err := copyFile(filepath.Join(sourceDir, entry.Name()), filepath.Join(destinationDir, entry.Name()), mode); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(sourcePath, destinationPath string, mode os.FileMode) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(destinationPath), filepath.Base(destinationPath)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := io.Copy(temp, source); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return replaceFile(tempPath, destinationPath)
}

func createClipperShortcuts(installDir string) error {
	executable := filepath.Join(installDir, "Clipper.exe")
	startMenu := filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs", "Clipper")
	if os.Getenv("APPDATA") == "" {
		return errors.New("Windows не сообщил путь APPDATA")
	}
	if err := os.MkdirAll(startMenu, 0o755); err != nil {
		return err
	}
	desktop := "$desktop = [Environment]::GetFolderPath('Desktop')\n"
	script := "$ErrorActionPreference = 'Stop'\ntry {\n" + desktop +
		"$targets = @((Join-Path $desktop 'Clipper.lnk'), " + quotePowerShell(filepath.Join(startMenu, "Clipper.lnk")) + ")\n" +
		"$shell = New-Object -ComObject WScript.Shell\n" +
		"foreach ($path in $targets) { $shortcut = $shell.CreateShortcut($path); $shortcut.TargetPath = " + quotePowerShell(executable) + "; $shortcut.WorkingDirectory = " + quotePowerShell(installDir) + "; $shortcut.IconLocation = " + quotePowerShell(executable+",0") + "; $shortcut.Save() }\n" +
		"} catch { Write-Error $_; exit 1 }\n"
	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", encodePowerShell(script))
	hideConsoleWindow(command)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func startInstalledClipper() error {
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		return errors.New("Windows не сообщил путь LOCALAPPDATA")
	}
	executable := filepath.Join(localAppData, "Programs", "Clipper", "Clipper.exe")
	if !fileExists(executable) {
		return errors.New("сначала завершите установку Clipper")
	}
	if err := exec.Command(executable).Start(); err != nil {
		return fmt.Errorf("не удалось запустить Clipper: %w", err)
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func ffmpegVersion(name string) []int {
	match := regexp.MustCompile(`ffmpeg-n([0-9]+(?:\.[0-9]+)*)-latest`).FindStringSubmatch(name)
	if len(match) < 2 {
		return nil
	}
	parts := strings.Split(match[1], ".")
	version := make([]int, 0, len(parts))
	for _, part := range parts {
		value, _ := strconv.Atoi(part)
		version = append(version, value)
	}
	return version
}

func versionGreater(left, right []int) bool {
	for index := 0; index < max(len(left), len(right)); index++ {
		leftPart, rightPart := 0, 0
		if index < len(left) {
			leftPart = left[index]
		}
		if index < len(right) {
			rightPart = right[index]
		}
		if leftPart != rightPart {
			return leftPart > rightPart
		}
	}
	return false
}

func validReleaseVersion(version string) bool {
	return len(releaseVersionParts(version)) == 3
}

func releaseVersionParts(version string) []int {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return nil
	}
	result := make([]int, len(parts))
	for index, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return nil
		}
		result[index] = value
	}
	return result
}

func versionCompare(left, right string) int {
	leftParts, rightParts := releaseVersionParts(left), releaseVersionParts(right)
	if leftParts == nil {
		if rightParts == nil {
			return 0
		}
		return -1
	}
	if rightParts == nil {
		return 1
	}
	for index := range leftParts {
		if leftParts[index] < rightParts[index] {
			return -1
		}
		if leftParts[index] > rightParts[index] {
			return 1
		}
	}
	return 0
}

func quotePowerShell(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func encodePowerShell(script string) string {
	encoded := utf16.Encode([]rune(script))
	data := make([]byte, len(encoded)*2)
	for index, value := range encoded {
		data[index*2] = byte(value)
		data[index*2+1] = byte(value >> 8)
	}
	return base64.StdEncoding.EncodeToString(data)
}

func formatBytes(value int64) string {
	if value < 0 {
		value = 0
	}
	const megabyte = 1024 * 1024
	return fmt.Sprintf("%.1f МБ", float64(value)/megabyte)
}
