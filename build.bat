@echo off
setlocal
cd /d "%~dp0"

if not exist "release" mkdir "release"

for /f %%A in ('go env GOARCH') do set "GOARCH=%%A"
if not "%GOARCH%"=="amd64" (
  echo The Clipper installer must be built with the Windows amd64 Go toolchain.
  exit /b 1
)

echo Building Clipper for Windows x64...
if not defined CLIPPER_VERSION set "CLIPPER_VERSION=dev"
go build -trimpath -ldflags="-H=windowsgui -X main.appVersion=%CLIPPER_VERSION%" -o "release\Clipper.exe" .
if errorlevel 1 (
  echo.
  echo Build failed. Check that Go 1.26.2 is installed.
  pause
  exit /b 1
)

copy /y "release\Clipper.exe" "Clipper.exe" >nul
if errorlevel 1 (
  echo Could not update the development Clipper.exe.
  exit /b 1
)
copy /y "release\Clipper.exe" "release\ClipperSetup.exe" >nul
if errorlevel 1 (
  echo Could not prepare the ClipperSetup.exe installer.
  exit /b 1
)
if exist "release\ClipperSetup.bat" del /q "release\ClipperSetup.bat"
if exist "release\ClipperUpdate.zip" del /q "release\ClipperUpdate.zip"
if exist "release\ApplyUpdate.bat" del /q "release\ApplyUpdate.bat"
if exist "release\ApplyUpdate.ps1" del /q "release\ApplyUpdate.ps1"
powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "tools\write-checksums.ps1" -ReleaseDirectory "release"
if errorlevel 1 (
  echo Could not create the release SHA-256 checksums.
  exit /b 1
)

echo.
echo Application: %CD%\release\Clipper.exe
echo Share this single installer with a new user:
echo   %CD%\release\ClipperSetup.exe
echo The same file checks for the latest release and updates an existing installation.
echo Checksums: %CD%\release\SHA256SUMS.txt
