//go:build !darwin

package power

// Default is nil off macOS: the feature reports unsupported and the
// preference cannot be enabled. Linux inhibition is a separate follow-up.
func Default() Backend { return nil }
