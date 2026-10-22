//go:build !linux && !darwin

package app

import "errors"

func lockCopyTarget(string) (func(), error) {
	return nil, errors.New("model copy file locking is unsupported on this platform")
}
