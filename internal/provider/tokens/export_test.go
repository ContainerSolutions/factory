package tokens

// Waiters is the number of callers waiting on the in-flight mint for (identity, repo, perms).
func (m *Minter) Waiters(identity, repo string, perms map[string]string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.flight.waitersLocked(scope{identity: identity, repo: repo, perms: perms}.key())
}
