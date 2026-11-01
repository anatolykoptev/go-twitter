// Golden-vector generator for xtid: every expected value comes from the
// reference implementation (x-client-transaction-id), never from this repo.
//
//   npm ci --ignore-scripts && node gen.mjs            # writes ../vectors.json
//
// Inputs (committed): ../home_real.html + ../ondemand_real.js (fixture
// set) and live_page.html + live_ondemand.js (live logged-out set).
import { parseHTML } from "linkedom";
import ClientTransaction, { convertRotationToMatrix, floatToHex, interpolate, isOdd } from "x-client-transaction-id";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(process.argv[2] ?? path.join(here, ".."));
const lock = JSON.parse(fs.readFileSync(path.join(here, "package-lock.json"), "utf8"));
const pkg = lock.packages["node_modules/x-client-transaction-id"];

function rng(seed) {
  return () => {
    seed |= 0; seed = (seed + 0x6d2b79f5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

// --- variants of the reference, used ONLY to pick interesting vectors -------
// (expected values always come from the unmodified reference; a self-check
// below proves variant(eps=1e-5, no clamp) == reference on every vector)
function cubicValue(curves, time, eps) {
  const calc = (a, b, m) => 3.0 * a * (1 - m) * (1 - m) * m + 3.0 * b * (1 - m) * m * m + m * m * m;
  let sg = 0, eg = 0, start = 0.0, mid = 0.0, end = 1.0;
  if (time <= 0.0) {
    if (curves[0] > 0.0) sg = curves[1] / curves[0];
    else if (curves[1] === 0.0 && curves[2] > 0.0) sg = curves[3] / curves[2];
    return sg * time;
  }
  if (time >= 1.0) {
    if (curves[2] < 1.0) eg = (curves[3] - 1.0) / (curves[2] - 1.0);
    else if (curves[2] === 1.0 && curves[0] < 1.0) eg = (curves[1] - 1.0) / (curves[0] - 1.0);
    return 1.0 + eg * (time - 1.0);
  }
  while (start < end) {
    mid = (start + end) / 2;
    const x = calc(curves[0], curves[2], mid);
    if (Math.abs(time - x) < eps) return calc(curves[1], curves[3], mid);
    if (x < time) start = mid; else end = mid;
  }
  return calc(curves[1], curves[3], mid);
}
function variantAnimate(ct, frames, targetTime, { eps, clamp }) {
  const fromColor = frames.slice(0, 3).concat(1).map(Number);
  const toColor = frames.slice(3, 6).concat(1).map(Number);
  const toRotation = [ct.solve(frames[6], 60.0, 360.0, true)];
  const curves = frames.slice(7).map((item, i) => ct.solve(item, isOdd(i), 1.0, false));
  const val = cubicValue(curves, targetTime, eps);
  const color = interpolate(fromColor, toColor, val).map((v) => (v > 0 ? v : 0)).map((v) => (clamp ? Math.min(v, 255) : v));
  const matrix = convertRotationToMatrix(interpolate([0.0], toRotation, val)[0]);
  const strArr = color.slice(0, -1).map((v) => Math.round(v).toString(16));
  for (const v of matrix) {
    let r = Math.round(v * 100) / 100;
    if (r < 0) r = -r;
    const h = floatToHex(r);
    strArr.push(h.startsWith(".") ? `0${h}`.toLowerCase() : h || "0");
  }
  strArr.push("0", "0");
  return strArr.join("").replace(/[.-]/g, "");
}

const SETS = [
  { name: "fixture", page: "../home_real.html", ondemand: "../ondemand_real.js", seed: 101 },
  { name: "live", page: "live_page.html", ondemand: "live_ondemand.js", seed: 202 },
];
const TUPLES = [
  ["GET", "/i/api/graphql/abc123/UserByScreenName"],
  ["POST", "/i/api/1.1/jot/client_event.json"],
  ["GET", "/i/api/graphql/xyz/UserTweetsAndReplies?variables=%7B%22userId%22%3A%2212%22%7D&features=%7B%7D"],
  ["get", "/i/api/graphql/abc123/UserByScreenName"],
  ["POST", "/i/api/graphql/q/CreateTweet"],
  ["GET", "/i/api/graphql/ünï/Ünï%20x"],
];
const RUNTIME_HASHES = ["abcdef1", "6f4d46a32a10fdcc", "0123456789abcdef0", "0123456789abcdef0123456789abcdef", "0123456789ABCDEF", "xyz_-9", ""];

const origFetch = globalThis.fetch;
const out = {
  reference: { package: "x-client-transaction-id", version: pkg.version, integrity: pkg.integrity },
  note: "Expected values are produced by the reference implementation; see gen/README.md.",
  runtimes: [],
  sets: [],
};

async function buildSet(spec) {
  const html = fs.readFileSync(path.join(root, spec.page), "utf8");
  const ondemand = fs.readFileSync(path.join(root, spec.ondemand), "utf8");
  const doc = parseHTML(html).window.document;
  globalThis.fetch = async () => ({ ok: true, status: 200, statusText: "OK", text: async () => ondemand });
  const proto = ClientTransaction.prototype;
  const realResolve = proto.getOnDemandFileUrl;
  let ondemandUrl = null;
  try { ondemandUrl = realResolve.call(new ClientTransaction(doc), doc); } catch { /* page has no runtime map */ }
  proto.getOnDemandFileUrl = () => "fixture://ondemand";
  const ct = await ClientTransaction.create(doc);
  proto.getOnDemandFileUrl = realResolve;
  globalThis.fetch = origFetch;

  const r = rng(spec.seed);
  const arrs = [0, 1, 2, 3].map((f) => { const kb = ct.keyBytes.slice(); kb[5] = f; return ct.get2dArray(kb, doc); });
  const stats = { clampCases: 0, epsilonCases: 0 };
  const same = (a, b) => a === b;

  // --- sweep A: animate(frame row, t) -----------------------------------------
  const animate = [];
  const seen = new Set();
  const add = (f, row, t) => {
    const k = `${f}/${row}/${t}`;
    if (seen.has(k)) return;
    seen.add(k);
    const key = ct.animate(arrs[f][row], t);
    const ref5 = variantAnimate(ct, arrs[f][row], t, { eps: 1e-5, clamp: false });
    if (!same(ref5, key)) throw new Error(`variant self-check failed at ${k}`);
    if (!same(variantAnimate(ct, arrs[f][row], t, { eps: 1e-5, clamp: true }), key)) stats.clampCases++;
    if (!same(variantAnimate(ct, arrs[f][row], t, { eps: 1e-6, clamp: false }), key)) stats.epsilonCases++;
    animate.push([f, row, t, key]);
  };
  for (let f = 0; f < 4; f++) for (let row = 0; row < arrs[f].length; row++) {
    const clampT = [], epsT = [];
    for (let ft = 0; ft <= 4096; ft++) {
      const t = ft / 4096;
      const key = ct.animate(arrs[f][row], t);
      if (variantAnimate(ct, arrs[f][row], t, { eps: 1e-5, clamp: true }) !== key) clampT.push(t);
      if (variantAnimate(ct, arrs[f][row], t, { eps: 1e-6, clamp: false }) !== key) epsT.push(t);
    }
    add(f, row, 0); add(f, row, 1);
    for (let i = 0; i < 4; i++) add(f, row, r());
    for (let i = 0; i < 2 && clampT.length; i++) add(f, row, clampT[Math.floor(r() * clampT.length)]);
    for (let i = 0; i < 2 && epsT.length; i++) add(f, row, epsT[Math.floor(r() * epsT.length)]);
    if ([0, 5, 6, 11].includes(row)) for (const t of [1.0000001, 1.26953125, 2.109375, 3.4375, 3.75, 12.265625, 12.5, -0.5]) add(f, row, t);
  }

  // --- sweep B: end-to-end animation key from key bytes -----------------------
  const L = ct.keyBytes.length;
  const describe = (kb) => {
    const row = kb[ct.DEFAULT_ROW_INDEX] % 16;
    let ft = ct.DEFAULT_KEY_BYTES_INDICES.reduce((a, b) => a * (kb[b] % 16), 1);
    const prod = ft;
    ft = Math.round(ft / 10) * 10;
    return { f: kb[5] % 4, row, t: ft / 4096, tie: prod % 10 === 5 };
  };
  const mk = () => Array.from({ length: L }, () => Math.floor(r() * 256));
  const keys = [];
  const addKey = (kb) => {
    const key = ct.getAnimationKey(kb, doc);
    const d = describe(kb);
    if (!same(variantAnimate(ct, arrs[d.f][d.row], d.t, { eps: 1e-5, clamp: false }), key)) throw new Error("key variant self-check failed");
    keys.push([Buffer.from(kb).toString("base64"), key]);
    return d;
  };
  addKey(ct.keyBytes.slice());
  for (let i = 0; i < 599; i++) addKey(mk());
  const want = { clamp: 100, eps: 100, tie: 50 };
  const got = { clamp: 0, eps: 0, tie: 0 };
  for (let i = 0; i < 40000 && (got.clamp < want.clamp || got.eps < want.eps || got.tie < want.tie); i++) {
    const kb = mk();
    const d = describe(kb);
    const key = ct.getAnimationKey(kb, doc);
    const isClamp = variantAnimate(ct, arrs[d.f][d.row], d.t, { eps: 1e-5, clamp: true }) !== key;
    const isEps = variantAnimate(ct, arrs[d.f][d.row], d.t, { eps: 1e-6, clamp: false }) !== key;
    let take = false;
    if (isClamp && got.clamp < want.clamp) { got.clamp++; take = true; }
    if (isEps && got.eps < want.eps) { got.eps++; take = true; }
    if (d.tie && got.tie < want.tie) { got.tie++; take = true; }
    if (take) addKey(kb);
  }
  stats.keyClampCases = got.clamp; stats.keyEpsilonCases = got.eps; stats.keyTieCases = got.tie;

  // --- sweep C: full transaction ids ------------------------------------------
  const ids = [];
  const origRandom = Math.random;
  for (const [m, p] of TUPLES) for (const tn of [1, 123456789, 2147483647]) for (const rv of [0, 255]) {
    Math.random = () => (rv + 0.5) / 256;
    const q = p.indexOf("?");
    // xtid signs the query-free path; the reference signs what it is given.
    const id = await ct.generateTransactionId(m, q >= 0 ? p.slice(0, q) : p, doc, undefined, undefined, tn);
    ids.push([m, p, tn, rv, id]);
  }
  Math.random = origRandom;

  return {
    name: spec.name, page: spec.page, ondemand: spec.ondemand,
    ondemandUrl, realAnimationKey: ct.animationKey, rowIndex: ct.DEFAULT_ROW_INDEX, indices: ct.DEFAULT_KEY_BYTES_INDICES,
    stats, animate, keys, ids,
  };
}

for (const spec of SETS) out.sets.push(await buildSet(spec));

// Math.round(x*100)/100 on exact ties (JS rounds toward +Inf), negative and positive.
const ties = [];
for (let k = -6000; k <= 6000; k++) {
  const x = k / 2000, y = x * 100;
  if (Math.abs(y % 1) === 0.5) ties.push([x, Math.round(y) / 100]);
}
out.round2 = ties.filter(([x]) => x < 0).filter((_, i) => i % 3 === 0).concat(ties.filter(([x]) => x > 0).filter((_, i) => i % 12 === 0));

for (const h of RUNTIME_HASHES) {
  const html = `<script>n.u=e=>(({1:"main",20113:"ondemand.s",30:"x"})[e]||e)+"."+({1:"aa11",20113:"${h}",30:"bb22"})[e]+"a.js"</script>`;
  let url = null;
  try { url = new ClientTransaction(parseHTML(html).window.document).getOnDemandFileUrl(); } catch { /* no match */ }
  out.runtimes.push([html, url]);
}

// One array per line keeps the file diffable and compact.
const line = (v) => JSON.stringify(v);
const block = (arr) => "[\n" + arr.map((x) => "    " + line(x)).join(",\n") + "\n  ]";
let text = "{\n";
text += `  "reference": ${line(out.reference)},\n  "note": ${line(out.note)},\n`;
text += `  "round2": ${block(out.round2)},\n  "runtimes": ${block(out.runtimes)},\n  "sets": [\n`;
text += out.sets.map((s) => {
  const head = { ...s }; delete head.animate; delete head.keys; delete head.ids;
  return `  {\n  "meta": ${line(head)},\n  "animate": ${block(s.animate)},\n  "keys": ${block(s.keys)},\n  "ids": ${block(s.ids)}\n  }`;
}).join(",\n");
text += "\n  ]\n}\n";
fs.writeFileSync(path.join(root, "vectors.json"), text);
console.log(JSON.stringify(out.sets.map((s) => ({ name: s.name, animate: s.animate.length, keys: s.keys.length, ids: s.ids.length, stats: s.stats, ondemandUrl: s.ondemandUrl }))), "round2", out.round2.length, "bytes", text.length);
