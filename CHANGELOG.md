# Changelog

## [0.8.16](https://github.com/anatolykoptev/go-twitter/compare/v0.8.15...v0.8.16) (2026-11-14)


### Refactoring

* merge five small root files into their natural homes ([#215](https://github.com/anatolykoptev/go-twitter/issues/215)) ([c3bbeec](https://github.com/anatolykoptev/go-twitter/commit/c3bbeecd52be4501d0bade7747e47fea1679f9f9))
* remove three unused unexported leftovers ([#214](https://github.com/anatolykoptev/go-twitter/issues/214)) ([85d8978](https://github.com/anatolykoptev/go-twitter/commit/85d8978af31b96d2304c8cc8eebe20d934d3438e))


### CI

* docs gate treats code-referenced docs as code ([#220](https://github.com/anatolykoptev/go-twitter/issues/220)) ([a4f7f8a](https://github.com/anatolykoptev/go-twitter/commit/a4f7f8a0c2abf42433c1cf290b16dd9118f0c49b))
* docs-only PRs skip the heavy gate ([#209](https://github.com/anatolykoptev/go-twitter/issues/209)) ([1e07459](https://github.com/anatolykoptev/go-twitter/commit/1e07459920aad9d5836a390929ed7a7cc14a4c51))
* re-sync the docs-only gate with the canonical block ([#218](https://github.com/anatolykoptev/go-twitter/issues/218)) ([8371f7b](https://github.com/anatolykoptev/go-twitter/commit/8371f7bdb99aa033f071b0279ddfcb8ce844cb85))
* remove the scheduled GraphQL drift check ([#221](https://github.com/anatolykoptev/go-twitter/issues/221)) ([2063992](https://github.com/anatolykoptev/go-twitter/commit/2063992b0f70a1d0383d6cce3499284c18c0c3b0))


### Documentation

* bring repository documents up to the repo-standards set ([#216](https://github.com/anatolykoptev/go-twitter/issues/216)) ([c9c7a39](https://github.com/anatolykoptev/go-twitter/commit/c9c7a3985c897e6f6ed93993d6c8806cdcf2bb72))

## [0.8.15](https://github.com/anatolykoptev/go-twitter/compare/v0.8.14...v0.8.15) (2026-11-07)


### Features

* CaptureRequest records the headers an identity sends ([#205](https://github.com/anatolykoptev/go-twitter/issues/205)) ([2884a79](https://github.com/anatolykoptev/go-twitter/commit/2884a795327f6316c712f78b054d19a1d1ca8558))

## [0.8.14](https://github.com/anatolykoptev/go-twitter/compare/v0.8.13...v0.8.14) (2026-11-06)


### Features

* **queryid:** report the warm-page layout and alert on webpack -&gt; vite ([#203](https://github.com/anatolykoptev/go-twitter/issues/203)) ([2df0b09](https://github.com/anatolykoptev/go-twitter/commit/2df0b0988b385204d7e7e49474c3ed97ba354f84))

## [0.8.13](https://github.com/anatolykoptev/go-twitter/compare/v0.8.12...v0.8.13) (2026-11-04)


### Bug Fixes

* **errors:** make 226 graduation monotonic, concurrency-safe and multi-code aware ([#200](https://github.com/anatolykoptev/go-twitter/issues/200)) ([fc8ebf9](https://github.com/anatolykoptev/go-twitter/commit/fc8ebf981e54f382a6ad8403b28b4427330a8e93))

## [0.8.12](https://github.com/anatolykoptev/go-twitter/compare/v0.8.11...v0.8.12) (2026-11-04)


### Features

* **queryid:** name the changed operations in the refresh applied log ([#198](https://github.com/anatolykoptev/go-twitter/issues/198)) ([c6d557a](https://github.com/anatolykoptev/go-twitter/commit/c6d557a3c748ef4882a70c66afbb7813921eb7db))

## [0.8.11](https://github.com/anatolykoptev/go-twitter/compare/v0.8.10...v0.8.11) (2026-11-04)


### Features

* **errors:** classify 226 as automated, 37 as not-authorized, add CheckSession ([#192](https://github.com/anatolykoptev/go-twitter/issues/192)) ([6d2cacd](https://github.com/anatolykoptev/go-twitter/commit/6d2cacdde0748f73dafc14ccc0a00adebb0d3cde))
* **xtid:** browser failure value on the current wire; fix: refresh generator once on 404 ([#191](https://github.com/anatolykoptev/go-twitter/issues/191)) ([8af64c6](https://github.com/anatolykoptev/go-twitter/commit/8af64c6ab05bfccaf54cb3f6dfce6f7103d3efb9))

## [0.8.10](https://github.com/anatolykoptev/go-twitter/compare/v0.8.9...v0.8.10) (2026-11-04)


### Bug Fixes

* current identities share one cookie jar per account and send each cookie once ([#188](https://github.com/anatolykoptev/go-twitter/issues/188)) ([f6afbf7](https://github.com/anatolykoptev/go-twitter/commit/f6afbf74bce2d01443b15db9426a3ae28aeac01d))

## [0.8.9](https://github.com/anatolykoptev/go-twitter/compare/v0.8.8...v0.8.9) (2026-11-04)


### Features

* wire coherence for current identities (account wire, warm navigations, api.x.com, referers, WireFor) ([#183](https://github.com/anatolykoptev/go-twitter/issues/183)) ([fb40947](https://github.com/anatolykoptev/go-twitter/commit/fb4094726df7841cfbbcc9642a5e9007361973b1))


### Bug Fixes

* current identities send each session cookie once ([#185](https://github.com/anatolykoptev/go-twitter/issues/185)) ([0128011](https://github.com/anatolykoptev/go-twitter/commit/01280111940aef37ed7ce788addbe8628dc66a01))
* stop cursor-paginated reads on empty page, repeated or terminal cursor, or page cap ([#186](https://github.com/anatolykoptev/go-twitter/issues/186)) ([59ee75d](https://github.com/anatolykoptev/go-twitter/commit/59ee75d98bd3cbb0fd2e4863225ab13357590a8e))

## [0.8.8](https://github.com/anatolykoptev/go-twitter/compare/v0.8.7...v0.8.8) (2026-11-04)


### Bug Fixes

* signed-in capture settles the web-client shape for current identities ([#181](https://github.com/anatolykoptev/go-twitter/issues/181)) ([402c390](https://github.com/anatolykoptev/go-twitter/commit/402c390c248e91db0942dc41bb96dfe71fbf7a30))

## [0.8.7](https://github.com/anatolykoptev/go-twitter/compare/v0.8.6...v0.8.7) (2026-11-04)


### Features

* send requests the way the X web client in Chrome does, per wire generation ([#177](https://github.com/anatolykoptev/go-twitter/issues/177)) ([04ee347](https://github.com/anatolykoptev/go-twitter/commit/04ee347a15e089ba46efcc8d959668d98f66d174))

## [0.8.6](https://github.com/anatolykoptev/go-twitter/compare/v0.8.5...v0.8.6) (2026-11-04)


### Features

* **social:** apply the browser identity a lease states ([#173](https://github.com/anatolykoptev/go-twitter/issues/173)) ([a02f558](https://github.com/anatolykoptev/go-twitter/commit/a02f558b34518bc14ff91af83beb1d9c4a80a60d))

## [0.8.5](https://github.com/anatolykoptev/go-twitter/compare/v0.8.4...v0.8.5) (2026-11-04)


### Bug Fixes

* bump go-stealth to v1.26.0; do not wait out a proxy backoff past the caller's deadline ([#169](https://github.com/anatolykoptev/go-twitter/issues/169)) ([934febf](https://github.com/anatolykoptev/go-twitter/commit/934febf42c2e5b7167f25d4ecc5aa39e7a3a3c80))
* **deps:** bump go-stealth to v1.26.1 ([#171](https://github.com/anatolykoptev/go-twitter/issues/171)) ([214dc42](https://github.com/anatolykoptev/go-twitter/commit/214dc42d49416e742ae22e91e8ffef0cdfc84143))
* one stable, consistent browser identity per account ([#167](https://github.com/anatolykoptev/go-twitter/issues/167)) ([19e4bc7](https://github.com/anatolykoptev/go-twitter/commit/19e4bc77c90955013511f16a84b74d2f6013a0a1))
* relogin follow-ups — bench on every path, single-flight, status-aware retry verdict ([#163](https://github.com/anatolykoptev/go-twitter/issues/163)) ([7e819c9](https://github.com/anatolykoptev/go-twitter/commit/7e819c94330a02874704ec7cc097d8b502e9020f))
* review follow-ups — relogin completion, stale ct0, terminal 214, feed drop count, open-account identity, flush race ([#170](https://github.com/anatolykoptev/go-twitter/issues/170)) ([95cfaf8](https://github.com/anatolykoptev/go-twitter/commit/95cfaf8ce25d4b676fb2628e1731d8e638cf8c02))
* tweet parsing never goes silently empty ([#165](https://github.com/anatolykoptev/go-twitter/issues/165)) ([35631b1](https://github.com/anatolykoptev/go-twitter/commit/35631b1eb362d578884877ead500ff2501ac4dd0))

## [0.8.4](https://github.com/anatolykoptev/go-twitter/compare/v0.8.3...v0.8.4) (2026-11-04)


### Performance

* bound the xtid cold start by the request ctx; send usage reports in the background ([#160](https://github.com/anatolykoptev/go-twitter/issues/160)) ([be70695](https://github.com/anatolykoptev/go-twitter/commit/be70695e50265bf7160df545503e3d9ba982e996))
* skip retry backoff for account-scoped failures; prefer pace-ready accounts ([#159](https://github.com/anatolykoptev/go-twitter/issues/159)) ([5bbc9a6](https://github.com/anatolykoptev/go-twitter/commit/5bbc9a69645adac05c0bac771d773eac19951cc4))

## [0.8.3](https://github.com/anatolykoptev/go-twitter/compare/v0.8.2...v0.8.3) (2026-11-04)


### Bug Fixes

* keep credentials until a relogin succeeds; make proxy backoff race-free ([#155](https://github.com/anatolykoptev/go-twitter/issues/155)) ([eb7a574](https://github.com/anatolykoptev/go-twitter/commit/eb7a574ea90123f55f56644c5c1ebce0be841323))
* **parse:** keep pinned, module and replaced tweets; mark replies, retweets, quotes and edits ([#152](https://github.com/anatolykoptev/go-twitter/issues/152)) ([a4ca52d](https://github.com/anatolykoptev/go-twitter/commit/a4ca52d7fa443c7037623526b8e3436db77424da))
* surface errors-only 200s, keep the post-relogin error class, encode query values fully ([#157](https://github.com/anatolykoptev/go-twitter/issues/157)) ([4300907](https://github.com/anatolykoptev/go-twitter/commit/43009077c213fd910dd9964a23d2c9c586a94db7))

## [0.8.2](https://github.com/anatolykoptev/go-twitter/compare/v0.8.1...v0.8.2) (2026-11-03)


### Features

* add package model and move response parsing to internal/parse ([#149](https://github.com/anatolykoptev/go-twitter/issues/149)) ([0d5ca72](https://github.com/anatolykoptev/go-twitter/commit/0d5ca72cdba42f0a0014c0336128e8dee70daa6e))


### Bug Fixes

* **xtid:** match the browser animation key ([#146](https://github.com/anatolykoptev/go-twitter/issues/146)) ([f1ed952](https://github.com/anatolykoptev/go-twitter/commit/f1ed952e4f734137048cdc352e48c087c9bf0e52))


### Refactoring

* move endpoint data to internal/operations ([#143](https://github.com/anatolykoptev/go-twitter/issues/143)) ([cd49fd0](https://github.com/anatolykoptev/go-twitter/commit/cd49fd06c0c80e8a5a4a6ced7d8d2488895423ef))
* move metric instruments to internal/telemetry ([#145](https://github.com/anatolykoptev/go-twitter/issues/145)) ([e7b7dad](https://github.com/anatolykoptev/go-twitter/commit/e7b7dad6c1351ba33cd4981ceacac2de86733a4e))
* move the query-ID refresher to internal/queryid ([#147](https://github.com/anatolykoptev/go-twitter/issues/147)) ([889c9a1](https://github.com/anatolykoptev/go-twitter/commit/889c9a18d27e0198f50b53a0094d527480f6dc52))

## [0.8.1](https://github.com/anatolykoptev/go-twitter/compare/v0.8.0...v0.8.1) (2026-11-02)


### Features

* **deps:** move to Go 1.27 and release connections on Client.Close ([#141](https://github.com/anatolykoptev/go-twitter/issues/141)) ([1e5c968](https://github.com/anatolykoptev/go-twitter/commit/1e5c968a7fff5d2f464a024535a902ab15b09e0d))

## [0.8.0](https://github.com/anatolykoptev/go-twitter/compare/v0.7.2...v0.8.0) (2026-11-02)


### ⚠ BREAKING CHANGES

* remove dead code and name test files by topic ([#136](https://github.com/anatolykoptev/go-twitter/issues/136))

### Features

* **pool:** per-account random pick pace ([#138](https://github.com/anatolykoptev/go-twitter/issues/138)) ([3ad73af](https://github.com/anatolykoptev/go-twitter/commit/3ad73afa9953c5c550e21e4c627b3de10a4f74c7))


### Bug Fixes

* **client:** give every account its own HTTP client ([#137](https://github.com/anatolykoptev/go-twitter/issues/137)) ([38873ba](https://github.com/anatolykoptev/go-twitter/commit/38873ba91ab180e8fd24eebb61d8b742432e292d))
* **deps:** update to the latest stable releases ([#135](https://github.com/anatolykoptev/go-twitter/issues/135)) ([61b6ed4](https://github.com/anatolykoptev/go-twitter/commit/61b6ed409a2f1aecf4871f82a6fbab2fe846b234))


### Refactoring

* remove dead code and name test files by topic ([#136](https://github.com/anatolykoptev/go-twitter/issues/136)) ([4e2049d](https://github.com/anatolykoptev/go-twitter/commit/4e2049d8145a653ad69c4774729f9237cea88cdc))


### Documentation

* rewrite the README and move the reference into docs ([#133](https://github.com/anatolykoptev/go-twitter/issues/133)) ([a633f2c](https://github.com/anatolykoptev/go-twitter/commit/a633f2c00ef1cf8c71cd8be13a5ae17443295af8))

## [0.7.2](https://github.com/anatolykoptev/go-twitter/compare/v0.7.1...v0.7.2) (2026-11-01)


### Features

* exported X limit table, per-account budget share, account hold, request observer ([#125](https://github.com/anatolykoptev/go-twitter/issues/125)) ([811bb2b](https://github.com/anatolykoptev/go-twitter/commit/811bb2b9dab246d32c906fea5c6c9792f9854bdc))


### Bug Fixes

* **xtid:** bit-exact parity with the reference generator (clamp, epsilon, rounding, FMA) ([#131](https://github.com/anatolykoptev/go-twitter/issues/131)) ([cbe90b9](https://github.com/anatolykoptev/go-twitter/commit/cbe90b9ce1c8b6876b58f333ba6d6090f1ed22e0))

## [0.7.1](https://github.com/anatolykoptev/go-twitter/compare/v0.7.0...v0.7.1) (2026-10-31)


### Features

* **xweb:** x-web Relay extractor, migration report, opt-in refresher source ([#119](https://github.com/anatolykoptev/go-twitter/issues/119) phase 1) ([#124](https://github.com/anatolykoptev/go-twitter/issues/124)) ([4fe9e36](https://github.com/anatolykoptev/go-twitter/commit/4fe9e36000134104a222b6190c6a93f47281be47))


### Bug Fixes

* **xtid:** match the reference on hex precision, SVG lookup and refresh; surface auth rejection ([#128](https://github.com/anatolykoptev/go-twitter/issues/128)) ([8b093d2](https://github.com/anatolykoptev/go-twitter/commit/8b093d22e5192a433e311468530e2eb32d7585bc))

## [0.7.0](https://github.com/anatolykoptev/go-twitter/compare/v0.6.27...v0.7.0) (2026-10-30)


### ⚠ BREAKING CHANGES

* pooled requests are sent without pre-request jitter by default; opt back in with WithRequestJitter(min, max).

### Features

* realtime primitives, no pre-request jitter by default, fix GraphQL bell fallback ([#120](https://github.com/anatolykoptev/go-twitter/issues/120)) ([8d1b9f2](https://github.com/anatolykoptev/go-twitter/commit/8d1b9f2d52d894d3a670c12429a2bf1ae30a1b67))


### Miscellaneous

* release 0.7.0 ([#123](https://github.com/anatolykoptev/go-twitter/issues/123)) ([f0130bc](https://github.com/anatolykoptev/go-twitter/commit/f0130bc5d17e114160b9cc1e22b31a76eb73e8ad))

## [0.6.27](https://github.com/anatolykoptev/go-twitter/compare/v0.6.26...v0.6.27) (2026-10-17)


### Bug Fixes

* **parsers:** read follower/tweet counts from post-legacy locations ([#117](https://github.com/anatolykoptev/go-twitter/issues/117)) ([d5ce58f](https://github.com/anatolykoptev/go-twitter/commit/d5ce58fd3eb29e0ac2bbdf3d5e16472f54822ec8))

## [0.6.26](https://github.com/anatolykoptev/go-twitter/compare/v0.6.25...v0.6.26) (2026-10-17)


### Bug Fixes

* **social:** cap each lease attempt so stalls rotate ([#114](https://github.com/anatolykoptev/go-twitter/issues/114)) ([c4af9d7](https://github.com/anatolykoptev/go-twitter/commit/c4af9d77b4aafda6be8a39dd09950f2a0074bde2))

## [0.6.25](https://github.com/anatolykoptev/go-twitter/compare/v0.6.24...v0.6.25) (2026-10-17)


### Features

* **social:** WithSocial helpers for remaining read ops ([#112](https://github.com/anatolykoptev/go-twitter/issues/112)) ([dceb5ff](https://github.com/anatolykoptev/go-twitter/commit/dceb5ff45b6742300fd0c3f65f3f38d348f0ccf4))

## [0.6.24](https://github.com/anatolykoptev/go-twitter/compare/v0.6.23...v0.6.24) (2026-10-16)


### Bug Fixes

* **pool:** caller errors and cancels never burn accounts; cap in-flight per account ([#109](https://github.com/anatolykoptev/go-twitter/issues/109)) ([e70e7c2](https://github.com/anatolykoptev/go-twitter/commit/e70e7c2ffdf7cb986a6387f540b92eedfccd5b57)), closes [#71](https://github.com/anatolykoptev/go-twitter/issues/71) [#96](https://github.com/anatolykoptev/go-twitter/issues/96)

## [0.6.23](https://github.com/anatolykoptev/go-twitter/compare/v0.6.22...v0.6.23) (2026-10-16)


### Bug Fixes

* **queryid:** refresh reads the legacy webpack layout of the authenticated page ([#107](https://github.com/anatolykoptev/go-twitter/issues/107)) ([3f57d27](https://github.com/anatolykoptev/go-twitter/commit/3f57d276bb6a67812edd50ff17481cfe0901b5ec)), closes [#68](https://github.com/anatolykoptev/go-twitter/issues/68)

## [0.6.22](https://github.com/anatolykoptev/go-twitter/compare/v0.6.21...v0.6.22) (2026-10-16)


### Features

* **endpoints:** runtime queryId/feature refresh from X's own bundle ([#68](https://github.com/anatolykoptev/go-twitter/issues/68)) ([#104](https://github.com/anatolykoptev/go-twitter/issues/104)) ([1694834](https://github.com/anatolykoptev/go-twitter/commit/16948345ee32d9b0db9dbe9dcb251cfeca3bb109))

## [0.6.21](https://github.com/anatolykoptev/go-twitter/compare/v0.6.20...v0.6.21) (2026-10-16)


### Bug Fixes

* **client:** one bad per-account proxy disables only that account ([#101](https://github.com/anatolykoptev/go-twitter/issues/101)) ([f3c1862](https://github.com/anatolykoptev/go-twitter/commit/f3c18621ff672a53e015aa62d246ee772aa32486)), closes [#96](https://github.com/anatolykoptev/go-twitter/issues/96)

## [0.6.20](https://github.com/anatolykoptev/go-twitter/compare/v0.6.19...v0.6.20) (2026-10-15)


### Features

* **ratelimit:** X rate-limit headers are the authority; carry them to go-social ([#98](https://github.com/anatolykoptev/go-twitter/issues/98)) ([916b5b2](https://github.com/anatolykoptev/go-twitter/commit/916b5b21a601b37078d330b06d9bfb9b6d7c0e26))

## [0.6.19](https://github.com/anatolykoptev/go-twitter/compare/v0.6.18...v0.6.19) (2026-10-15)


### Bug Fixes

* **request:** stop charging endpoint-wide faults to accounts; rotate reads ([#95](https://github.com/anatolykoptev/go-twitter/issues/95)) ([03952ac](https://github.com/anatolykoptev/go-twitter/commit/03952ac6139bd58a6ecca48dfc77025882dd96dc))

## [0.6.18](https://github.com/anatolykoptev/go-twitter/compare/v0.6.17...v0.6.18) (2026-10-13)


### Bug Fixes

* **pool:** escalate header-less 88s, pool wait metric, wake jitter ([#93](https://github.com/anatolykoptev/go-twitter/issues/93)) ([cc3e7c9](https://github.com/anatolykoptev/go-twitter/commit/cc3e7c9824e5207774f38ad9ea68d54321c35430))

## [0.6.17](https://github.com/anatolykoptev/go-twitter/compare/v0.6.16...v0.6.17) (2026-10-13)


### Features

* **session:** pluggable SessionStore with file-backed default ([#91](https://github.com/anatolykoptev/go-twitter/issues/91)) ([ab70683](https://github.com/anatolykoptev/go-twitter/commit/ab706839d799ae348073cc423beaa33332eb2577))

## [0.6.16](https://github.com/anatolykoptev/go-twitter/compare/v0.6.15...v0.6.16) (2026-10-13)


### Bug Fixes

* **auth:** provided credentials override the on-disk session; atomic session writes ([#86](https://github.com/anatolykoptev/go-twitter/issues/86)) ([#88](https://github.com/anatolykoptev/go-twitter/issues/88)) ([c66d296](https://github.com/anatolykoptev/go-twitter/commit/c66d296f33c4696ec8ff106af7731c5b24342ca9))

## [0.6.15](https://github.com/anatolykoptev/go-twitter/compare/v0.6.14...v0.6.15) (2026-10-13)


### Bug Fixes

* **social:** share xtid state and close per-call clients ([#84](https://github.com/anatolykoptev/go-twitter/issues/84)) ([#85](https://github.com/anatolykoptev/go-twitter/issues/85)) ([21b9b32](https://github.com/anatolykoptev/go-twitter/commit/21b9b32ef88e79ab83dcbb5d3b426ed0fa8be3ad))

## [0.6.14](https://github.com/anatolykoptev/go-twitter/compare/v0.6.13...v0.6.14) (2026-10-13)


### Bug Fixes

* wait for endpoint-limited accounts ([#58](https://github.com/anatolykoptev/go-twitter/issues/58)), code 88 rate limit is not a ban ([#60](https://github.com/anatolykoptev/go-twitter/issues/60)) ([#79](https://github.com/anatolykoptev/go-twitter/issues/79)) ([0d65de6](https://github.com/anatolykoptev/go-twitter/commit/0d65de64beb73998727b2cb6e16d773936cc0e8e))
* **xtid:** single-flight negative cache + Vite-build init with authenticated warm page ([#80](https://github.com/anatolykoptev/go-twitter/issues/80)) ([6191154](https://github.com/anatolykoptev/go-twitter/commit/6191154c3f400ec489839a13efc5fe0eae42c6bb))

## [0.6.13](https://github.com/anatolykoptev/go-twitter/compare/v0.6.12...v0.6.13) (2026-10-13)


### Features

* **metrics:** OpenTelemetry instruments ([#77](https://github.com/anatolykoptev/go-twitter/issues/77)) ([1135e7f](https://github.com/anatolykoptev/go-twitter/commit/1135e7fe04446a05fc4df8abf67eb0408ca876d8))


### Documentation

* refresh README against current API surface ([#55](https://github.com/anatolykoptev/go-twitter/issues/55)) ([3a6b741](https://github.com/anatolykoptev/go-twitter/commit/3a6b74138dd565a611bad75b31f216803d1342b6))

## [0.6.12](https://github.com/anatolykoptev/go-twitter/compare/v0.6.11...v0.6.12) (2026-09-29)


### Bug Fixes

* **parsers:** release the TweetWithVisibilityResults unwrap ([#53](https://github.com/anatolykoptev/go-twitter/issues/53)) ([2a4bc4e](https://github.com/anatolykoptev/go-twitter/commit/2a4bc4e382b53df9b53e2c141d9a034fb60f941b))

## [0.6.11](https://github.com/anatolykoptev/go-twitter/compare/v0.6.10...v0.6.11) (2026-09-29)


### Features

* read-parity surface — SearchUsers, GetTweetConversation, product tabs ([#49](https://github.com/anatolykoptev/go-twitter/issues/49)) ([81fb6f8](https://github.com/anatolykoptev/go-twitter/commit/81fb6f88627350dd4f44ec0312686d78287ffda3))


### Bug Fixes

* classify proxy 407 as a proxy failure, not an account failure ([#44](https://github.com/anatolykoptev/go-twitter/issues/44)) ([43c4f5e](https://github.com/anatolykoptev/go-twitter/commit/43c4f5e060c6e56831557586c517708c3c7dbca8))
* gql-sync additive merge + features test subset check ([#39](https://github.com/anatolykoptev/go-twitter/issues/39)) ([#40](https://github.com/anatolykoptev/go-twitter/issues/40)) ([b115ac7](https://github.com/anatolykoptev/go-twitter/commit/b115ac78e5544b3aa7d5de2a5c950c13207d8988))

## [0.6.10](https://github.com/anatolykoptev/go-twitter/compare/v0.6.9...v0.6.10) (2026-07-27)


### Refactoring

* **pacing:** add jitter to xtid fetch retry via stealth.BackoffConfig ([#37](https://github.com/anatolykoptev/go-twitter/issues/37)) ([60a8e59](https://github.com/anatolykoptev/go-twitter/commit/60a8e59281a8261566e0819ae5c04855565b5dc0))

## [0.6.9](https://github.com/anatolykoptev/go-twitter/compare/v0.6.8...v0.6.9) (2026-07-22)


### Features

* **errors:** split IsBanned/IsRateLimited, type relogin-failed path, lock double-wrap contract; Closes [#28](https://github.com/anatolykoptev/go-twitter/issues/28) ([#33](https://github.com/anatolykoptev/go-twitter/issues/33)) ([7d77b14](https://github.com/anatolykoptev/go-twitter/commit/7d77b14d03364c8a43f8b444faa80bd9dc1476d6))
* **errors:** typed APIError + exported Is* predicates (errors.As); Closes [#22](https://github.com/anatolykoptev/go-twitter/issues/22) ([#27](https://github.com/anatolykoptev/go-twitter/issues/27)) ([09110f7](https://github.com/anatolykoptev/go-twitter/commit/09110f714a282befffac8b3f91f84e2a7aec4823))
* **gql-sync:** fail-closed queryID completeness gate + best-effort extract; Closes [#23](https://github.com/anatolykoptev/go-twitter/issues/23) ([#32](https://github.com/anatolykoptev/go-twitter/issues/32)) ([ab890a8](https://github.com/anatolykoptev/go-twitter/commit/ab890a89238bab4e58153f509e1d3a5f5a0f2799))


### CI

* preflight (ubuntu-24.04-arm) + release-please (App token); Closes [#25](https://github.com/anatolykoptev/go-twitter/issues/25) ([#29](https://github.com/anatolykoptev/go-twitter/issues/29)) ([f26e22d](https://github.com/anatolykoptev/go-twitter/commit/f26e22d9aa835eb323ff6308e9f3a061a9a4890a))
