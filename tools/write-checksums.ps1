param(
    [string] $ReleaseDirectory = 'release'
)

$ErrorActionPreference = 'Stop'
$files = @('ClipperSetup.bat', 'ClipperSetup.exe', 'ClipperUpdate.zip')
$lines = foreach ($name in $files) {
    $path = Join-Path $ReleaseDirectory $name
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "Release file is missing: $path"
    }
    $digest = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($digest -notmatch '^[0-9a-f]{64}$') {
        throw "Could not calculate a valid SHA-256 for $name"
    }
    '{0}  {1}' -f $digest, $name
}

$manifest = Join-Path $ReleaseDirectory 'SHA256SUMS.txt'
[IO.File]::WriteAllLines($manifest, $lines, [Text.Encoding]::ASCII)
