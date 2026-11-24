# Builds TeleMount Windows Setup (dist\TeleMount-Setup.exe) bundling telemount.exe + signed WinFsp MSI.
param(
    [string]$Version = "1.0.0",
    [string]$WinFspUrl = "https://github.com/winfsp/winfsp/releases/download/v2.2/winfsp-2.2.26323.msi",
    [string]$WinFspSha256 = ""
)

$defaultWinFspUrl = "https://github.com/winfsp/winfsp/releases/download/v2.2/winfsp-2.2.26323.msi"
$defaultWinFspSha256 = "36CE01F9EEDBBE6842B7FB4BEDC79D7D4F71983E8ACEB6877B46A07B331C7E19"
if ($WinFspSha256 -eq "" -and $WinFspUrl -eq $defaultWinFspUrl) {
    $WinFspSha256 = $defaultWinFspSha256
}

$ErrorActionPreference = "Stop"

$repoRoot = Split-Path -Parent $PSScriptRoot
$distDir = Join-Path $repoRoot "dist"
$exePath = Join-Path $distDir "telemount.exe"
$msiPath = Join-Path $distDir "winfsp.msi"
$issPath = Join-Path $repoRoot "installer\telemount.iss"

New-Item -ItemType Directory -Force -Path $distDir | Out-Null

# 1. Build pure-Go Windows GUI binary (CGO_ENABLED=0) with embedded semver
$verTag = if ($Version.StartsWith("v")) { $Version } else { "v$Version" }
Write-Host "[1/3] Building telemount.exe $verTag (windows/amd64, CGO_ENABLED=0, GUI)..."
Push-Location $repoRoot
try {
    $env:CGO_ENABLED = "0"
    $env:GOOS = "windows"
    $env:GOARCH = "amd64"
    go build -trimpath -ldflags="-s -w -H=windowsgui -X telemount/internal/version.Version=$verTag" -o $exePath ./cmd/telemount
    if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE" }
} finally {
    Pop-Location
}

# 2. Download & verify official signed WinFsp MSI
if (-not (Test-Path $msiPath)) {
    Write-Host "[2/3] Downloading official WinFsp MSI from $WinFspUrl ..."
    $tmpMsi = "$msiPath.download"
    Invoke-WebRequest -Uri $WinFspUrl -OutFile $tmpMsi -UseBasicParsing
    Move-Item -Path $tmpMsi -Destination $msiPath -Force
} else {
    Write-Host "[2/3] Reusing cached $msiPath"
}

$sig = Get-AuthenticodeSignature -FilePath $msiPath
if ($sig.Status -ne "Valid") {
    Remove-Item -Path $msiPath -Force -ErrorAction SilentlyContinue
    throw "WinFsp MSI Authenticode signature check failed (Status: $($sig.Status)). Aborting."
}
if ($WinFspSha256 -ne "") {
    $actualHash = (Get-FileHash -Path $msiPath -Algorithm SHA256).Hash
    if ($actualHash -ine $WinFspSha256) {
        Remove-Item -Path $msiPath -Force -ErrorAction SilentlyContinue
        throw "WinFsp MSI SHA-256 mismatch: got $actualHash, want $WinFspSha256"
    }
}
Write-Host "      Verified WinFsp MSI signature: $($sig.SignerCertificate.Subject)"

# 3. Locate Inno Setup Compiler (ISCC.exe) and build TeleMount-Setup.exe
$iscc = $null
$cmd = Get-Command "ISCC.exe" -ErrorAction SilentlyContinue
if ($cmd) {
    $iscc = $cmd.Source
} else {
    $candidates = @(
        "$env:LOCALAPPDATA\Programs\Inno Setup 6\ISCC.exe",
        "${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe",
        "$env:ProgramFiles\Inno Setup 6\ISCC.exe"
    )
    foreach ($c in $candidates) {
        if (Test-Path $c) { $iscc = $c; break }
    }
}
if (-not $iscc) {
    throw "Inno Setup 6 (ISCC.exe) not found. Install via: winget install --id JRSoftware.InnoSetup --scope user"
}

Write-Host "[3/3] Compiling TeleMount-Setup.exe with $iscc ..."
& $iscc `
    "/DAppVersion=$Version" `
    "/DAppExePath=$exePath" `
    "/DWinFspMsiPath=$msiPath" `
    "/DOutputDir=$distDir" `
    $issPath
if ($LASTEXITCODE -ne 0) { throw "ISCC failed with exit code $LASTEXITCODE" }

# Remove standalone intermediate exe (Windows distribution is Setup installer only)
Remove-Item -Path $exePath -Force -ErrorAction SilentlyContinue

$setupExe = Join-Path $distDir "TeleMount-Setup.exe"
$sizeMB = (Get-Item $setupExe).Length / 1MB
Write-Host ("Done! Installer created: {0} ({1:N2} MB)" -f $setupExe, $sizeMB)
