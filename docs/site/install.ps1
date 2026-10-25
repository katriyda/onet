#Requires -Version 5.1
[CmdletBinding()]
param(
    [string]$Version = $env:ONET_VERSION,
    [string]$Dir = $env:ONET_INSTALL_DIR,
    [switch]$NoPath,
    [switch]$Help
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

function Show-Usage {
    @'
onet installer - download and install the onet binary.

usage: install.ps1 [-Version VERSION] [-Dir DIR] [-NoPath]
  -Version VERSION   install a specific release tag (e.g. v0.3.1)
  -Dir DIR           install directory (default: %LOCALAPPDATA%\Programs\onet)
  -NoPath            do not add the install directory to the user PATH
  -Help              show this help

env: ONET_VERSION, ONET_INSTALL_DIR, ONET_NO_PATH=1
'@
}

function Get-Sha256Hex([string]$Path) {
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $stream = [System.IO.File]::OpenRead($Path)
        try {
            $bytes = $sha.ComputeHash($stream)
        } finally {
            $stream.Dispose()
        }
    } finally {
        $sha.Dispose()
    }
    return ([System.BitConverter]::ToString($bytes) -replace '-', '').ToLowerInvariant()
}

if ($Help) {
    Show-Usage
    exit 0
}

if ($env:ONET_NO_PATH -eq '1') {
    $NoPath = $true
}

$Base = 'https://github.com/katriyda/onet/releases'

$archRaw = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
switch ($archRaw) {
    'AMD64' { $arch = 'amd64' }
    'ARM64' { $arch = 'arm64' }
    default { throw "unsupported architecture: $archRaw" }
}

$Version = "$Version".Trim()
if ($Version) {
    $Version = $Version -replace '^v', ''
    if ($Version -notmatch '^[0-9]+\.[0-9]+\.[0-9]+$') {
        throw "invalid version tag (expected x.y.z): $Version"
    }
}

if (-not $Dir) {
    $Dir = Join-Path $env:LOCALAPPDATA 'Programs\onet'
}
$Dir = [System.IO.Path]::GetFullPath($Dir)

$asset = "onet-windows-$arch.zip"
$base = if ($Version) { "$Base/download/v$Version" } else { "$Base/latest/download" }

$tmp = $null
try {
    $tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("onet-install-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Force -Path $tmp | Out-Null

    $zip = Join-Path $tmp $asset
    try {
        Invoke-WebRequest -Uri "$base/$asset" -OutFile $zip -UseBasicParsing
    } catch {
        throw "download failed: $base/$asset"
    }

    $sums = Join-Path $tmp 'checksums.txt'
    $haveSums = $true
    try {
        Invoke-WebRequest -Uri "$base/checksums.txt" -OutFile $sums -UseBasicParsing
    } catch {
        $haveSums = $false
    }

    if (-not $haveSums) {
        Write-Warning 'checksums.txt not available for this release; skipping verification'
    } else {
        $expected = $null
        foreach ($line in Get-Content -LiteralPath $sums) {
            if ($line -match '^([0-9a-fA-F]{64})\s+\*?(.+)$') {
                if ($Matches[2].Trim() -eq $asset) {
                    $expected = $Matches[1].ToLowerInvariant()
                    break
                }
            }
        }
        if (-not $expected) {
            throw "$asset is not listed in checksums.txt"
        }
        $actual = Get-Sha256Hex $zip
        if ($actual -ne $expected) {
            throw "checksum mismatch for $asset"
        }
    }

    Expand-Archive -LiteralPath $zip -DestinationPath $tmp -Force
    $exe = Get-ChildItem -LiteralPath $tmp -Recurse -Filter 'onet.exe' -File | Select-Object -First 1
    if (-not $exe) {
        throw "binary not found in $asset"
    }

    $target = Join-Path $Dir 'onet.exe'
    $running = Get-Process onet -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $target }
    if ($running) {
        throw 'onet.exe is running; stop it first'
    }

    New-Item -ItemType Directory -Force -Path $Dir | Out-Null
    Copy-Item -LiteralPath $exe.FullName -Destination $target -Force

    $added = $false
    if (-not $NoPath) {
        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        if ($null -eq $userPath) {
            $userPath = ''
        }
        $present = $false
        foreach ($part in ($userPath -split ';')) {
            if ($part -and ($part.TrimEnd('\') -ieq $Dir.TrimEnd('\'))) {
                $present = $true
                break
            }
        }
        if (-not $present) {
            $newPath = if ($userPath) { $userPath.TrimEnd(';') + ';' + $Dir } else { $Dir }
            [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
            $env:Path = $Dir + ';' + $env:Path
            $added = $true
        }
    }

    try {
        $verOut = & $target version 2>$null
    } catch {
        throw 'downloaded binary failed to start'
    }
    if ($LASTEXITCODE -ne 0) {
        throw 'downloaded binary failed to start'
    }
    $ver = ("$verOut").Trim() -replace '^v', ''
    if (-not $ver) {
        $ver = if ($Version) { $Version } else { 'latest' }
    }

    Write-Output "onet $ver (windows/$arch)"
    Write-Output "installed: $target"
    Write-Output 'run: onet'
    if ($added) {
        Write-Output "path: added $Dir to user PATH (new terminals)"
    }
} finally {
    if ($tmp -and (Test-Path -LiteralPath $tmp)) {
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}
