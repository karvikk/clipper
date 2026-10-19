# Clipper

Clipper is a Windows x64 desktop app written in Go. It finds popular moments in a video (or falls back to audio analysis), transcribes speech, burns animated word captions into a 9:16 clip, and adds a local or linked gameplay video as the lower half of the frame. Clip length is adjustable from 15 to 90 seconds.

Video processing runs locally. Finished clips are saved under `%USERPROFILE%\Videos\Clipper`; settings and the Whisper model live under `%AppData%\Clipper`. The access key is entered in the app's **Настройки** panel and stored encrypted with Windows DPAPI. New users without a key are directed to Telegram [@karvikk](https://t.me/karvikk) from the settings page. Existing keys from both the Python app's `%USERPROFILE%\.clipper_license` file and the previous Go settings are imported automatically; the legacy Python key file is left untouched. The Keygen account and machine fingerprint format are unchanged.

Clipper uses a working hardware H.264 encoder when FFmpeg can verify NVIDIA NVENC, Intel Quick Sync, or AMD AMF; otherwise it falls back to faster, thread-limited x264 encoding. Whisper and FFmpeg's filter threads are also capped to reduce CPU contention. Processing utilities run without opening console windows.

## Install on another PC

Run `build.bat`. It creates `Clipper.exe` and a `release\` folder containing the app and one `ClipperSetup.exe`. Share that same installer for a new install or an update; it checks the latest GitHub release every time it is launched.

The installer EXE verifies and installs Microsoft's signed WebView2 Runtime if needed, then downloads about 350 MB of required components including Whisper and its model on a fresh install. It verifies component SHA-256 digests, installs the app for the current Windows user, and creates Desktop and Start menu shortcuts. It does not require Python, Go, or administrator privileges on the recipient's PC. An internet connection is required to check releases and download components. If Clipper is already installed, the same file compares versions and replaces only the app executable when an update is available, preserving Whisper, tools, settings, access key, history, and clips. Missing core components trigger a verified repair install. If Whisper or its model is later missing or damaged, Clipper verifies and restores it at the next processing attempt.

Components are downloaded from the official [whisper.cpp releases](https://github.com/ggml-org/whisper.cpp/releases), [FFmpeg builds](https://github.com/BtbN/FFmpeg-Builds/releases), [yt-dlp releases](https://github.com/yt-dlp/yt-dlp/releases), [Deno releases](https://github.com/denoland/deno/releases), and [whisper.cpp model repository](https://huggingface.co/ggerganov/whisper.cpp). Setup verifies all release asset digests supplied by GitHub and pinned SHA-256 values for the Whisper executable archive and model before installing them.

## Share Clipper or an update

1. Keep the project source in the repository. Include the Go files, `ui.html`, `installer.html`, `.bat`/`.ps1` scripts, `go.mod`, `go.sum`, tests, and `README.md`. `.gitignore` excludes generated EXEs, release packages, and video outputs.
2. Create a GitHub Release and attach **only `ClipperSetup.exe`** and `SHA256SUMS.txt`.
3. For either a first-time install or an update, your friend downloads and launches `ClipperSetup.exe`. It detects the installed version, checks GitHub for the newest release, then installs or updates accordingly.

Do not upload your personal license key, `%AppData%\Clipper` settings, or `.clipper_license` file. The installed app does not silently update itself; download and run the latest `ClipperSetup.exe` when you want to check for updates.

### Automated versioned releases

The GitHub Actions workflow runs Go tests, `go vet`, and the embedded UI JavaScript syntax check on pushes to `main` and pull requests. It pins current Node 24-compatible official actions. To publish a version, create and push a semantic version tag from the tested commit:

```powershell
git switch main
git pull --ff-only
git tag v1.4.0
git push origin v1.4.0
```

GitHub Actions builds Windows x64 binaries with that version embedded, creates a GitHub Release with generated change notes and explicit download instructions, and attaches only `ClipperSetup.exe` and `SHA256SUMS.txt`. The same `ClipperSetup.exe` checks GitHub for newer versions and performs either a new installation or an update. Do not reuse a published tag; increment the version for each release. The app is not Authenticode-signed, so Windows SmartScreen may show a warning until a code-signing certificate is configured.

## App sections

- **Создать клипы** — choose source/background, clip count and duration.
- **Мои клипы** — search completed clips, see their sizes, open them in Explorer, or remove a Clipper output.
- **История** — review the latest 200 local processing attempts and their outcomes.
- **Пресеты** — apply ready-made clip count and duration combinations without changing video or background selections.
- **Гид** — follow a short checklist for preparing source video, background, and processing.
- **Настройки** — store the access key and check that required tools are available.

## Build and run from source

Use Windows x64 and Go 1.26.2 or newer:

```powershell
go test ./...
go vet ./...
.\build.bat
```

For development, run `go run .`. The application opens a custom frameless window using WebView2, with the native window border hidden where supported by Windows. On a fresh PC, run the generated `release\ClipperSetup.exe` to install WebView2 Runtime and the media tools. A valid Clipper license key is required to create clips; enter it in **Настройки**. Existing Python settings are imported on first launch.
