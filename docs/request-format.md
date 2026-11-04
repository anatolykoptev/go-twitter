# Request format

go-twitter calls the endpoints used by the x.com web app, so its requests follow that app's format. This page lists what the client sends and which settings affect it.

## Transaction ID

`x-client-transaction-id` is generated from animation keys on an x.com page. The keys are refreshed every 30 minutes and are first loaded on the first request that carries an account's cookies. If loading fails, the header is omitted, retries back off from 60 s to 15 minutes, and one `xtid.init_failed` alert per failure episode goes to `PoolAlertHook`.

By default the keys come from a page fetched with the account's cookies (`https://x.com/tesla`), falling back to the logged-out landing page. `ClientConfig.XtidKeySource = xtid.KeySourceLoggedOut`, or `GO_TWITTER_XTID_KEY_SOURCE=logged-out`, reads them from the cookie-less `https://x.com/i/jf/` page instead, so no account cookies are sent for this step. `XtidKeySource` is ignored when the manager is shared by the `*WithSocial` helpers; the environment variable still applies there. `cmd/xtid-probe` shows which source X currently accepts.

## x-xp-forwarded-for

Generated from a `guest_id` and the user agent of the request. Each account has its own generator, so two accounts send different values. The `guest_id` is the one in the account's cookie jar once the server has set it, so the header agrees with the cookie; until then it is derived from the account's username, which keeps it stable across restarts. The user agent is the account's own (see below), so the value embeds the same browser identity as the rest of the request. Requests that carry no account, such as the guest-token fallback, use one client-wide generator.

## CSRF token

`ct0` is never rotated on a timer or replaced with a client-generated value on an established session, because x.com validates it server-side and doing so causes error 353. The client adopts the `ct0` from each successful response's `set-cookie`, saves it to the session store, and on error 353 adopts the value the server offers before falling back to relogin. A generated `ct0` is used only at login, when x.com sets none.

## Transport and headers

Requests go through an HTTP client with a Chrome profile. Every account gets its own HTTP client, built in `NewClient` or, for an account added later through `Pool().Add`, on its first request, and with it its own cookie jar, so a `set-cookie` on a response to one account is never sent on another account's request. An account with its own `Proxy` connects through it; the others connect through `DefaultProxy`, or directly when it is empty. The shared default client serves only requests that carry no account, such as the guest-token fallback. `*WithSocial` helpers build a new `Client` per call, so each call builds one more HTTP client.

Headers are sent in a fixed order, and client hints are derived from the account's user agent. `ParseAccounts` assigns each account a browser profile and user agent through `AssignBrowserProfile`, derived from its username; an account added later gets the same on its first request, and an account created by the open-account flow keeps the identity of the client that created its session. A profile or user agent set on the `Account` is never replaced.

## Request spacing

Each account's consecutive pool requests are spaced by the per-account pacer: 0.8 s to 2.0 s by default, configurable with `WithAccountPace` or `AccountPaceMin` and `AccountPaceRandom`, and removable with `AccountPaceDisabled`. Pacing is keyed by account, so a busy caller does not delay callers using other accounts.

There is no pre-request delay by default. To add one:

```go
client, err := twitter.NewClient(cfg, twitter.WithRequestJitter(500*time.Millisecond, 2500*time.Millisecond))
```

Writes and media uploads keep their own delay either way.

The `*WithSocial` helpers build a client per call, so they share one pacer per account within a process, for callers using the same pace settings. They take the same options as a trailing argument:

```go
user, err := twitter.GetUserWithSocial(ctx, sc, "handle", twitter.WithRequestJitter(500*time.Millisecond, 2500*time.Millisecond))
```
