//go:build !darwin

package tart

// Tart runs on macOS; this path also supports provider tests on other hosts.
func (p *startupProcess) wait() error {
	err := p.cmd.Wait()
	p.mu.Lock()
	return err
}
