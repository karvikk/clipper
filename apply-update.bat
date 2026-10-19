@echo off
setlocal
set "UPDATE_SCRIPT=%~dp0ApplyUpdate.ps1"

if not exist "%UPDATE_SCRIPT%" (
  echo ApplyUpdate.ps1 is missing. Extract all files from ClipperUpdate.zip first.
  pause
  exit /b 1
)

powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%UPDATE_SCRIPT%"
if errorlevel 1 (
  echo.
  echo The update did not finish. The previous Clipper version was preserved.
  pause
  exit /b 1
)
exit /b 0
