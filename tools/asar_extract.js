// Minimal read-only ASAR extractor used for reverse-engineering vendor
// clients. Usage: node asar_extract.js <archive.asar> <outDir> [filter]
// The filter, when given, is a substring matched against each entry's path;
// only matching files are written. Nothing under the vendor install is touched.
const fs = require('fs');
const path = require('path');
const zlib = require('zlib');

function readHeader(fd) {
  const sizeBuf = Buffer.alloc(16);
  fs.readSync(fd, sizeBuf, 0, 16, 0);
  // Pickle: uint32 payloadSize, uint32 headerObjectSize, uint32 headerSize, uint32 jsonSize
  const headerSize = sizeBuf.readUInt32LE(12);
  const headerBuf = Buffer.alloc(headerSize);
  fs.readSync(fd, headerBuf, 0, headerSize, 16);
  const start = headerBuf.indexOf(0x7b);
  let end = headerBuf.length;
  while (end > start && headerBuf[end - 1] !== 0x7d) end--;
  const json = headerBuf.slice(start, end).toString('utf8');
  const baseOffset = 16 + headerSize;
  return { header: JSON.parse(json), baseOffset };
}

function walk(header, prefix, out) {
  for (const [name, node] of Object.entries(header.files || {})) {
    const full = prefix ? prefix + '/' + name : name;
    if (node.files) walk(node, full, out);
    else out.push({ path: full, size: node.size || 0, offset: String(node.offset || '0') });
  }
}

function main() {
  const [arc, outDir, filter] = process.argv.slice(2);
  if (arc === '--list') {
    const fd = fs.openSync(outDir, 'r');
    const { header } = readHeader(fd);
    const entries = [];
    walk(header, '', entries);
    for (const e of entries) console.log(`${e.size}\t${e.path}`);
    fs.closeSync(fd);
    return;
  }
  if (!arc || !outDir) {
    console.error('usage: node asar_extract.js <archive.asar> <outDir> [filter]');
    process.exit(2);
  }
  const fd = fs.openSync(arc, 'r');
  const { header, baseOffset } = readHeader(fd);
  const entries = [];
  walk(header, '', entries);
  let written = 0;
  for (const e of entries) {
    if (filter && !e.path.includes(filter)) continue;
    const start = baseOffset + Number(e.offset);
    const buf = Buffer.alloc(e.size);
    fs.readSync(fd, buf, 0, e.size, start);
    const dest = path.join(outDir, e.path);
    fs.mkdirSync(path.dirname(dest), { recursive: true });
    fs.writeFileSync(dest, buf);
    written++;
  }
  fs.closeSync(fd);
  console.log(`entries=${entries.length} written=${written}`);
}

main();
