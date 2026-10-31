// Package xtidprobe is the seam between cmd/xtid-probe and the packages whose
// unexported internals it measures. xtid and the root package assign the hooks
// from init, so the probe needs no exported API on them (nothing here is a
// semver commitment: the package is internal).
package xtidprobe

import (
	"context"

	"github.com/anatolykoptev/go-twitter/internal/bundle"
)

// Transaction is a ClientTransaction as the probe sees it.
type Transaction interface {
	GenerateID(method, path string) string
	// AnimationKey is key material; the probe compares keys and never prints them.
	AnimationKey() string
}

var (
	// FetchMaterial fetches the warm page and the indices file for pageURL
	// without touching any manager's cached keys. Set by package xtid.
	FetchMaterial func(ctx context.Context, f bundle.Fetcher, pageURL string) (page, indices string, err error)
	// NewTransaction builds the current (fixed) algorithm's transaction. Set by
	// package xtid.
	NewTransaction func(page, indices string) (Transaction, error)
	// GraphQLFeatures returns the library's GraphQL feature switches. Set by the
	// root package.
	GraphQLFeatures func() map[string]any
)
