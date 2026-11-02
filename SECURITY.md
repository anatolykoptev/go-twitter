# Security policy

## Supported versions

Security fixes go into the latest release. The project is pre-1.0, so please upgrade to get them.

## Reporting a vulnerability

Report privately through [GitHub security advisories](https://github.com/anatolykoptev/go-twitter/security/advisories/new). Do not open a public issue.

Please include the affected version, the steps to reproduce and the impact. Do not include real cookies, tokens or passwords.

## Scope

In scope:

- Cookies, tokens, passwords, TOTP secrets or proxy credentials reaching logs, errors, metrics, traces or a host other than the intended one.
- Session data stored with weaker protection than documented.
- Vulnerabilities in dependencies that affect this library.

Out of scope:

- X's own services and accounts.
- Account restrictions applied by X.

## Handling credentials

`auth_token` and `ct0` give full access to an account. Load them from a secret store or the environment, never commit them, and leave `TWITTER_DUMP_DIR` unset outside debugging because it writes response bodies to disk.
