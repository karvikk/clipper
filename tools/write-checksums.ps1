param(
    [string] $ReleaseDirectory = 'release'
)

$ErrorActionPreference = 'Stop'
$files = @('ClipperSetup.exe')
$lines = foreach ($name in $files) {
    $path = Join-Path $ReleaseDirectory $name
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "Release file is missing: $path"
    }
    $sha256 = [Security.Cryptography.SHA256]::Create()
    try {
        $stream = [IO.File]::OpenRead($path)
        try {
            $digest = [BitConverter]::ToString($sha256.ComputeHash($stream)).Replace('-', '').ToLowerInvariant()
        }
        finally {
            $stream.Dispose()
        }
    }
    finally {
        $sha256.Dispose()
    }
    if ($digest -notmatch '^[0-9a-f]{64}$') {
        throw "Could not calculate a valid SHA-256 for $name"
    }
    '{0}  {1}' -f $digest, $name
}

$manifest = Join-Path $ReleaseDirectory 'SHA256SUMS.txt'
[IO.File]::WriteAllLines($manifest, $lines, [Text.Encoding]::ASCII)
