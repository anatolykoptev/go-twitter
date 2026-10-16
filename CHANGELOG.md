# Changelog

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
