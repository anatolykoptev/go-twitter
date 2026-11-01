// Reduces a saved logged-out x.com /i/jf/ page to the elements xtid reads, so
// nothing session-specific (guest cookies, nonces, tokens) is committed:
//   - the twitter-site-verification meta tag,
//   - the webpack runtime's chunk-name/chunk-hash maps, trimmed to a few entries
//     (always including ondemand.s),
//   - the four loading-x-anim SVGs, verbatim.
//
//   node reduce-live-page.mjs <raw-page.html> <reduced-page.html>
import fs from "node:fs";

const [inPath, outPath] = process.argv.slice(2);
const raw = fs.readFileSync(inPath, "utf8");

const meta = raw.match(/<meta[^>]*name=["']twitter-site-verification["'][^>]*>/);
if (!meta) throw new Error("verification meta not found");

const maps = raw.match(/\(\(\{([^{}]*?\b\d+:"ondemand\.s"[^{}]*)\}\)\[e\]\|\|e\)\+"\."\+\(\{([^{}]*)\}\)\[e\]/);
if (!maps) throw new Error("webpack chunk maps not found");
const entries = (s) => [...s.matchAll(/(\d+):"([^"]*)"/g)].map((m) => [m[1], m[2]]);
const names = entries(maps[1]);
const hashes = new Map(entries(maps[2]));
const odId = names.find(([, n]) => n === "ondemand.s")[0];
const keep = new Set([odId, names[0][0], names[1][0], names[names.length - 1][0]]);
const pick = (list) => list.filter(([id]) => keep.has(id)).map(([id, v]) => `${id}:${JSON.stringify(v)}`).join(",");
const nameList = names.filter(([id]) => hashes.has(id));
const runtime = `t.u=e=>(({${pick(nameList)}})[e]||e)+"."+({${pick([...hashes])}})[e]+"a.js"`;

const svgs = [0, 1, 2, 3].map((i) => {
  const m = raw.match(new RegExp(`<svg[^>]*id=["']loading-x-anim-${i}["'][^>]*>[\\s\\S]*?</svg>`));
  if (!m) throw new Error(`svg ${i} not found`);
  return m[0];
});

fs.writeFileSync(
  outPath,
  `<!doctype html>\n<html><head>\n${meta[0]}\n<script>${runtime}</script>\n</head><body>\n${svgs.join("\n")}\n</body></html>\n`,
);
