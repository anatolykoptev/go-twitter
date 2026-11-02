// Package tweetflag gives the parsers and the client access to the unexported
// promoted marker on model.Tweet without adding public API to model. Package
// model installs the accessors in its init; every importer of model therefore
// sees them populated before any of its own initialisation runs.
package tweetflag

var (
	set func(t any, v bool)
	get func(t any) bool
)

// Install is called once by package model.
func Install(setFn func(t any, v bool), getFn func(t any) bool) {
	set, get = setFn, getFn
}

// Set marks t (a *model.Tweet) as a timeline ad or not.
func Set(t any, v bool) { set(t, v) }

// Promoted reports whether t (a *model.Tweet) was marked as a timeline ad.
func Promoted(t any) bool { return get(t) }
