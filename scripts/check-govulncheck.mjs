import { readFileSync } from "node:fs";

const text = readFileSync(process.argv[2], "utf8");
const called = new Set();
let messages = 0;
let depth = 0;
let start = 0;
let inString = false;
let escaped = false;
for (let i = 0; i < text.length; i++) {
  const c = text[i];
  if (inString) {
    if (escaped) escaped = false;
    else if (c === "\\") escaped = true;
    else if (c === '"') inString = false;
    continue;
  }
  if (c === '"') inString = true;
  else if (c === "{" && depth++ === 0) start = i;
  else if (c === "}" && --depth === 0) {
    messages++;
    const finding = JSON.parse(text.slice(start, i + 1)).finding;
    const frame = finding?.trace?.[0];
    if (frame?.function) called.add(`${finding.osv} in ${frame.module}@${frame.version}, fixed in ${finding.fixed_version}`);
  }
}
if (messages === 0) {
  console.error("govulncheck report is empty");
  process.exit(1);
}
for (const line of called) console.error(`govulncheck: ${line}`);
process.exit(called.size > 0 ? 1 : 0);
