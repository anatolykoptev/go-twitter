# xtid browser vectors

The animation key is not computed by X's server: X's own script runs a CSS
animation in the user's browser and prints what the browser reports. So the
oracle for `xtid`'s animation key is a real browser, and `vectors.json` holds its
answers. `TestBrowserGolden` holds `newClientTransaction` to them exactly, with
no allowance, for 1000 verification keys.

(The Lqm1 reference in `../reference` is kept for what it is good for: the id's
hash format, the `ondemand.s` URL and page parsing. It is wrong for the animation
key in two ways found here: it never caps a colour channel at 255, and it solves
the easing curve differently from Chromium.)

## What runs

`run.mjs` loads `../reference/live_page.html` (a public, cookie-less page, reduced
to the verification meta tag and the four loading animation SVGs) into headless
Chromium from a local HTTP server, with the verification key replaced by each key
in `keys.json`. It then runs `../reference/live_ondemand.js`, the script X ships,
unmodified, and calls its transaction-id function. The script's `crypto.subtle.digest`
call is intercepted to read the string it hashes,
`<path>!<method>!<time>obfiowerehiring<animation key>`; the animation key is the
text after the keyword.

The browser is started with `--host-resolver-rules="MAP * ~NOTFOUND, EXCLUDE 127.0.0.1"`:
no host other than `127.0.0.1` resolves, so nothing leaves the machine and no
x.com traffic is made.

## Provenance

`vectors.json` starts with a `provenance` object: the browser version string, the
`playwright-core` version, the generation time, the platform, the key seed, the
SHA-256 of `live_page.html`, `live_ondemand.js`, `run.mjs` and `keys.json`, and the
class counts of the keys. `TestBrowserGolden` fails if any of those files has
changed since, so the vectors cannot silently drift from their inputs.

Recorded with Chromium 148.0.7778.96 (headless shell, `playwright-core` 1.60.0) on
darwin-arm64. Chromium 156.0.8078.4 gave identical answers for all 1000 keys, and
for 4000 further random keys that are not committed.

## The keys

`keys.json` is 1000 seeded verification keys (48 bytes, base64) with the hard
cases included by construction:

| class | meaning | count |
|---|---|---|
| overflow | a colour channel rounds above 255 before the cap | 269 |
| negative | a colour channel rounds below 0 before the floor | 184 |
| solver | the key changes if the easing is solved by plain bisection (Lqm1's method) to 1e-5, 1e-6 or 1e-7 | 100 |
| plain | none of the above | 499 |

Classes overlap, and the pinned fixture key of `clamp_test.go` is the first key.
The test recomputes the counts and fails if the code no longer classifies the
keys as recorded.

## Regenerate

Needs Node 20+ and a Chromium headless shell. Nothing here is needed to run the
Go tests.

```sh
cd xtid/testdata/browser
npm ci --ignore-scripts
npx playwright-core install chromium-headless-shell

# 1. keys (only if the key set or its classes should change)
BROWSER_KEYS_OUT=$PWD/keys.json go test ../.. -run TestBrowserKeysGen -v

# 2. the browser's answers
node run.mjs                                   # keys.json -> vectors.json
CHROMIUM_EXECUTABLE=/path/to/chrome node run.mjs   # a specific build
```

Then `go test ./xtid -run TestBrowserGolden`.

## What this does not establish

Only Chromium, and only on arm64 macOS, is recorded. Firefox and Safari run the
same script but are different engines (different easing solver, colour and matrix
serialisation); X's server accepts the Chromium value, which is what `xtid` sends.
The vectors cover one page and one script version: when X ships a new
`ondemand.s`, the page and script in `../reference` and these vectors need to be
re-recorded together.
