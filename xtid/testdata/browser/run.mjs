// Records, for each verification key in keys.json, the animation key that X's own
// transaction-id script computes in a real browser.
//
//   npm ci --ignore-scripts
//   npx playwright-core install chromium-headless-shell
//   node run.mjs                 # reads keys.json, writes vectors.json
//   CHROMIUM_EXECUTABLE=/path/to/chrome node run.mjs   # a specific build
//
// What runs: ../reference/live_ondemand.js, the script X ships, unmodified, in
// headless Chromium on ../reference/live_page.html with the verification key
// replaced. The page is served from 127.0.0.1 and the browser is told to resolve
// no other host, so nothing leaves this machine. The script's SHA-256 call is
// intercepted to read the string it hashes ("<path>!<method>!<time>obfiowerehiring<animation key>");
// the animation key is the text after the keyword.
import { chromium } from "playwright-core";
import crypto from "node:crypto";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const ref = path.join(here, "..", "reference");
const keysFile = path.join(here, process.argv[2] ?? "keys.json");
const outFile = path.join(here, process.argv[3] ?? "vectors.json");
const KEYWORD = "obfiowerehiring";

const sha = (p) => crypto.createHash("sha256").update(fs.readFileSync(p)).digest("hex");
const keys = JSON.parse(fs.readFileSync(keysFile, "utf8"));
const pageSrc = fs.readFileSync(path.join(ref, "live_page.html"), "utf8");
const script = fs.readFileSync(path.join(ref, "live_ondemand.js"), "utf8");
const metaRe = /(name="twitter-site-verification" content=")[^"]+/;
if (!metaRe.test(pageSrc)) throw new Error("verification meta tag not found in live_page.html");

let current = "";
const server = http.createServer((_, res) => {
  res.setHeader("content-type", "text/html");
  res.end(current);
}).listen(0, "127.0.0.1");
await new Promise((r) => server.once("listening", r));
const base = `http://127.0.0.1:${server.address().port}/`;

const browser = await chromium.launch({
  executablePath: process.env.CHROMIUM_EXECUTABLE || undefined,
  args: ["--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1"],
});
const version = browser.version();
const page = await browser.newPage();
const vectors = [];
for (const key of keys.keys) {
  current = pageSrc.replace(metaRe, `$1${key}`);
  await page.goto(base);
  const hashed = await page.evaluate(async ({ script }) => {
    let captured = null;
    const digest = crypto.subtle.digest.bind(crypto.subtle);
    crypto.subtle.digest = (alg, data) => {
      captured = new TextDecoder().decode(data);
      return digest(alg, data);
    };
    let modules;
    window.webpackChunk_twitter_responsive_web = { push(chunk) { modules = chunk[1]; } };
    (0, eval)(script);
    let spec;
    const req = () => ({});
    req.dn = () => {}; req.d = (_t, s) => { spec = s; }; req.n = () => {}; req.e = () => {};
    modules[Object.keys(modules)[0]](null, {}, req);
    await spec[2]()("GET", "/i/api/x");
    return captured;
  }, { script });
  const at = hashed.indexOf(KEYWORD);
  if (at < 0) throw new Error(`no keyword in hashed string for key ${key.slice(0, 8)}`);
  vectors.push([key, hashed.slice(at + KEYWORD.length)]);
}
await browser.close();
server.close();

const lock = JSON.parse(fs.readFileSync(path.join(here, "package-lock.json"), "utf8"));
const out = {
  provenance: {
    browser: version.startsWith("Chrom") ? version : `Chromium ${version}`,
    playwright: lock.packages["node_modules/playwright-core"].version,
    generatedAt: new Date().toISOString(),
    platform: `${os.platform()}-${os.arch()}`,
    seed: keys.seed,
    pageSha256: sha(path.join(ref, "live_page.html")),
    scriptSha256: sha(path.join(ref, "live_ondemand.js")),
    runnerSha256: sha(path.join(here, "run.mjs")),
    keysSha256: sha(keysFile),
    classes: keys.classes,
  },
  keys: vectors,
};
fs.writeFileSync(outFile, "{\n \"provenance\": " + JSON.stringify(out.provenance) + ",\n \"keys\": [\n" + vectors.map((v) => "  " + JSON.stringify(v)).join(",\n") + "\n ]\n}\n");
console.log(`${vectors.length} vectors from ${out.provenance.browser}`);
