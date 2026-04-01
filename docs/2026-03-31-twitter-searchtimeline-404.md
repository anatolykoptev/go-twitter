# Twitter SearchTimeline 404 — Root Cause Analysis

**Date:** 2026-03-31
**Status:** RESOLVED — root cause is GET→POST migration
**Affected:** go-twitter, go-job, go-hully (any service using SearchTimeline)

## Timeline

- **~2026-03-18**: Twitter silently migrates SearchTimeline from GET to POST
- **2026-03-19**: gallery-dl issue #9275 — first public report of 404
- **2026-03-17**: twscrape issue #298 — "No account available for SearchTimeline"
- **2026-03-29**: twikit PR #412 — fix: switch to POST
- **2026-03-31**: Our investigation confirms POST works via curl

## Root Cause

Twitter changed the SearchTimeline GraphQL endpoint from **GET** (parameters in query string) to **POST** (parameters in JSON body). GET requests now return `404 0` (empty body).

**Proof:** curl POST via residential proxy returns 200 with tweet data. Same credentials via GET return 404.

```bash
# GET → 404
curl -s "https://x.com/i/api/graphql/.../SearchTimeline?variables=...&features=..." → HTTP 404

# POST → 200 with tweets
curl -s -X POST -d '{"variables":...,"features":...}' "https://x.com/i/api/graphql/.../SearchTimeline" → HTTP 200
```

## Hypotheses Investigated

### 1. Stale Query ID (doc_id) — PARTIALLY TRUE
- Our ID `nK1dw4oV3k4w5TdtcAdSww` was outdated
- Fresh IDs extracted from x.com main.js bundle: `GcXk9vN_d1jUfHNqLacXQA`
- twikit uses `flaR-PUMshxFWZWPNpq4zA`
- **But updating query ID alone did NOT fix the 404** — POST method is the key

### 2. Missing x-xp-forwarded-for (XPFF) header — NOT THE CAUSE
- Researched and implemented AES-256-GCM XPFF generator
- Base key: `0e6be1f1e21ffc33590b888fd4dc81b19713e570e805d4e5df80a493c9571a05`
- Extracts real guest_id from x.com set-cookie
- **Implemented but did NOT fix 404** — Twitter still returned 404 with XPFF on GET requests
- May be required in the future, good to have

### 3. Outdated feature flags — NOT THE CAUSE
- Updated all features from current x.com JS bundle (30+ flags)
- Tested with both minimal and full features
- **Did NOT fix 404** — same result with old and new features on GET

### 4. Wrong referer/origin (twitter.com vs x.com) — NOT THE CAUSE
- Updated all headers from `twitter.com` to `x.com`
- **Did NOT fix 404** — curl with x.com referer still 404 on GET

### 5. Account-level restrictions — NOT THE CAUSE
- Tested with 6 different accounts (old and new)
- All accounts show "healthy" in go-social validator (UserByScreenName works)
- Even tested from real Chrome browser with cookies — same 404
- **All accounts fail equally on GET, all succeed on POST**

### 6. IP/datacenter blocking — NOT THE CAUSE
- Tested via Webshare residential proxies (different ports/IPs)
- Same 404 on GET regardless of IP
- POST works through residential proxy
- Note: x.com landing page doesn't render in headless Chrome from datacenter IPs

### 7. Broken xtid (x-client-transaction-id) parser — PARTIALLY TRUE
- twscrape issue #298 and twikit issues #408-411 report broken ondemand.s parsing
- Twitter changed webpack chunk format ~March 18
- Our xtid parser still works (successfully extracts anim_key)
- **Not the cause of 404** — xtid generates fine, 404 persists

### 8. Missing `requiresAuth` for SearchTimeline — FIXED, NOT THE CAUSE
- go-twitter treated SearchTimeline as non-auth endpoint
- Pool.Next without wait → no accounts available → guest fallback → 404
- **Fixed:** added SearchTimeline to requiresAuth
- But even with auth, GET still returns 404

### 9. Account `active=false` in ephemeral client — FIXED, NOT THE CAUSE
- Accounts created via `&twitter.Account{...}` defaulted to `active=false`
- Pool.Next filter rejected inactive accounts
- **Fixed:** SetActive(true) after successful loadOrLogin
- But still 404 because GET

### 10. Rate limiter blocking with zero config — FIXED, NOT THE CAUSE
- Ephemeral client created without RateLimit config
- `RequestsPerWindow=0` → `Allow()` returns false for all requests
- **Fixed:** pass `RateLimit: ratelimit.Config{RequestsPerWindow: 50, WindowDuration: 15*time.Minute}`
- But still 404 because GET

## Changes Made (go-twitter v0.2.8 → v0.3.2)

| Version | Change |
|---------|--------|
| v0.2.8 | SearchTimeline added to requiresAuth (no guest fallback) |
| v0.2.9 | Updated query IDs, features, referer to x.com |
| v0.3.0 | XPFF header generator (xpff/ package) |
| v0.3.1 | SetActive(true) after successful loadOrLogin |
| v0.3.2 | Real guest_id extraction from x.com cookies |
| **v0.4.0** | **TODO: Switch SearchTimeline from GET to POST** |

## Changes Made (go-job)

- Migrated from hardcoded TWITTER_ACCOUNTS to go-social pool API
- Added `internal/social/client.go` (AcquireAccount + ReportUsage)
- twitter.go: social-first search with local fallback
- RateLimit config in ephemeral client
- Docker-compose: GO_SOCIAL_URL/TOKEN, removed TWITTER_ACCOUNTS

## Changes Made (go-social)

- AcquireResult includes Username field
- API response injects username into credentials map

## Community References

- gallery-dl#9275: SearchTimeline 404, author says "resolved itself" (intermittent)
- twscrape#298: "No account available for SearchTimeline" (broken since March 18)
- twikit#412: **FIX: switch from gql_get to gql_post** (March 29)
- twikit#408-411: Broken ondemand.s parsing (March 18)
- Scrapfly blog: "doc_ids rotate constantly, 10-15 hours monthly maintenance"

## Next Steps

1. **Implement POST for SearchTimeline in go-twitter** — the actual fix
2. Consider adding POST support for other endpoints Twitter may migrate
3. Add auto-extraction of query IDs from JS bundles (reduce manual updates)
4. Monitor twikit/twscrape/gallery-dl for future Twitter API changes
