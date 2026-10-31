package legacyxtid

// New builds the pre-fix ClientTransaction from a warm page and indices file.
func New(homeHTML, indicesJS string) (*ClientTransaction, error) {
	return newClientTransaction(homeHTML, indicesJS)
}

// AnimationKey returns the derived animation key (for key comparison only).
func (ct *ClientTransaction) AnimationKey() string { return ct.animationKey }
