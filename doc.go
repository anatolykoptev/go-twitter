// Package twitter is an unofficial client for the X (Twitter) web API.
//
// A [Client] holds a pool of accounts and exposes typed methods for timelines,
// search, profiles, posting and direct messages. Reads pick an account from the
// pool and rotate on failure; writes run on the account you pass. The client
// follows the rate limits X reports per account and endpoint, and failures
// surface as [*APIError] values with predicates such as [IsRateLimited].
//
// The response types ([Tweet], [TwitterUser], [DMConversation], [DMMessage]) are
// defined in package model and aliased here, so either import path works.
//
// Accounts authenticate with the auth_token and ct0 cookies of a logged-in
// x.com session. Treat both as passwords.
//
// See the docs directory of the repository for the guides on configuration,
// errors, rate limits and metrics.
package twitter
