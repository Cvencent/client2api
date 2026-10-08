const fs = require("fs");

const path = process.argv[2];
const pattern = process.argv[3];
const before = Number(process.argv[4] || 700);
const after = Number(process.argv[5] || 1400);
const limit = Number(process.argv[6] || 200);

const text = fs.readFileSync(path, "utf8");
const re = new RegExp(pattern, "gi");
let match;
let count = 0;
while ((match = re.exec(text)) && count < limit) {
  const start = Math.max(0, match.index - before);
  const end = Math.min(text.length, match.index + after);
  console.log(`\n--- ${match[0]} @${match.index} ---\n${text.slice(start, end)}`);
  count++;
}
if (count === 0) {
  console.log("no matches");
}
