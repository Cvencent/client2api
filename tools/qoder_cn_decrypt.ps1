# Read-only helper: unwraps the DPAPI-protected AES key that Qoder CN's Electron
# safeStorage keeps in "<UserDataDir>\Local State" and prints it as lowercase hex.
# Nothing is modified; tools/qoder_aes_open.js consumes this key in memory to
# open auth.v1.dat.
#
# Usage: powershell -NoProfile -File tools/qoder_cn_decrypt.ps1 -UserDataDir "$env:APPDATA\com.qodercn.app.stable"
param(
  [Parameter(Mandatory = $true)][string]$UserDataDir
)

$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Security

$statePath = Join-Path $UserDataDir 'Local State'
if (-not (Test-Path -LiteralPath $statePath)) { throw "Local State not found: $statePath" }

$state = Get-Content -LiteralPath $statePath -Raw -Encoding UTF8 | ConvertFrom-Json
$enc = [Convert]::FromBase64String($state.os_crypt.encrypted_key)
if ($enc.Length -lt 5) { throw 'encrypted_key too short' }
if ([Text.Encoding]::ASCII.GetString($enc, 0, 5) -ne 'DPAPI') { throw 'encrypted_key is not a DPAPI blob' }

$dpapiBlob = New-Object byte[] ($enc.Length - 5)
[Array]::Copy($enc, 5, $dpapiBlob, 0, $dpapiBlob.Length)
$key = [Security.Cryptography.ProtectedData]::Unprotect($dpapiBlob, $null, [Security.Cryptography.DataProtectionScope]::CurrentUser)
Write-Host "unwrapped key bytes=$($key.Length)" -ForegroundColor DarkGray
($key | ForEach-Object { $_.ToString('x2') }) -join ''
