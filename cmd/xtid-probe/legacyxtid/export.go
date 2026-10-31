package legacyxtid

// New builds the pre-fix ClientTransaction from a warm page and indices file.
func New(homeHTML, indicesJS string) (*ClientTransaction, error) {
	return newClientTransaction(homeHTML, indicesJS)
}
