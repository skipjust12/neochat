//go:build !linux

package terminal

// HideServerSecrets does nothing here: only Linux has /proc/<pid>/environ
// and the switch that closes it.
func HideServerSecrets() error { return nil }
