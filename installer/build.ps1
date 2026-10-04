<#
.SYNOPSIS
    Builds client2api-setup-<version>.exe, the Windows installer.

.DESCRIPTION
    Two stages:

    1. Stage a release build of the gateway into installer/setup/payload,
       which is the directory the installer embeds with //go:embed.  The
       binaries are built exactly the way .github/workflows/go-binaries.yml
       builds them -- -trimpath plus -s -w with the version injected through
       -X -- so the installer and the release archives cannot disagree about
       what "0.1.0-dev" means.

    2. Build installer/setup, the Go installer itself, into
       <repo>\dist\client2api-setup-<version>.exe.

    The version is never typed twice: it is read from cmd/client2api/main.go
    and passed to both builds with -X, and installer/version_test.go fails if
    the literal inside installer/setup/main.go ever drifts from it.

    If windres is available the installer gets the app icon and a VERSIONINFO
    resource; without it the build still succeeds, just with a generic icon.

.PARAMETER NoData
    Build an empty package: leave out the live data/ directory and ship the
    example config instead of the live one.  Use this for an installer you
    intend to hand to someone else -- data/ holds working access/refresh
    tokens and configs/client2api.json holds your settings; restore both on
    the target machine with the panel's import feature.

.PARAMETER SkipSmoke
    Skip the panel smoke test.  The default is to refuse the build when the
    staged gateway cannot boot its /panel/ in a real browser, which is what
    caught the 0.1.5 dead-panel regression.  Only pass this when no browser is
    available and you accept that the installer will still self-check on the
    target machine before replacing anything.

.PARAMETER OutputDirectory
    Where the finished setup .exe is written.  Defaults to <repo>\dist.

.PARAMETER PayloadDirectory
    Where the payload is staged.  Defaults to installer\setup\payload, which
    is the only path //go:embed can read from; override it only if you have
    also edited the go:embed directive.

.EXAMPLE
    pwsh -File installer\build.ps1
    Builds dist\client2api-setup-0.1.0-dev.exe including the current accounts.

.EXAMPLE
    pwsh -File installer\build.ps1 -NoData
    Builds a credential-free installer that is safe to share.
#>
[CmdletBinding()]
param(
    [switch] $NoData,
    [switch] $SkipSmoke,
    [string] $OutputDirectory,
    [string] $PayloadDirectory
)

$ErrorActionPreference = 'Stop'

$installerDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$repoRoot     = Split-Path -Parent $installerDir

if (-not $OutputDirectory)  { $OutputDirectory  = Join-Path $repoRoot 'dist' }
if (-not $PayloadDirectory) { $PayloadDirectory = Join-Path $installerDir 'setup\payload' }

function Write-Step($message) {
    Write-Host "==> $message" -ForegroundColor Cyan
}

# Get-IntegrityLabel reports the Windows mandatory label of a file.  It
# decides whether an install launched from that file may write to ordinary
# locations: a Low-integrity setup .exe is refused everywhere but its own
# sandbox.  Reading the label needs no special rights; writing one does, so
# the relabel steps below treat a refusal as information, not as noise.
function Get-IntegrityLabel([string]$Path) {
    $out = try { & icacls.exe $Path 2>&1 | Out-String } catch { '' }
    if ($out -match 'Low Mandatory Level') { return 'Low' }
    if ($out -match 'Medium Mandatory Level') { return 'Medium' }
    if ($out -match 'High Mandatory Level') { return 'High' }
    return 'None'
}

# --- toolchain ---------------------------------------------------------------

function Resolve-GoTool {
    $go = Get-Command go -ErrorAction SilentlyContinue
    if ($go) { return $go.Source }

    # The maintainer's sandbox keeps a self-contained Go next to the repo; fall
    # back to it so the script also works where PATH was never set up.
    $fallback = 'D:\client2api-lab\_tools\go\bin\go.exe'
    if (Test-Path $fallback) { return $fallback }
    throw 'go not found on PATH; install Go or add it to PATH.'
}

# windres is optional: it only adds the icon/version resource to the installer.
function Resolve-Windres {
    $found = Get-Command windres -ErrorAction SilentlyContinue
    if ($found) { return $found.Source }

    $candidates = @(
        'D:\client2api-lab\_tools\w64devkit\bin\windres.exe'
    )
    foreach ($candidate in $candidates) {
        if (Test-Path $candidate) { return $candidate }
    }
    return $null
}

$go       = Resolve-GoTool
$windres  = Resolve-Windres

# --- version ----------------------------------------------------------------

$mainGo = Join-Path $repoRoot 'cmd\client2api\main.go'
$match  = Select-String -Path $mainGo -Pattern '^var version = "([^"]+)"' -List
if (-not $match) { throw "could not read the version from $mainGo" }
$version = $match.Matches[0].Groups[1].Value

# VERSIONINFO wants four numeric fields; "0.1.0-dev" becomes "0.1.0.0".
$numeric = ($version -split '[^0-9]+' | Where-Object { $_ -ne '' } | Select-Object -First 3)
while ($numeric.Count -lt 3) { $numeric += '0' }
$versionNumeric = ($numeric -join '.') + '.0'

$setupName = "client2api-setup-$version.exe"
$setupPath = Join-Path $OutputDirectory $setupName

Write-Step "client2api $version  (numeric $versionNumeric)"
Write-Step "go     : $go"
Write-Step "windres: $(if ($windres) { $windres } else { 'not found (installer will use a generic icon)' })"

# --- stage the payload ------------------------------------------------------

Write-Step "staging the payload into $PayloadDirectory"

if (-not (Test-Path $PayloadDirectory)) {
    New-Item -ItemType Directory -Force -Path $PayloadDirectory | Out-Null
}

# Wipe staged output but keep the placeholder that keeps //go:embed happy in a
# fresh checkout.
Get-ChildItem -Force -Path $PayloadDirectory |
    Where-Object { $_.Name -ne '.gitkeep' } |
    ForEach-Object { Remove-Item -Recurse -Force -LiteralPath $_.FullName }

New-Item -ItemType Directory -Force -Path (Join-Path $PayloadDirectory 'configs') | Out-Null

if (-not $env:GOFLAGS) { $env:GOFLAGS = '-mod=mod' }
$env:CGO_ENABLED = '0'

$ldflags = "-s -w -X main.version=$version"

Write-Step 'building client2api.exe'
& $go build -trimpath -ldflags "$ldflags -H=windowsgui" -o (Join-Path $PayloadDirectory 'client2api.exe') ./cmd/client2api
if ($LASTEXITCODE -ne 0) { throw "go build ./cmd/client2api failed ($LASTEXITCODE)" }

Write-Step 'building probe.exe'
& $go build -trimpath -ldflags $ldflags -o (Join-Path $PayloadDirectory 'probe.exe') ./cmd/probe
if ($LASTEXITCODE -ne 0) { throw "go build ./cmd/probe failed ($LASTEXITCODE)" }

Write-Step 'building panelsmoke.exe'
# panelsmoke is a console program on purpose: it must print why a candidate
# failed.  The installer also ships it, so the same gate runs on the target
# machine before an upgrade touches the existing files.
& $go build -trimpath -ldflags '-s -w' -o (Join-Path $PayloadDirectory 'panelsmoke.exe') ./cmd/panelsmoke
if ($LASTEXITCODE -ne 0) { throw "go build ./cmd/panelsmoke failed ($LASTEXITCODE)" }

Copy-Item -Force (Join-Path $repoRoot 'README.md') $PayloadDirectory
Copy-Item -Force (Join-Path $repoRoot 'LICENSE')   $PayloadDirectory
Copy-Item -Force (Join-Path $installerDir 'client2api.ico') $PayloadDirectory

Copy-Item -Force (Join-Path $repoRoot 'configs\client2api.example.json') (Join-Path $PayloadDirectory 'configs')

if ($NoData) {
    # A shareable installer must not carry the operator's settings either: the
    # example config is exactly what the gateway writes on a fresh machine, so
    # "empty package" means no credentials and no operator state.
    Copy-Item -Force (Join-Path $repoRoot 'configs\client2api.example.json') (Join-Path $PayloadDirectory 'configs\client2api.json')
    New-Item -ItemType Directory -Force -Path (Join-Path $PayloadDirectory 'data') | Out-Null
    Write-Warning 'empty package (-NoData): no account credentials and no live config; import a bundle to restore state.'
} else {
    $liveConfig = Join-Path $repoRoot 'configs\client2api.json'
    if (Test-Path $liveConfig) {
        Copy-Item -Force $liveConfig (Join-Path $PayloadDirectory 'configs')
    } else {
        Write-Warning 'configs\client2api.json is missing; the installer ships the example and the gateway writes a fresh config on first run.'
        Copy-Item -Force (Join-Path $repoRoot 'configs\client2api.example.json') (Join-Path $PayloadDirectory 'configs\client2api.json')
    }
    $liveData = Join-Path $repoRoot 'data'
    if (-not (Test-Path $liveData)) { throw "data/ not found at $liveData; pass -NoData to build without it." }
    Copy-Item -Recurse -Force $liveData (Join-Path $PayloadDirectory 'data')
    Write-Warning 'data/ included: this setup .exe contains your live account credentials. Do not share it.'
}

# --- panel smoke test --------------------------------------------------------

# The packaging gate.  Nothing else in this build ever executes the panel's
# JavaScript: the server can answer 200 with the same bytes while a stray brace
# leaves every button dead.  0.1.5 shipped exactly that.  Starting the staged
# binary and loading /panel/ in a real browser is the only check that catches
# it, so the build refuses to produce an installer when the panel does not boot.
if ($SkipSmoke) {
    Write-Warning '-SkipSmoke: the panel smoke test was skipped; the installer will still self-check on the target machine.'
} else {
    Write-Step 'running the panel smoke test against the staged binary'
    & (Join-Path $PayloadDirectory 'panelsmoke.exe') -exe (Join-Path $PayloadDirectory 'client2api.exe')
    if ($LASTEXITCODE -ne 0) {
        throw "panel smoke test failed ($LASTEXITCODE); refusing to build an installer whose panel cannot boot"
    }
    $candidateHash = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $PayloadDirectory 'client2api.exe')).Hash.ToLowerInvariant()
    $markerPath = Join-Path $PayloadDirectory '.smoke-ok'
    [IO.File]::WriteAllText($markerPath, $candidateHash + "`n", (New-Object Text.UTF8Encoding($false)))
    Write-Step "recorded build smoke hash $candidateHash"
}

# --- icon + version resource ------------------------------------------------

$syso = Join-Path $installerDir 'setup\rsrc_windows_amd64.syso'
if ($windres) {
    Write-Step 'embedding the icon and VERSIONINFO resource'
    $iconPath = (Join-Path $installerDir 'client2api.ico').Replace('\', '\\')
    $manifestPath = (Join-Path $installerDir 'app.manifest').Replace('\', '\\')
    $rc = @"
#include <windows.h>
1 ICON "$iconPath"
1 24 "$manifestPath"
1 VERSIONINFO
FILEVERSION $($versionNumeric.Replace('.', ','))
PRODUCTVERSION $($versionNumeric.Replace('.', ','))
FILEFLAGSMASK 0x3fL
FILEFLAGS 0x0L
FILEOS 0x40004L
FILETYPE 0x1L
FILESUBTYPE 0x0L
BEGIN
  BLOCK "StringFileInfo"
  BEGIN
    BLOCK "080404b0"
    BEGIN
      VALUE "CompanyName", "client2api"
      VALUE "FileDescription", "client2api installer"
      VALUE "FileVersion", "$version"
      VALUE "InternalName", "client2api-setup"
      VALUE "OriginalFilename", "$setupName"
      VALUE "ProductName", "client2api"
      VALUE "ProductVersion", "$version"
    END
  END
  BLOCK "VarFileInfo"
  BEGIN
    VALUE "Translation", 0x804, 1200
  END
END
"@
    $rcPath = Join-Path $installerDir 'rsrc.rc'
    Set-Content -Path $rcPath -Value $rc -Encoding ASCII
    & $windres -i $rcPath -o $syso
    if ($LASTEXITCODE -ne 0) {
        Write-Warning "windres failed ($LASTEXITCODE); continuing without the icon resource."
        if (Test-Path $syso) { Remove-Item -Force $syso }
    }
} elseif (Test-Path $syso) {
    Remove-Item -Force $syso
}

# --- build the installer ----------------------------------------------------

New-Item -ItemType Directory -Force -Path $OutputDirectory | Out-Null

# Replace any previous build.  Dropping a Low mandatory label first only
# matters when this runs inside the sandboxed workspace that put one there;
# on an ordinary drive icacls answers "access is denied" for the relabel
# and there is nothing to drop.  Deleting is what has to work, so try that
# first and fall back to the label shuffle only when the file survived.
if (Test-Path $setupPath) {
    Remove-Item -Force $setupPath -ErrorAction SilentlyContinue
}
if (Test-Path $setupPath) {
    try { & icacls.exe $setupPath /setintegritylevel Low 2>&1 | Out-Null } catch { }
    Remove-Item -Force $setupPath -ErrorAction SilentlyContinue
}
if (Test-Path $setupPath) {
    throw "cannot replace $setupPath; close whatever is holding it, or delete it by hand"
}

Write-Step 'building the installer'
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
try {
    & $go build -trimpath -ldflags $ldflags -o $setupPath ./installer/setup
    if ($LASTEXITCODE -ne 0) { throw "go build ./installer/setup failed ($LASTEXITCODE)" }
} finally {
    Remove-Item Env:\GOOS -ErrorAction SilentlyContinue
    Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue
}

# Ship the setup .exe at Medium integrity.  Windows starts a process at the
# executable's integrity level whenever that is lower than the parent's,
# and a Low setup .exe cannot write to ordinary Medium locations such as
# the user profile or a data drive: the shell answers every "New Folder"
# and every install with "access denied -- continue as administrator".
# Relabelling is therefore mandatory when the build ran inside the
# sandboxed workspace, and a no-op on an ordinary drive where the file
# already has no label.
if ((Get-IntegrityLabel $setupPath) -eq 'Low') {
    try { & icacls.exe $setupPath /setintegritylevel Medium 2>&1 | Out-Null } catch { }
    if ((Get-IntegrityLabel $setupPath) -eq 'Low') {
        throw "$setupPath is still Low integrity; an install launched from it would be denied write access"
    }
}

$size = [math]::Round((Get-Item $setupPath).Length / 1MB, 1)
Write-Host ''
Write-Host "installer: $setupPath  ($size MB)" -ForegroundColor Green
Write-Host "sha256   : $((Get-FileHash $setupPath -Algorithm SHA256).Hash)" -ForegroundColor Green
