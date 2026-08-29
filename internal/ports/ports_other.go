//go:build !linux && !darwin

package ports

import "errors"

// errUnsupported keeps the package building everywhere Go does. Listening
// turns any error into "no ports", so a session row on such a platform simply
// carries no port chip.
var errUnsupported = errors.New("listening ports are not supported on this platform")

func parents() (map[int]int, error) { return nil, errUnsupported }

func listening([]int) (map[int][]int, error) { return nil, errUnsupported }
