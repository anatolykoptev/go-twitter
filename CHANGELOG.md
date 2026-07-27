# Changelog

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
