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
$shellDllPath = Join-Path $distDir "telemount_shell.dll"
$msiPath = Join-Path $distDir "winfsp.msi"
$issPath = Join-Path $repoRoot "installer\telemount.iss"

New-Item -ItemType Directory -Force -Path $distDir | Out-Null

# 1. Build pure-Go Windows GUI binary (CGO_ENABLED=0) with embedded semver
$verTag = if ($Version.StartsWith("v")) { $Version } else { "v$Version" }
Write-Host "[1/4] Building telemount.exe $verTag (windows/amd64, CGO_ENABLED=0, GUI)..."
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

# 2. Build native x64 Windows Explorer Shell Icon Overlay COM DLL (telemount_shell.dll)
Write-Host "[2/4] Building telemount_shell.dll (x64 COM Shell Icon Overlay)..."
$vcvars = $null
$vswhere = "${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe"
if (Test-Path $vswhere) {
    $vsInstall = & $vswhere -latest -products * -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath 2>$null | Select-Object -First 1
    if (-not $vsInstall) {
        $vsInstall = & $vswhere -all -products * -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath 2>$null | Select-Object -First 1
    }
    if ($vsInstall) {
        $cand = Join-Path $vsInstall "VC\Auxiliary\Build\vcvars64.bat"
        if (Test-Path $cand) { $vcvars = $cand }
    }
}
if (-not $vcvars) {
    $found = Get-ChildItem "${env:ProgramFiles(x86)}\Microsoft Visual Studio", "$env:ProgramFiles\Microsoft Visual Studio" -Recurse -Filter "vcvars64.bat" -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($found) { $vcvars = $found.FullName }
}
if (-not $vcvars) {
    throw "MSVC Build Tools (vcvars64.bat) not found; required to compile telemount_shell.dll."
}

Push-Location $repoRoot
try {
    $resPath = Join-Path $distDir "telemount_shell.res"
    $objPath = Join-Path $distDir "telemount_shell.obj"
    $buildCmd = "`"$vcvars`" >nul && rc /nologo /fo `"$resPath`" shell-ext\telemount_shell.rc && cl /nologo /O2 /W4 /WX /MT /LD /EHsc shell-ext\telemount_shell.cpp `"$resPath`" /Fe:`"$shellDllPath`" /Fo:`"$objPath`" /link /DEF:shell-ext\telemount_shell.def ole32.lib advapi32.lib shell32.lib user32.lib"
    cmd /c $buildCmd
    if ($LASTEXITCODE -ne 0) { throw "MSVC build of telemount_shell.dll failed with exit code $LASTEXITCODE" }
    Remove-Item -Path $resPath, $objPath, (Join-Path $distDir "telemount_shell.lib"), (Join-Path $distDir "telemount_shell.exp") -Force -ErrorAction SilentlyContinue
} finally {
    Pop-Location
}

# 3. Download & verify official signed WinFsp MSI
if (-not (Test-Path $msiPath)) {
    Write-Host "[3/4] Downloading official WinFsp MSI from $WinFspUrl ..."
    $tmpMsi = "$msiPath.download"
    Invoke-WebRequest -Uri $WinFspUrl -OutFile $tmpMsi -UseBasicParsing
    Move-Item -Path $tmpMsi -Destination $msiPath -Force
} else {
    Write-Host "[3/4] Reusing cached $msiPath"
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

# 4. Locate Inno Setup Compiler (ISCC.exe) and build TeleMount-Setup.exe
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

Write-Host "[4/4] Compiling TeleMount-Setup.exe with $iscc ..."
& $iscc `
    "/DAppVersion=$Version" `
    "/DAppExePath=$exePath" `
    "/DShellDllPath=$shellDllPath" `
    "/DWinFspMsiPath=$msiPath" `
    "/DOutputDir=$distDir" `
    $issPath
if ($LASTEXITCODE -ne 0) { throw "ISCC failed with exit code $LASTEXITCODE" }

# Remove standalone intermediate binaries (Windows distribution is Setup installer only)
Remove-Item -Path $exePath, $shellDllPath -Force -ErrorAction SilentlyContinue

$setupExe = Join-Path $distDir "TeleMount-Setup.exe"
$sizeMB = (Get-Item $setupExe).Length / 1MB
Write-Host ("Done! Installer created: {0} ({1:N2} MB)" -f $setupExe, $sizeMB)
