# Vendored ffmpeg (Opus-only)

`ffmpeg.exe` is a **minimal, statically linked FFmpeg** built only for this
project's speech-transcription path. It is not a general-purpose FFmpeg.

## Why it exists

`clients/doubaoime` turns a WAV upload into 20 ms Ogg Opus frames, because the
Doubao input method's private ASR WebSocket protocol speaks `speech_opus`. The
gateway is built with `CGO_ENABLED=0` (so it cross-compiles to five platforms
from one pure-Go tree), and there is no pure-Go Opus **encoder** --
`github.com/pion/opus` ships a decoder only, and its encoder lives in an
`internal/` package that cannot be imported. So an external FFmpeg is the only
way to encode.

Rather than require every operator to install one, the Windows installer ships
this binary. `clients/doubaoime.findFFmpeg` already looks for an `ffmpeg.exe`
next to the running `client2api.exe`, so no code change was needed to pick it
up.

## What was kept

A stock `ffmpeg.exe` is ~128 MB. This build is **1.95 MB** because configure
was told to keep only what the transcription path touches:

    --disable-everything
    --enable-libopus        --enable-encoder=libopus
    --enable-muxer=ogg      --enable-muxer=opus
    --enable-demuxer=pcm_s16le --enable-demuxer=wav
    --enable-decoder=pcm_s16le
    --enable-protocol=file  --enable-protocol=pipe

`muxer=opus` is a separate muxer from `muxer=ogg` in FFmpeg's source (both
live in `oggenc.c`), and the module passes `-f opus`; enabling only `ogg`
produces a binary that rejects the module's own command line. Likewise
`pcm_s16le` needs both the demuxer and the decoder, because the module feeds raw
PCM through stdin.

## Licence

LGPL v3 or later (`--enable-version3`). The stock GPL builds were deliberately
avoided: a GPL FFmpeg redistributed inside this installer would put the whole
package under GPL. LGPL keeps client2api's own licence unaffected. FFmpeg's
source is available from <https://ffmpeg.org/download.html>; libopus (BSD) from
<https://downloads.xiph.org/releases/opus/>.

## Rebuilding

    pwsh -File third_party\ffmpeg\build.ps1

The script pins every input version and prints the SHA256 to compare against
`ffmpeg.sha256`. It is not part of a normal `build.ps1` run -- the binary is
committed, and only needs rebuilding when FFmpeg or libopus moves to a new
version.

## Verifying the committed binary

The binary is committed on purpose (it is the distributable artefact), but its
integrity is pinned so a tampered copy is obvious:

    Get-FileHash third_party\ffmpeg\ffmpeg.exe -Algorithm SHA256

and its output is parsed by the module's own parser in
`clients/doubaoime/audio_test.go`, so a binary that cannot produce frames the
protocol accepts fails the test suite rather than failing at runtime on an
operator's machine.