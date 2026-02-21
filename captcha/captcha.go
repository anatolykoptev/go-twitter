package captcha

import "context"

// Solver abstracts CAPTCHA solving services (Capsolver, 2captcha, etc.).
type Solver interface {
	// Solve submits a CAPTCHA challenge and returns the solution token.
	// siteKey is the Arkose/FunCaptcha public key, pageURL is the page triggering the challenge.
	Solve(ctx context.Context, siteKey, pageURL string) (token string, err error)

	// Balance returns the account balance in USD.
	Balance(ctx context.Context) (float64, error)
}
