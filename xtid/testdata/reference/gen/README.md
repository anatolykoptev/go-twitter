# xtid golden vectors

`../vectors.json` holds expected values produced by the **reference
implementation**, not by this repository, so `xtid` is tested bit-for-bit against
an oracle it did not write. CI needs no Node: only the regeneration does.

The reference is the oracle for what it is good for: the transaction id's hash
format (`ids`), the `ondemand.s` URL (`runtimes`), the rounding helper (`round2`)
and page parsing. It is **not** the oracle for the animation key: it does not
cap a colour channel at 255 (about 5% of keys print three hex digits where X
expects `ff`, and X answers 404) and it solves the easing curve by plain
bisection where Chromium uses Newton steps from a sampled first guess. The
animation key is held to a real browser instead, see `../../browser/README.md`.

| | |
|---|---|
| Reference | `x-client-transaction-id` (npm; source: Lqm1/x-client-transaction-id) |
| Pinned version | `0.3.2` |
| Integrity | `sha512-DpX8C4wF8S54cSE9X1MJkJbNElhSg5337iW0UMFDJVAVCj+cHePCvS6tj2EQw85UGVf1ywBSTGVckncBONUdDg==` |
| Node | 18 or newer |

Regenerate (the output is deterministic; a second run is byte-identical):

```sh
cd xtid/testdata/reference/gen
npm ci --ignore-scripts
node gen.mjs            # rewrites ../vectors.json
```

Then run `go test ./xtid -run TestReferenceGolden`.

## Inputs

Two data sets, each a page plus the script that carries the key-byte indices:

- `fixture`: `../home_real.html` and `../ondemand_real.js`.
- `live`: `../live_page.html` and `../live_ondemand.js`, taken from a cookie-less
  GET of `https://x.com/i/jf/` and of the `ondemand.s` script it points to on
  `abs.twimg.com`. The page is reduced by `reduce-live-page.mjs` to what `xtid`
  reads: the verification meta tag, the webpack chunk-name/hash maps trimmed to a
  few entries, and the four loading animation SVGs. Everything else (guest
  cookies, nonces, tokens, the rest of the app shell) is dropped.

## What the vectors cover

- `ids`: full transaction ids with a fixed time and random byte. `xtid` signs the
  path without its query string, so the reference is fed the query-free path.
- `round2`: exact ties of `Math.round(x*100)/100` (JS rounds them toward +Inf).
- `runtimes`: `ondemand.s` URL resolution from an inline webpack runtime for
  several chunk-hash shapes.
