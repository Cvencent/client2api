<#
.SYNOPSIS
    Rebuilds the vendored speech-input ffmpeg.exe in this directory.

.DESCRIPTION
    Not part of a normal release build -- ffmpeg.exe is committed, and only
    needs rebuilding when FFmpeg or libopus moves to a new version.  See
    README.md in this directory for why the project vendors one at all.

    Two Windows-specific details are baked in below, both discovered the hard
    way:

      * SHELL must be the 8.3 short path.  w64devkit's make is a native MinGW
        build; when a recipe shells out (every libtool invocation does) it
        resolves /bin/sh to "C:/Program Files/Git/usr/bin/sh.exe" and passes it
        to CreateProcess unquoted, so the space truncates the path and the
        build dies with "C:/Program: No such file or directory".

      * x86 assembly is disabled.  The recipe otherwise wants nasm, and
        --disable-x86asm keeps the build reproducible on a machine that only
        has the w64devkit toolchain.

.PARAMETER OutputDirectory
    Where to write ffmpeg.exe.  Defaults to this directory.

.EXAMPLE
    pwsh -File third_party\ffmpeg\build.ps1
#>
[CmdletBinding()]
param(
    [string] $OutputDirectory
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$here = Split-Path -Parent $MyInvocation.MyCommand.Path
if (-not $OutputDirectory) { $OutputDirectory = $here }

# Pinned inputs.  Bump all three together and record the new SHA256.
$ffmpegVersion = '8.1'
$opusVersion  = '1.5.2'

$tools = 'D:\client2api-lab\_tools'
$make  = Join-Path $tools 'w64devkit\bin\make.exe'
$bash  = 'C:\Program Files\Git\bin\bash.exe'

foreach ($need in @($make, $bash)) {
    if (-not (Test-Path $need)) { throw "missing build tool: $need" }
}

$work = Join-Path ([System.IO.Path]::GetTempPath()) "c2a-ffmpeg-$ffmpegVersion"
New-Item -ItemType Directory -Force -Path $work | Out-Null

# Drive the POSIX toolchain through Git Bash.  Paths inside the bash -lc string
# are MSYS-style (/c/...); $work is converted once, here.
$msys = $work -replace '\\', '/' -replace '^([A-Za-z]):', '/$1'.ToLower()
$env:PATH = "$(Join-Path $tools 'w64devkit\bin');$env:PATH"

function Invoke-Bash {
    param([string] $Command)
    & $bash -lc $Command
    if ($LASTEXITCODE -ne 0) { throw "bash failed ($LASTEXITCODE): $Command" }
}

if (-not (Test-Path (Join-Path $work "opus-$opusVersion"))) {
    Write-Host "==> fetching opus $opusVersion"
    Invoke-Bash "cd '$msys' && curl --ssl-no-revoke -fsSLO 'https://downloads.xiph.org/releases/opus/opus-$opusVersion.tar.gz' && tar xf opus-$opusVersion.tar.gz"
}

if (-not (Test-Path (Join-Path $work "ffmpeg-$ffmpegVersion"))) {
    Write-Host "==> fetching ffmpeg $ffmpegVersion"
    Invoke-Bash "cd '$msys' && curl --ssl-no-revoke -fsSLO 'https://ffmpeg.org/releases/ffmpeg-$ffmpegVersion.tar.xz' && tar xf ffmpeg-$ffmpegVersion.tar.xz"
}

# The 8.3 short path is what keeps libtool's shell invocations working; see the
# header comment.
$shortShell = 'C:/PROGRA~1/Git/usr/bin/sh.exe'

Write-Host "==> building libopus (static)"
Invoke-Bash "cd '$msys/opus-$opusVersion' && SHELL=/bin/sh CONFIG_SHELL=/bin/sh ./configure --disable-shared --enable-static --disable-extra-programs --disable-doc --prefix='$msys/opus-install' >/dev/null 2>&1"
Invoke-Bash "cd '$msys/opus-$opusVersion' && '$make' -j4 SHELL='$shortShell' >/dev/null 2>&1"
Invoke-Bash "cd '$msys/opus-$opusVersion' && '$make' install SHELL='$shortShell' >/dev/null 2>&1"

    Write-Host "==> configuring ffmpeg (speech decode + opus encode)"
# --disable-everything first, then re-enable exactly what encodeWithFFmpeg in
# clients/doubaoime/audio.go touches.  Adding a component here means adding it
# to that Go function too, or the module will fail at runtime.
$configure = @(
    './configure'
    '--disable-everything'
    '--disable-doc --disable-debug --disable-ffplay'
    '--disable-network --disable-autodetect --disable-x86asm'
    '--disable-avdevice'
    '--enable-libopus --enable-encoder=libopus'
    '--enable-swresample --enable-filter=aresample'
    '--enable-muxer=ogg --enable-muxer=opus'
    '--enable-muxer=pcm_s16le --enable-encoder=pcm_s16le'
    '--enable-demuxer=wav --enable-demuxer=pcm_s16le'
    '--enable-demuxer=mp3 --enable-demuxer=mov --enable-demuxer=ogg'
    '--enable-demuxer=matroska --enable-demuxer=flac'
    '--enable-decoder=pcm_s16le --enable-decoder=mp3 --enable-decoder=aac'
    '--enable-decoder=flac --enable-decoder=vorbis --enable-decoder=opus'
    '--enable-parser=aac --enable-parser=flac --enable-parser=mpegaudio --enable-parser=opus --enable-parser=vorbis'
    '--enable-protocol=file --enable-protocol=pipe'
    '--enable-version3'
    "prefix='$msys/ffmpeg-install'"
    '> /dev/null'
) -join ' '
Invoke-Bash "cd '$msys/ffmpeg-$ffmpegVersion' && PKG_CONFIG_PATH='$msys/opus-install/lib/pkgconfig' SHELL=/bin/sh CONFIG_SHELL=/bin/sh $configure"

Write-Host "==> building ffmpeg"
Invoke-Bash "cd '$msys/ffmpeg-$ffmpegVersion' && '$make' -j4 SHELL='$shortShell' >/dev/null 2>&1"

$built = Join-Path $work "ffmpeg-$ffmpegVersion\ffmpeg.exe"
if (-not (Test-Path $built)) { throw "ffmpeg.exe was not produced at $built" }

Copy-Item -Force $built (Join-Path $OutputDirectory 'ffmpeg.exe')

$hash = (Get-FileHash (Join-Path $OutputDirectory 'ffmpeg.exe') -Algorithm SHA256).Hash.ToLowerInvariant()
Set-Content -LiteralPath (Join-Path $OutputDirectory 'ffmpeg.sha256') -Value "$hash  ffmpeg.exe" -NoNewline
Write-Host "==> ffmpeg.exe  $((Get-Item (Join-Path $OutputDirectory 'ffmpeg.exe')).Length) bytes"
Write-Host "==> sha256     $hash"
Write-Host "==> recorded in ffmpeg.sha256; update README.md if the version moved."
