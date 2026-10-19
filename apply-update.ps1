$ErrorActionPreference = 'Stop'

$source = Join-Path $PSScriptRoot 'Clipper.exe'
$installDirectory = Join-Path $env:LOCALAPPDATA 'Programs\Clipper'
$target = Join-Path $installDirectory 'Clipper.exe'
$staged = Join-Path $installDirectory 'Clipper.exe.new'
$backup = Join-Path $installDirectory 'Clipper.exe.bak'

if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
    throw 'Clipper.exe is missing. Extract all files from ClipperUpdate.zip first.'
}
if (-not (Test-Path -LiteralPath $target -PathType Leaf)) {
    throw 'The installed Clipper was not found. Use ClipperSetup.bat for a first installation.'
}

Write-Host 'Waiting for Clipper to close. Save your work, then close the app.'
while (Get-Process -Name Clipper -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $target }) {
    Start-Sleep -Seconds 2
}

try {
    Copy-Item -LiteralPath $source -Destination $staged -Force
    $sourceHash = (Get-FileHash -LiteralPath $source -Algorithm SHA256).Hash
    $stagedHash = (Get-FileHash -LiteralPath $staged -Algorithm SHA256).Hash
    if ($sourceHash -ne $stagedHash) {
        throw 'The copied update failed its SHA-256 check.'
    }

    [System.IO.File]::Replace($staged, $target, $backup, $true)
    if (Test-Path -LiteralPath $backup) {
        Remove-Item -LiteralPath $backup -Force
    }
}
catch {
    if (Test-Path -LiteralPath $staged) {
        Remove-Item -LiteralPath $staged -Force
    }
    throw
}

Write-Host 'Clipper has been updated. Your settings, clips, Whisper model, and tools were preserved.'
Read-Host 'Press Enter to close'
