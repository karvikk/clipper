# Clipper

Clipper is a Windows x64 desktop app written in Go. It finds popular moments in a video (or falls back to audio analysis), transcribes speech, burns animated word captions into a 9:16 clip, and adds a local or linked gameplay video as the lower half of the frame. Clip length is adjustable from 15 to 90 seconds.

Video processing runs locally. Finished clips are saved under `%USERPROFILE%\Videos\Clipper`; settings and the Whisper model live under `%AppData%\Clipper`. The access key is entered in the app's **Настройки** panel and stored encrypted with Windows DPAPI. Existing keys from both the Python app's `%USERPROFILE%\.clipper_license` file and the previous Go settings are imported automatically; the legacy Python key file is left untouched. The Keygen account and machine fingerprint format are unchanged.

## Install on another PC

Run `build.bat`. It creates `Clipper.exe` and a `release\` folder containing the app, setup launcher, and a small update-only ZIP. For a first install, share `release\ClipperSetup.bat` and `release\ClipperSetup.exe`; the other person runs the `.bat`.

The setup launcher checks and installs Microsoft's signed WebView2 Runtime, then opens the Clipper installer. The installer downloads about 350 MB of required components and the compact Whisper model, verifies their SHA-256 digests, installs the app for the current Windows user, and creates Desktop and Start menu shortcuts. It does not require Python, Go, or administrator privileges on the recipient's PC. An internet connection is required during setup. The installed app can then run offline except for online video downloads and license checks.

Components are downloaded from the official [whisper.cpp releases](https://github.com/ggml-org/whisper.cpp/releases), [FFmpeg builds](https://github.com/BtbN/FFmpeg-Builds/releases), [yt-dlp releases](https://github.com/yt-dlp/yt-dlp/releases), [Deno releases](https://github.com/denoland/deno/releases), and [whisper.cpp model repository](https://huggingface.co/ggerganov/whisper.cpp). Setup verifies all release asset digests supplied by GitHub and pinned SHA-256 values for the Whisper executable archive and model before installing them.

## Send an update to a friend

The friend does **not** need to download or install Whisper and FFmpeg again. After running `build.bat`, send only `release\ClipperUpdate.zip`. They extract all files to a folder and run `ApplyUpdate.bat`; the script waits until their Clipper is closed and replaces only `Clipper.exe`. It leaves the installed tools, Whisper model, saved access key, settings, history, and completed clips in place. If the friend has not installed Clipper yet, send the setup pair above instead.

For GitHub, the easiest option is a **GitHub Release** in your repository:

1. Keep the project source in the repository. Include the Go files, `ui.html`, `installer.html`, `.bat`/`.ps1` scripts, `go.mod`, `go.sum`, tests, and `README.md`. `.gitignore` excludes generated EXEs, release packages, and video outputs.
2. For an update to an existing installation, attach **only `ClipperUpdate.zip`** from `release\` to the release. Your friend downloads and runs `ApplyUpdate.bat` from the extracted archive.
3. For a first-time installation, attach **`ClipperSetup.bat` and `ClipperSetup.exe`**. The `.bat` installs/checks WebView2 and starts the app installer.

Do not upload your personal license key, `%AppData%\Clipper` settings, or `.clipper_license` file. A GitHub Release distributes the files but does not perform silent auto-updates; the friend downloads the new ZIP and runs its update script.

### Automated versioned releases

The GitHub Actions workflow runs Go tests, `go vet`, and the embedded UI JavaScript syntax check on pushes to `main` and pull requests. To publish a version, create and push a semantic version tag from the tested commit:

```powershell
git switch main
git pull --ff-only
git tag v1.1.0
git push origin v1.1.0
```

GitHub Actions builds Windows x64 binaries with that version embedded, creates a GitHub Release with generated notes, and attaches `ClipperSetup.bat`, `ClipperSetup.exe`, `ClipperUpdate.zip`, and `SHA256SUMS.txt`. The friend can download the matching release asset; use the setup pair for a new installation and only `ClipperUpdate.zip` for an existing one. Do not reuse a published tag; increment the version for each release. The app is not Authenticode-signed, so Windows SmartScreen may show a warning until a code-signing certificate is configured.

## App sections

- **Создать клипы** — choose source/background, clip count and duration.
- **Мои клипы** — search completed clips, see their sizes, open them in Explorer, or remove a Clipper output.
- **История** — review the latest 200 local processing attempts and their outcomes.
- **Настройки** — store the access key and check that required tools are available.

## Build and run from source

Use Windows x64 and Go 1.26.2 or newer:

```powershell
go test ./...
go vet ./...
.\build.bat
```

For development, run `go run .`. The application opens a custom frameless window using WebView2. On a fresh PC, run the installer launcher once to install WebView2 Runtime and the media tools. A valid Clipper license key is required to create clips; enter it in **Настройки**. Existing Python settings are imported on first launch.
