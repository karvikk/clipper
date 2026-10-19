@echo off
setlocal
set "SETUP_EXE=%~dp0ClipperSetup.exe"
set "WEBVIEW2_SETUP=%TEMP%\Clipper-WebView2-%RANDOM%.exe"

if not exist "%SETUP_EXE%" (
  echo ClipperSetup.exe is missing. Keep it next to this file.
  pause
  exit /b 1
)

echo Preparing Microsoft Edge WebView2 Runtime...
powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "$ErrorActionPreference='Stop'; [Net.ServicePointManager]::SecurityProtocol=[Net.SecurityProtocolType]::Tls12; Invoke-WebRequest -Uri 'https://go.microsoft.com/fwlink/p/?LinkId=2124703' -OutFile '%WEBVIEW2_SETUP%'; $signature=Get-AuthenticodeSignature -LiteralPath '%WEBVIEW2_SETUP%'; if ($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Subject -notmatch 'Microsoft') { throw 'The WebView2 installer signature is not valid.' }"
if errorlevel 1 (
  echo Could not download or verify the Microsoft WebView2 installer.
  pause
  exit /b 1
)

start /wait "" "%WEBVIEW2_SETUP%" /silent /install
if errorlevel 1 (
  del /q "%WEBVIEW2_SETUP%" >nul 2>nul
  echo WebView2 could not be installed. Check your internet connection and try again.
  pause
  exit /b 1
)
del /q "%WEBVIEW2_SETUP%" >nul 2>nul

start "" "%SETUP_EXE%"
exit /b 0
