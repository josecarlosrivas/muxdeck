//go:build !darwin

package tcc

// TCC is a macOS concern; everywhere else there is nothing to detect and
// nothing to record.
const supported = false

func probeDirs() []Dir { return nil }
