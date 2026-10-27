# tools

Developer-side helpers.  Nothing here ships in the gateway binary, and none of
it is built by `go build ./...`, `go vet ./...` or `go test ./...` — see the
Go harnesses below for why.

## Go harnesses

Both are throwaway-style probes that are still worth keeping around: they drive
real code (the panel handler, the WorkBuddy browser login) against inputs we
cannot reproduce in a unit test.  They used to live in `cmd/*-temp/`, where
being `package main` under `cmd/` dragged them into the default build surface —
so a dependency bump or an interface change would first break a program nobody
maintains, at a location unrelated to the product code.

They now live under `tools/<name>/` and every one carries:

```go
//go:build ignore
```

That build constraint is the whole mechanism: the file is excluded from
`go vet ./...` / `go build ./...` / `go test ./...`, yet `go run <file>` still
compiles and runs it.  It has to be `go run ./tools/<name>/main.go` (the file
form) — naming the directory, `go run ./tools/<name>`, is rejected with "build
constraints exclude all Go files", because a package with only ignored files
has no buildable package behind it.

`cmd/*-temp/` is now never legitimate; a guard in `installer/probe_surface_test.go`
fails if a buildable Go file reappears under a directory like that.

### `tools/uilook/main.go` — render a panel view to a PNG

Boots the real `internal/panel` handler against a fake registry in a headless
browser and screenshots one view (or the account-model dialog) so a UI change
can be reviewed without a live gateway.  Needs a Chromium-family browser
(Microsoft Edge or Google Chrome) installed.

```sh
go run ./tools/uilook/main.go -view accounts -out uilook.png
go run ./tools/uilook/main.go -editable -theme light -width 1440 -height 960
```

`-view` takes any panel view id (accounts, chat, usage, alerts, credits,
packages, taskscenter, models, platforms, config, logs).  Without `-view` it
drives the page straight into the account-model dialog.  Other flags: `-theme`
(dark|light|system), `-width`, `-height`, `-out`.

### `tools/wb-autologin-check/main.go` — live WorkBuddy CN auto-login

Runs `Clients/AutoLoginProvider` end to end against the real WorkBuddy CN site
with a rented SMS number, printing the job's progress log and the final state.
Needs a live SMS-platform credential and the number to reuse; the credential is
read from the environment, never written down.

```sh
WB_REAL_PHONE=13800000000 WB_REAL_TOKEN=<sms-platform-token> \
  go run ./tools/wb-autologin-check/main.go
```

(On Windows PowerShell, set `WB_REAL_PHONE` / `WB_REAL_TOKEN` with `$env:`
first.)

## Node / PowerShell helpers

Read-only reverse-engineering helpers for vendor clients; they never modify
what they read.

| file | what it does |
| --- | --- |
| `asar_extract.js` | extract an Electron `.asar` archive: `node tools/asar_extract.js <archive.asar> <outDir> [filter]` |
| `qoder_cn_decrypt.ps1` | unwrap Qoder CN's DPAPI-protected AES key: `powershell -NoProfile -File tools/qoder_cn_decrypt.ps1 -UserDataDir "$env:APPDATA\com.qodercn.app.stable"` |
| `qoder_aes_open.js` | open `auth.v1.dat` with the key above: `node tools/qoder_aes_open.js <auth.v1.dat> <keyHexFile>` |
| `qoder_extract_paths.js` | pull API paths / endpoints out of a file: `node tools/qoder_extract_paths.js <file> [filter...]` |
| `qoder_probe.js` | show regex matches with surrounding context: `node tools/qoder_probe.js <file> <pattern> [before] [after] [limit]` |
| `qoder_strings.js` | deduped regex matches (strings dump): `node tools/qoder_strings.js <file> <pattern> [limit]` |
