# Documentation index

| Document | Description |
|---|---|
| [README](../README.md) | What the client is, install, quick start, what it covers, known limits |
| [CHANGELOG](../CHANGELOG.md) | Release history, written by release-please |
| [CONTRIBUTING](../CONTRIBUTING.md) | Build, the preflight gate, pull-request rules, generated files, test data rules |
| [SECURITY](../SECURITY.md) | How to report a vulnerability privately |
| [CODE_OF_CONDUCT](../CODE_OF_CONDUCT.md) | Contributor Covenant code of conduct |
| [LICENSE](../LICENSE) | Apache License 2.0 |
| [api](api.md) | Every client method, whether the pool or the caller picks the account, and the types |
| [configuration](configuration.md) | Accounts, every ClientConfig field with its default, environment variables |
| [errors](errors.md) | Recovery per error class, predicates, the relogin breaker |
| [maintenance](maintenance.md) | Keeping GraphQL operation ids current with gql-sync and the weekly drift check, tools, CI |
| [metrics](metrics.md) | OpenTelemetry instruments, their attributes and how to install a provider |
| [rate-limits](rate-limits.md) | Per-account, per-endpoint windows, seeds, spend shares, holds, pacing and the request observer |
| [request-format](request-format.md) | What the client sends: transaction id, headers, TLS profile, identities and wire generations |
| [sessions](sessions.md) | The SessionStore contract and a database-backed example |
| [xweb-migration](xweb-migration.md) | Generated inventory of operations on the new web client (cmd/xweb-report, do not edit) |
| [CLAUDE](../CLAUDE.md) | Notes for coding agents: current layout, the exact CI gate, invariants (generated ids, wire identity, ct0, offline tests) |
