package relay

// SetBeforeCommit installs a hook that runs after a batch is flushed and
// before it commits.
func (r *Relay) SetBeforeCommit(f func() error) { r.beforeCommit = f }
