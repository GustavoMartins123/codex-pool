import { readFileSync } from "node:fs";

const unpatchedRanges = new Map([["https://github.com/advisories/GHSA-fc5q-39xc-vjpf", "<=4.0.7"]]);

const report = JSON.parse(readFileSync(process.argv[2], "utf8"));
if (!report.vulnerabilities) {
  console.error(`npm audit report has no vulnerabilities section: ${JSON.stringify(report.error ?? report)}`);
  process.exit(1);
}
const vulnerabilities = report.vulnerabilities;
const advisories = (name, seen = new Set()) => {
  if (seen.has(name)) return [];
  seen.add(name);
  return (vulnerabilities[name]?.via ?? []).flatMap((via) => (typeof via === "string" ? advisories(via, seen) : [via]));
};
const blocking = Object.keys(vulnerabilities).filter((name) => {
  const found = advisories(name);
  return found.length === 0 || found.some((advisory) => unpatchedRanges.get(advisory.url) !== advisory.range);
});
for (const name of blocking) console.error(`npm audit: ${name} (${vulnerabilities[name].severity})`);
process.exit(blocking.length > 0 ? 1 : 0);
