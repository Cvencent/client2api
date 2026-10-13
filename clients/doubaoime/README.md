# doubaoime (Doubao Input Method)

`doubaoime` turns the **Doubao Input Method** (豆包输入法, Android) online speech
service into an OpenAI-compatible transcription source. It is a separate
platform from `clients/doubao` (the Doubao chat web app) because the credentials,
the transport and the capability are all different: the input method speaks a
private Android ASR WebSocket protocol with its own device registration, not the
chat web API.

The module serves exactly one thing the rest of the gateway cannot express:

```text
POST /v1/audio/transcriptions   (multipart/form-data)
    -> WAV 16 kHz mono PCM (other supported containers are decoded with FFmpeg)
    -> FFmpeg Ogg Opus (20 ms frames)
    -> Doubao IME private ASR WebSocket protocol
    -> {"text": "..."}
```

## Enabling it

The module registers a platform named `doubaoime`. It needs no account: the
first transcription request registers a device against Doubao and caches the
device id and app key. Add an (optional) block to `configs/client2api.json`:

```json
{
  "clients": {
    "doubaoime": {
      "ffmpeg_path": "ffmpeg",
      "models": ["whisper-1"]
    }
  }
}
```

A missing block is fine — every value has a default. The module is also
compiled in by the blank import in `clients/all/all.go`.

### FFmpeg is required, and the Windows installer bundles one

The Doubao input method transports speech as Opus, and this project has no
in-process Opus encoder (it is a `CGO_ENABLED=0` build, and the only pure-Go
Opus implementation ships a decoder but no importable encoder). The module
therefore shells out to FFmpeg to turn the input WAV into 20 ms Ogg Opus
frames. It looks for the encoder in this order:

1. `ffmpeg_path` from the config block (an absolute path or a command name),
2. an `ffmpeg.exe` next to the running `client2api.exe`,
3. `ffmpeg` on `PATH`.

The bundled copy deliberately outranks `PATH`: it is checksum-pinned and covered
by the test suite, whereas a system FFmpeg may be a stripped build without
libopus.

**The Windows installer ships a bundled copy**, so step 2 already succeeds after
an ordinary install and no system FFmpeg is needed. It is a purpose-built
~3.4 MB LGPL build carrying the Opus encoder, the Ogg/Opus and raw-PCM muxers,
and decoders for the common transcription upload containers -- see
`third_party/ffmpeg/README.md` for what was kept, why, and how to rebuild it.
Its SHA256 is pinned in `ffmpeg.sha256`, checked when the installer packages it,
and asserted by the module's tests, so a tampered or mispackaged copy fails
before it can reach a user.

The portable archives (Linux/macOS/Windows zip) do **not** bundle a binary --
install FFmpeg through your package manager there.

When no FFmpeg is found at all the platform still starts, `Status()` explains
the problem, and `/healthz` reports it as not servable. Every other platform is
unaffected.

## Calling it

It plugs into the gateway's standard transcription endpoint, so any
OpenAI-compatible client that supports `POST /v1/audio/transcriptions` works:

```powershell
curl.exe -s -X POST http://127.0.0.1:8790/v1/audio/transcriptions `
  -H "Authorization: Bearer <gateway-api-key>" `
  -F "file=@speech.wav" `
  -F "model=doubaoime/whisper-1" `
  -F "response_format=json"
```

`GET /v1/models` lists the transcription models alongside the chat models, each
tagged with `extra.kind = "transcription"`, so a client can discover the routing
name without being told. A bare `whisper-1` also resolves when only one
transcription provider serves it.

The endpoint is multipart, with the same shape as OpenAI's
`POST /v1/audio/transcriptions`. The upload can be WAV, FLAC, MP3, MP4/M4A,
OGG, or WebM. WAV is decoded in-process; every other container is decoded by
FFmpeg and then resampled to 16 kHz mono before it reaches Doubao.

`response_format` accepts `json` (the default), `text`, `srt`, `verbose_json`,
and `vtt`. `verbose_json` carries the audio duration and one whole-utterance
segment; Doubao does not expose real word timestamps, so `words` is omitted
rather than fabricated. `diarized_json`, `timestamp_granularities`,
`include=logprobs`, streaming, diarization, language lists, keyword biasing,
chunking controls, prompt biasing, and non-zero temperature are rejected with
HTTP 400 because this upstream cannot implement them. In particular, a non-empty
`prompt` is refused rather than silently accepted: Doubao cannot use it for word
hints or style control.

## Config

| key | default | meaning |
|---|---|---|
| `register_url` | `https://log.snssdk.com/service/2/device_register/` | device registration endpoint |
| `settings_url` | `https://is.snssdk.com/service/settings/v3/` | settings / app-key endpoint |
| `websocket_url` | `wss://frontier-audio-ime-ws.doubao.com/ocean/api/v1/ws` | ASR WebSocket endpoint |
| `user_agent` | Doubao IME Android UA | handshake `User-Agent` |
| `ffmpeg_path` | auto-discovery | FFmpeg executable for Opus encoding |
| `models` | `["whisper-1"]` | model ids this platform advertises |
| `request_timeout` | `8s` | per-request timeout for register/settings/frames |
| `start_timeout` | `8s` | how long to wait for `TaskStarted` / `SessionStarted` |
| `result_timeout` | `30s` | how long to wait for the final transcript |
| `total_timeout` | `55s` | whole-request ceiling (stays under the gateway's) |

Environment overrides: `CLIENT2API_DOUBAOIME_REGISTER_URL`,
`..._SETTINGS_URL`, `..._WEBSOCKET_URL`, `..._USER_AGENT`,
`..._FFMPEG_PATH`.

## Credentials

There is no login. The module generates a stable device identity (a CDID UUID,
an OpenUDID and a ClientUDID), registers it, exchanges it for an `app_key`, and
stores the pair in `data/doubaoime/doubao_credentials.json` (mode `0600`,
written atomically). An upstream `401`/`403` triggers one re-registration before
the failure is surfaced. Tokens, cookies and audio are never logged.

## Input rules

- WAV must be RIFF/WAVE with 16-bit PCM samples. FLAC, MP3, MP4/M4A, OGG and
  WebM are decoded by FFmpeg; a format with no matching decoder is rejected
  with HTTP 400.
- Multi-channel and non-16 kHz input is downmixed and resampled to 16 kHz mono
  before the Opus encoding step.
- An all-silent clip, or an upstream reply with no text, becomes HTTP 422.

## Provenance and risk

The protocol here comes from reverse-engineering the Doubao input method's
Android client, not from a published API (see
`docs/upstream/doubaoime.md`). Doubao may change its fields, risk controls or
protocol at any time; treat this platform as best-effort and personal-use only.
The gateway never stores or replays your voice beyond the single request.
