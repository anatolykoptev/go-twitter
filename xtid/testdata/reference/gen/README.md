# xtid golden vectors

`../vectors.json` holds expected values produced by the **reference
implementation**, not by this repository, so `xtid` is tested bit-for-bit against
an oracle it did not write. CI needs no Node: only the regeneration does.

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

- `animate`: `animate(row, t)` for every frame x row, with random times, times
  where the 255 colour clamp or the 1e-6 epsilon changes the result, and `t >= 1`.
- `keys`: random 48-byte verification keys through the whole animation-key path
  (frame, row, frameTime including rounding ties), plus keys chosen so the clamp,
  the epsilon and a `frameTime/10` tie each change the result if mishandled.
- `ids`: full transaction ids with a fixed time and random byte. `xtid` signs the
  path without its query string, so the reference is fed the query-free path.
- `round2`: exact ties of `Math.round(x*100)/100` (JS rounds them toward +Inf).
- `runtimes`: `ondemand.s` URL resolution from an inline webpack runtime for
  several chunk-hash shapes.

The generator also contains copies of the reference's cubic/animate code with the
epsilon and clamp as parameters. They are used only to *select* interesting
vectors, and a self-check asserts they equal the reference when parameterised
like it, so a stale copy fails generation instead of skewing the sample.
