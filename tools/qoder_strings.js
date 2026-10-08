const fs = require("fs");

const path = process.argv[2];
const pattern = process.argv[3];
const limit = Number(process.argv[4] || 200);
const text = fs.readFileSync(path, "utf8");
const re = new RegExp(pattern, "gi");
const seen = new Set();
let match;
let count = 0;
while ((match = re.exec(text)) && count < limit) {
  const value = match[0];
  if (value.length > 400) continue;
  if (seen.has(value)) continue;
  seen.add(value);
  console.log(value);
  count++;
}
