package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

//go:embed ui.html
var appHTML string

type StartRequest struct {
	URL          string `json:"url"`
	ClipCount    int    `json:"clipCount"`
	ClipDuration int    `json:"clipDuration"`
	GameplayURL  string `json:"gameplayUrl"`
	GameplayPath string `json:"gameplayPath"`
}

type AppState struct {
	Version      string `json:"version"`
	WorkDir      string `json:"workDir"`
	GameplayURL  string `json:"gameplayUrl"`
	GameplayPath string `json:"gameplayPath"`
	LicenseKey   string `json:"-"`
	ClipCount    int    `json:"clipCount"`
	ClipDuration int    `json:"clipDuration"`
}

type UIEvent struct {
	Type      string `json:"type"`
	Message   string `json:"message"`
	Current   int    `json:"current,omitempty"`
	Total     int    `json:"total,omitempty"`
	File      string `json:"file,omitempty"`
	Remaining *int   `json:"remaining,omitempty"`
}

type App struct {
	window    webview2.WebView
	mu        sync.Mutex
	cancel    context.CancelFunc
	jobDone   chan struct{}
	alive     bool
	config    AppState
	historyID string
}

func main() {
	if err := run(); err != nil {
		showStartupError(err.Error())
	}
}

func run() error {
	if runtime.GOOS != "windows" {
		return errors.New("Clipper desktop currently supports Windows only.")
	}
	if isSetupExecutable() {
		return runInstaller()
	}

	window := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  "Clipper — умная нарезка видео",
			Width:  1180,
			Height: 820,
			Center: true,
		},
	})
	if window == nil {
		return errors.New("не удалось создать окно. Установите Microsoft Edge WebView2 Runtime.")
	}
	defer window.Destroy()

	app, err := newApp(window)
	if err != nil {
		return err
	}
	defer app.shutdown()

	if err := window.Bind("getAppState", app.getAppState); err != nil {
		return fmt.Errorf("не удалось подключить интерфейс: %w", err)
	}
	if err := window.Bind("startClipping", app.startClipping); err != nil {
		return fmt.Errorf("не удалось подключить интерфейс: %w", err)
	}
	if err := window.Bind("stopClipping", app.stopClipping); err != nil {
		return fmt.Errorf("не удалось подключить интерфейс: %w", err)
	}
	if err := window.Bind("openOutputFolder", app.openOutputFolder); err != nil {
		return fmt.Errorf("не удалось подключить интерфейс: %w", err)
	}
	if err := window.Bind("chooseGameplayFile", app.chooseGameplayFile); err != nil {
		return fmt.Errorf("не удалось подключить интерфейс: %w", err)
	}
	if err := window.Bind("saveSettings", app.saveSettings); err != nil {
		return fmt.Errorf("не удалось подключить настройки: %w", err)
	}
	if err := window.Bind("beginWindowDrag", beginWindowDrag); err != nil {
		return fmt.Errorf("не удалось подключить управление окном: %w", err)
	}
	if err := window.Bind("minimizeWindow", minimizeWindow); err != nil {
		return fmt.Errorf("не удалось подключить управление окном: %w", err)
	}
	if err := window.Bind("toggleMaximizeWindow", toggleMaximizeWindow); err != nil {
		return fmt.Errorf("не удалось подключить управление окном: %w", err)
	}
	if err := window.Bind("closeWindow", closeWindow); err != nil {
		return fmt.Errorf("не удалось подключить управление окном: %w", err)
	}
	if err := window.Bind("openClipFile", app.openClipFile); err != nil {
		return fmt.Errorf("не удалось подключить открытие клипа: %w", err)
	}
	if err := window.Bind("getClipLibrary", app.getClipLibrary); err != nil {
		return fmt.Errorf("не удалось подключить библиотеку клипов: %w", err)
	}
	if err := window.Bind("deleteClipFile", app.deleteClipFile); err != nil {
		return fmt.Errorf("не удалось подключить удаление клипов: %w", err)
	}
	if err := window.Bind("getJobHistory", app.getJobHistory); err != nil {
		return fmt.Errorf("не удалось подключить историю обработок: %w", err)
	}
	if err := window.Bind("getDiagnostics", app.getDiagnostics); err != nil {
		return fmt.Errorf("не удалось подключить проверку компонентов: %w", err)
	}

	window.SetSize(1180, 820, webview2.HintMin)
	if err := applyBorderlessWindow(window.Window()); err != nil {
		return fmt.Errorf("не удалось создать окно без рамки: %w", err)
	}
	window.SetHtml(appHTML)
	window.Run()
	return nil
}

func showStartupError(message string) {
	title, err := windows.UTF16PtrFromString("Clipper")
	if err != nil {
		log.Printf("%s: %v", message, err)
		return
	}
	text, err := windows.UTF16PtrFromString(message)
	if err != nil {
		log.Printf("Clipper: %s", message)
		return
	}
	procedure := syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW")
	procedure.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), 0x10)
}

func newApp(window webview2.WebView) (*App, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("не удалось определить папку настроек: %w", err)
	}
	configDir = filepath.Join(configDir, "Clipper")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return nil, fmt.Errorf("не удалось создать папку настроек: %w", err)
	}

	videoDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("не удалось определить домашнюю папку: %w", err)
	}
	workDir := filepath.Join(videoDir, "Videos", "Clipper")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, fmt.Errorf("не удалось создать папку клипов: %w", err)
	}

	state := AppState{Version: appVersion, WorkDir: workDir, ClipCount: 3, ClipDuration: defaultClipLength}
	settingsPath := filepath.Join(configDir, "settings.json")
	if data, err := os.ReadFile(settingsPath); err == nil {
		var previous struct {
			LicenseKey string `json:"licenseKey"`
		}
		if err := json.Unmarshal(data, &previous); err != nil {
			return nil, fmt.Errorf("не удалось прочитать настройки %q: %w", settingsPath, err)
		}
		if err := json.Unmarshal(data, &state); err != nil {
			return nil, fmt.Errorf("не удалось прочитать настройки %q: %w", settingsPath, err)
		}
		state.LicenseKey = previous.LicenseKey
		state.Version = appVersion
		state.WorkDir = workDir
		if state.ClipCount < 1 || state.ClipCount > 20 {
			state.ClipCount = 3
		}
		if state.ClipDuration < minClipLength || state.ClipDuration > maxClipLength {
			state.ClipDuration = defaultClipLength
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("не удалось прочитать настройки %q: %w", settingsPath, err)
	}
	encryptedKeyPath := filepath.Join(configDir, "license.dpapi")
	if encrypted, err := os.ReadFile(encryptedKeyPath); err == nil {
		key, err := unprotectLicenseKey(encrypted)
		if err != nil {
			return nil, fmt.Errorf("не удалось расшифровать сохранённый ключ доступа: %w", err)
		}
		state.LicenseKey = key
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("не удалось прочитать сохранённый ключ доступа: %w", err)
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("не удалось определить домашнюю папку: %w", err)
	}
	if state.GameplayURL == "" {
		legacyPath := filepath.Join(homeDir, ".clipper_settings.json")
		if data, err := os.ReadFile(legacyPath); err == nil {
			var legacy struct {
				GameplayURL string `json:"gameplay_url"`
			}
			if err := json.Unmarshal(data, &legacy); err != nil {
				return nil, fmt.Errorf("не удалось прочитать старые настройки %q: %w", legacyPath, err)
			}
			state.GameplayURL = legacy.GameplayURL
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("не удалось прочитать старые настройки %q: %w", legacyPath, err)
		}
	}
	if state.LicenseKey == "" {
		legacyKeyPath := filepath.Join(homeDir, ".clipper_license")
		if data, err := os.ReadFile(legacyKeyPath); err == nil {
			state.LicenseKey = strings.TrimSpace(string(data))
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("не удалось прочитать сохранённый ключ доступа: %w", err)
		}
	}

	if err := cleanupPartial(workDir); err != nil {
		return nil, fmt.Errorf("не удалось очистить временные файлы: %w", err)
	}
	if state.LicenseKey != "" {
		if err := saveConfigFile(state); err != nil {
			return nil, err
		}
	}
	return &App{window: window, alive: true, config: state}, nil
}

func (a *App) getAppState() (string, error) {
	a.mu.Lock()
	state := a.config
	a.mu.Unlock()
	data, err := json.Marshal(struct {
		AppState
		LicenseKey string `json:"licenseKey"`
	}{AppState: state, LicenseKey: state.LicenseKey})
	return string(data), err
}

func (a *App) saveConfig(state AppState) error {
	if err := saveConfigFile(state); err != nil {
		return err
	}
	a.mu.Lock()
	a.config = state
	a.mu.Unlock()
	return nil
}

func saveConfigFile(state AppState) error {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("не удалось определить папку настроек: %w", err)
	}
	configDir = filepath.Join(configDir, "Clipper")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return fmt.Errorf("не удалось создать папку настроек: %w", err)
	}
	keyPath := filepath.Join(configDir, "license.dpapi")
	if state.LicenseKey == "" {
		if err := os.Remove(keyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("не удалось удалить сохранённый ключ доступа: %w", err)
		}
	} else {
		encrypted, err := protectLicenseKey(state.LicenseKey)
		if err != nil {
			return fmt.Errorf("не удалось защитить ключ доступа Windows DPAPI: %w", err)
		}
		if err := writeFileAtomically(keyPath, encrypted, 0o600); err != nil {
			return fmt.Errorf("не удалось сохранить защищённый ключ доступа: %w", err)
		}
	}
	path := filepath.Join(configDir, "settings.json")
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("не удалось сохранить настройки: %w", err)
	}
	if err := writeFileAtomically(path, data, 0o600); err != nil {
		return fmt.Errorf("не удалось записать настройки %q: %w", path, err)
	}
	return nil
}

func (a *App) saveSettings(raw string) error {
	var settings struct {
		LicenseKey string `json:"licenseKey"`
	}
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return fmt.Errorf("не удалось прочитать настройки: %w", err)
	}
	a.mu.Lock()
	state := a.config
	a.mu.Unlock()
	state.LicenseKey = strings.TrimSpace(settings.LicenseKey)
	if len(state.LicenseKey) > 1024 {
		return errors.New("ключ доступа слишком длинный")
	}
	if err := a.saveConfig(state); err != nil {
		return err
	}
	a.emit(UIEvent{Type: "settings-saved", Message: "Настройки сохранены."})
	return nil
}

func (a *App) startClipping(raw string) error {
	var request StartRequest
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		return fmt.Errorf("не удалось прочитать параметры: %w", err)
	}
	request.URL = strings.TrimSpace(request.URL)
	request.GameplayURL = strings.TrimSpace(request.GameplayURL)
	request.GameplayPath = strings.TrimSpace(request.GameplayPath)
	if request.ClipCount < 1 || request.ClipCount > 20 {
		return errors.New("количество клипов должно быть от 1 до 20")
	}
	if request.ClipDuration < minClipLength || request.ClipDuration > maxClipLength {
		return fmt.Errorf("длительность клипа должна быть от %d до %d секунд", minClipLength, maxClipLength)
	}
	if request.GameplayURL != "" && !validHTTPURL(request.GameplayURL) {
		return errors.New("проверьте ссылку на фоновое видео")
	}
	if request.GameplayPath != "" {
		info, err := os.Stat(request.GameplayPath)
		if err != nil || info.IsDir() {
			if request.GameplayURL == "" {
				return errors.New("не удалось открыть выбранный файл геймплея")
			}
			request.GameplayPath = ""
		}
	}
	if request.GameplayURL == "" && request.GameplayPath == "" {
		return errors.New("добавьте локальный файл геймплея или ссылку на него")
	}
	if !validHTTPURL(request.URL) {
		return errors.New("введите ссылку на исходное видео")
	}

	historyID, err := a.addJobHistory(request)
	if err != nil {
		return err
	}
	a.mu.Lock()
	key := a.config.LicenseKey
	if a.cancel != nil {
		a.mu.Unlock()
		if err := a.finishJobHistory(historyID, "Ошибка", 0, "Обработка уже запущена"); err != nil {
			return fmt.Errorf("обработка уже запущена; также не удалось сохранить историю: %w", err)
		}
		return errors.New("обработка уже запущена")
	}
	if key == "" {
		a.mu.Unlock()
		if err := a.finishJobHistory(historyID, "Ошибка", 0, "Не сохранён ключ доступа"); err != nil {
			return fmt.Errorf("сначала сохраните ключ доступа в настройках; также не удалось сохранить историю: %w", err)
		}
		return errors.New("сначала сохраните ключ доступа в настройках")
	}
	state := a.config
	state.GameplayURL = request.GameplayURL
	state.GameplayPath = request.GameplayPath
	state.ClipCount = request.ClipCount
	state.ClipDuration = request.ClipDuration
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	a.cancel = cancel
	a.jobDone = done
	a.historyID = historyID
	a.mu.Unlock()

	a.emit(UIEvent{Type: "started", Message: "Подготавливаю обработку…"})
	go func() {
		defer func() {
			cancel()
			a.mu.Lock()
			if a.jobDone == done {
				a.cancel = nil
				a.jobDone = nil
			}
			a.mu.Unlock()
			close(done)
		}()
		a.runJob(ctx, request, state)
	}()
	return nil
}

func (a *App) runJob(ctx context.Context, request StartRequest, state AppState) {
	license, err := validateLicense(ctx, state.LicenseKey)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			a.emit(UIEvent{Type: "stopped", Message: "Обработка остановлена."})
			return
		}
		a.emit(UIEvent{Type: "error", Message: err.Error()})
		return
	}
	if license.Remaining != nil && *license.Remaining < 1 {
		a.emit(UIEvent{Type: "error", Message: "На ключе закончились клипы. Введите другой ключ."})
		return
	}
	a.emit(UIEvent{Type: "license", Message: "Ключ подтверждён.", Remaining: license.Remaining})
	if err := a.saveConfig(state); err != nil {
		a.emit(UIEvent{Type: "error", Message: err.Error()})
		return
	}

	count := request.ClipCount
	if license.Remaining != nil && count > *license.Remaining {
		count = *license.Remaining
	}
	remaining := license.Remaining
	onProgress := func(event UIEvent) {
		event.Remaining = remaining
		a.emit(event)
	}
	onClip := func(path string) error {
		if err := incrementLicenseUsage(ctx, state.LicenseKey, license.ID); err != nil {
			return err
		}
		if remaining != nil {
			(*remaining)--
		}
		a.emit(UIEvent{Type: "usage", Message: "Клип обработан", File: path, Remaining: remaining})
		return nil
	}

	made, err := processVideo(ctx, request, count, state.WorkDir, onProgress, onClip)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			a.emit(UIEvent{Type: "stopped", Message: "Обработка остановлена.", Current: made})
			return
		}
		a.emit(UIEvent{Type: "error", Message: err.Error(), Current: made})
		return
	}
	a.emit(UIEvent{Type: "complete", Message: fmt.Sprintf("Готово: создано клипов — %d.", made), Current: made, Total: count, Remaining: remaining})
}

func (a *App) stopClipping() error {
	a.mu.Lock()
	cancel := a.cancel
	a.mu.Unlock()
	if cancel == nil {
		return errors.New("сейчас ничего не обрабатывается")
	}
	cancel()
	return nil
}

func (a *App) openOutputFolder() error {
	a.mu.Lock()
	workDir := a.config.WorkDir
	a.mu.Unlock()
	return exec.Command("explorer.exe", workDir).Start()
}

func (a *App) openClipFile(path string) error {
	path, err := a.validateClipPath(path)
	if err != nil {
		return err
	}
	return exec.Command("explorer.exe", "/select,", path).Start()
}

func (a *App) getClipLibrary() (string, error) {
	a.mu.Lock()
	workDir := a.config.WorkDir
	a.mu.Unlock()
	clips, err := listClips(workDir)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(clips)
	if err != nil {
		return "", fmt.Errorf("не удалось подготовить библиотеку клипов: %w", err)
	}
	return string(data), nil
}

func (a *App) deleteClipFile(path string) error {
	path, err := a.validateClipPath(path)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("не удалось удалить клип: %w", err)
	}
	return nil
}

func (a *App) validateClipPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("путь к клипу пуст")
	}
	a.mu.Lock()
	workDir := a.config.WorkDir
	a.mu.Unlock()
	return validateClipPath(workDir, path)
}

func (a *App) chooseGameplayFile() (string, error) {
	filter := syscall.StringToUTF16("Видео (*.mp4;*.mov;*.mkv;*.webm)\x00*.mp4;*.mov;*.mkv;*.webm\x00Все файлы (*.*)\x00*.*\x00")
	title, err := windows.UTF16PtrFromString("Выберите видео для фона")
	if err != nil {
		return "", fmt.Errorf("не удалось открыть выбор файла: %w", err)
	}
	file := make([]uint16, 32768)
	ofn := openFileName{
		StructSize: uint32(unsafe.Sizeof(openFileName{})),
		Owner:      uintptr(a.window.Window()),
		Filter:     &filter[0],
		File:       &file[0],
		MaxFile:    uint32(len(file)),
		Title:      title,
		Flags:      0x00001000 | 0x00000800 | 0x00080000,
		DefaultExt: nil,
	}
	proc := syscall.NewLazyDLL("comdlg32.dll").NewProc("GetOpenFileNameW")
	result, _, _ := proc.Call(uintptr(unsafe.Pointer(&ofn)))
	if result == 0 {
		extendedError, _, _ := syscall.NewLazyDLL("comdlg32.dll").NewProc("CommDlgExtendedError").Call()
		if extendedError == 0 {
			return "", nil
		}
		return "", fmt.Errorf("диалог выбора файла завершился с ошибкой 0x%x", extendedError)
	}
	return windows.UTF16ToString(file), nil
}

type openFileName struct {
	StructSize    uint32
	Owner         uintptr
	Instance      uintptr
	Filter        *uint16
	CustomFilter  *uint16
	MaxCustFilter uint32
	FilterIndex   uint32
	File          *uint16
	MaxFile       uint32
	FileTitle     *uint16
	MaxFileTitle  uint32
	InitialDir    *uint16
	Title         *uint16
	Flags         uint32
	FileOffset    uint16
	FileExtension uint16
	DefaultExt    *uint16
	CustData      uintptr
	Hook          uintptr
	TemplateName  *uint16
	Reserved      uintptr
	ReservedWord  uint32
	FlagsEx       uint32
}

func (a *App) shutdown() {
	a.mu.Lock()
	a.alive = false
	cancel := a.cancel
	done := a.jobDone
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (a *App) emit(event UIEvent) {
	if event.Type == "complete" || event.Type == "stopped" || event.Type == "error" {
		a.mu.Lock()
		historyID := a.historyID
		a.mu.Unlock()
		status := map[string]string{"complete": "Готово", "stopped": "Остановлено", "error": "Ошибка"}[event.Type]
		if historyID != "" {
			if err := a.finishJobHistory(historyID, status, event.Current, event.Message); err != nil {
				log.Printf("Не удалось сохранить историю Clipper: %v", err)
			}
		}
	}
	data, err := json.Marshal(event)
	if err != nil {
		log.Printf("Не удалось подготовить сообщение интерфейсу: %v", err)
		return
	}
	a.mu.Lock()
	window, alive := a.window, a.alive
	a.mu.Unlock()
	if !alive {
		return
	}
	window.Dispatch(func() {
		window.Eval("window.onClipperEvent(" + string(data) + ")")
	})
}

func validHTTPURL(value string) bool {
	parsed, err := url.ParseRequestURI(value)
	return err == nil &&
		(parsed.Scheme == "https" || parsed.Scheme == "http") &&
		parsed.Host != ""
}
