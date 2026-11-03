// Golden-vector generator for xtid: every expected value comes from the
// reference implementation (x-client-transaction-id), never from this repo.
// The reference is the oracle for what it is good for: the transaction id's hash
// format, the indices script, the ondemand.s URL and the rounding helpers. It is
// NOT the oracle for the animation key (it does not cap a colour channel at 255
// and solves the easing curve differently from Chromium); those vectors come from
// a real browser, see ../../browser/README.md.
//
//   npm ci --ignore-scripts && node gen.mjs            # writes ../vectors.json
//
// Inputs (committed): ../home_real.html + ../ondemand_real.js (fixture
// set) and live_page.html + live_ondemand.js (live logged-out set).
import { parseHTML } from "linkedom";
import ClientTransaction from "x-client-transaction-id";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(process.argv[2] ?? path.join(here, ".."));
const lock = JSON.parse(fs.readFileSync(path.join(here, "package-lock.json"), "utf8"));
const pkg = lock.packages["node_modules/x-client-transaction-id"];

const SETS = [
  { name: "fixture", page: "../home_real.html", ondemand: "../ondemand_real.js" },
  { name: "live", page: "live_page.html", ondemand: "live_ondemand.js" },
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

  // --- full transaction ids ----------------------------------------------------
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
    ondemandUrl, rowIndex: ct.DEFAULT_ROW_INDEX, indices: ct.DEFAULT_KEY_BYTES_INDICES,
    ids,
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
  const head = { ...s }; delete head.ids;
  return `  {\n  "meta": ${line(head)},\n  "ids": ${block(s.ids)}\n  }`;
}).join(",\n");
text += "\n  ]\n}\n";
fs.writeFileSync(path.join(root, "vectors.json"), text);
console.log(JSON.stringify(out.sets.map((s) => ({ name: s.name, ids: s.ids.length, ondemandUrl: s.ondemandUrl }))), "round2", out.round2.length, "bytes", text.length);
