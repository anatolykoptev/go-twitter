package xtid

import (
	"context"

	"github.com/anatolykoptev/go-twitter/internal/bundle"
	"github.com/anatolykoptev/go-twitter/internal/xtidprobe"
)

// probeTx exposes the animation key to cmd/xtid-probe through the internal
// xtidprobe seam only; the method is unreachable outside this module.
type probeTx struct{ *ClientTransaction }

func (p probeTx) AnimationKey() string { return p.animationKey }

func init() {
	xtidprobe.FetchMaterial = func(ctx context.Context, f bundle.Fetcher, pageURL string) (string, string, error) {
		return NewManager(f).fetchMaterial(ctx, retryFetcher{f}, pageURL)
	}
	xtidprobe.NewTransaction = func(page, indices string) (xtidprobe.Transaction, error) {
		ct, err := newClientTransaction(page, indices)
		if err != nil {
			return nil, err
		}
		return probeTx{ct}, nil
	}
}
