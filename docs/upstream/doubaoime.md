# Doubao Input Method ASR (upstream notes)

These notes describe the private protocol `clients/doubaoime` speaks. They were
reconstructed from observed behaviour of the Doubao Input Method (豆包输入法)
Android client (`com.bytedance.android.doubaoime`, `aid=401734`) — **not** from a
published API. Doubao may change any of it, including field numbers or risk
controls, at any time.

## Fixed client identity

| field | value |
|---|---|
| `aid` | `401734` |
| `app_name` | `oime` |
| `package` | `com.bytedance.android.doubaoime` |
| `version_code` | `100102018` |
| `version_name` | `1.1.2` |
| device registration host | `log.snssdk.com` |
| settings / token host | `is.snssdk.com` |
| ASR WebSocket host | `frontier-audio-ime-ws.doubao.com` |

## 1. Device registration

`POST https://log.snssdk.com/service/2/device_register/` with a JSON body:

```json
{
  "magic_tag": "ss_app_log",
  "header": { "aid": 401734, "app_name": "oime", "cdid": "...", "openudid": "...", "clientudid": "...", "...": "..." },
  "_gen_time": 1700000000000
}
```

The response is searched for a `device_id`. Three identifiers are generated once
and reused: `cdid` (a UUID), `clientudid` (a UUID) and `openudid` (16 hex
characters).

## 2. App key (token)

`POST https://is.snssdk.com/service/settings/v3/` with `body=null` and header
`x-ss-stub: uppercase(md5("body=null"))`. The **same cookie jar** as the
registration call must be used, because the response carries session cookies.
The response is searched for an `app_key`.

## 3. ASR WebSocket

`wss://frontier-audio-ime-ws.doubao.com/ocean/api/v1/ws?aid=401734&device_id=<id>`
with headers `proto-version: v2`, `x-custom-keepalive: true` and the Doubao IME
User-Agent. Frames are binary protobuf.

Request fields:

| # | field |
|---|---|
| 2 | token (app key) |
| 3 | service name (`ASR`) |
| 5 | method name |
| 6 | payload (JSON) |
| 7 | audio data |
| 8 | request id |
| 9 | frame state varint |

Response fields observed:

| # | field |
|---|---|
| 1 / 8 | request id |
| 2 | task id |
| 3 | service name |
| 4 | message type |
| 5 | status code (varint) |
| 6 | status message / payload |
| 7 | result JSON / audio |

Sequence: `StartTask` -> `TaskStarted`, `StartSession` -> `SessionStarted`, a run
of `TaskRequest` frames (first `frame_state=1`, middle `3`, last `9`),
`FinishSession`, then result frames ending in `SessionFinished`.

`StartSession` payload:

```json
{
  "audio_info": { "channel": 1, "format": "speech_opus", "sample_rate": 16000 },
  "enable_punctuation": true,
  "enable_speech_rejection": false,
  "extra": {
    "app_name": "com.android.chrome",
    "cell_compress_rate": 8,
    "did": "<device_id>",
    "enable_asr_threepass": true,
    "enable_asr_twopass": true,
    "input_mode": "tool"
  }
}
```

## 4. Audio

Speech is carried as Opus. This module takes the standard WAV upload, downmixes
and resamples to 16 kHz mono, and uses FFmpeg to produce Ogg Opus (the Windows
installer ships an Opus-only LGPL build for exactly this step; see
`third_party/ffmpeg/README.md`). The Ogg pages
are parsed and every non-header packet (skipping `OpusHead` / `OpusTags`) becomes
one audio frame.

## 5. Results

Result frames carry JSON like:

```json
{ "results": [ { "text": "请帮我把首页", "is_interim": false, "is_vad_finished": true } ] }
```

Final segments are preferred; if none arrived, the interim segments are used.
Adjacent duplicates/prefixes are merged and empty output becomes HTTP 422.
