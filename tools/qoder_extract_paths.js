const fs = require("fs");

const path = process.argv[2];
const text = fs.readFileSync(path, "utf8");
const re = /(?:path|url|endpoint)\s*:\s*["'`]([^"'`\n]{3,300})["'`]|["'`](\/api\/[^"'`\n]{3,300})["'`]/g;
const filters = process.argv.slice(3).map((value) => value.toLowerCase());
const seen = new Set();
let match;
while ((match = re.exec(text))) {
  const value = match[1] || match[2];
  if (!value) continue;
  const lower = value.toLowerCase();
  if (filters.length && !filters.some((filter) => lower.includes(filter))) continue;
  if (seen.has(value)) continue;
  seen.add(value);
  console.log(value);
}
