// One-off AES-GCM opener for Qoder CN's auth.v1.dat.
// Usage: node qoder_aes_open.js <auth.v1.dat> <keyHexFile>
// The 32-byte key comes from DPAPI-unwrapping Local State's encrypted_key
// (see tools/qoder_cn_unwrap_key.ps1). Nothing is written.
const fs = require('fs');
const crypto = require('crypto');

const [blobPath, keyPath] = process.argv.slice(2);
if (!blobPath || !keyPath) {
  console.error('usage: node qoder_aes_open.js <auth.v1.dat> <keyHexFile>');
  process.exit(2);
}
const blob = fs.readFileSync(blobPath);
const key = Buffer.from(fs.readFileSync(keyPath, 'utf8').trim(), 'hex');
if (key.length !== 32) {
  console.error(`key length ${key.length}, want 32`);
  process.exit(3);
}

function attempt(prefix, nonceLen, tagLen) {
  const min = prefix + nonceLen + tagLen + 1;
  if (blob.length < min) return null;
  const nonce = blob.subarray(prefix, prefix + nonceLen);
  const tag = blob.subarray(blob.length - tagLen);
  const ct = blob.subarray(prefix + nonceLen, blob.length - tagLen);
  try {
    const d = crypto.createDecipheriv('aes-256-gcm', key, nonce, { authTagLength: tagLen });
    d.setAuthTag(tag);
    const out = Buffer.concat([d.update(ct), d.final()]);
    return out.toString('utf8');
  } catch {
    return null;
  }
}

const layouts = [];
for (const prefix of [0, 1, 2, 3, 4, 5, 12, 16]) {
  for (const nonceLen of [12, 16]) {
    for (const tagLen of [16]) layouts.push([prefix, nonceLen, tagLen]);
  }
}
for (const [prefix, nonceLen, tagLen] of layouts) {
  const out = attempt(prefix, nonceLen, tagLen);
  if (out && out.trimStart().startsWith('{')) {
    console.error(`opened with prefix=${prefix} nonce=${nonceLen} tag=${tagLen}`);
    process.stdout.write(out);
    process.exit(0);
  }
}
console.error('no layout matched');
process.exit(1);
