package llmrunner

// SetRemote overrides the clone's remote URL. Tests use it to clone a local
// git repository instead of a real GitHub repo.
func (b *Backend) SetRemote(remote string) {
	b.remote = remote
}
