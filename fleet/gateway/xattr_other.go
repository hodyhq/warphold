//go:build !linux

package gateway

import "errors"

// WarpHold's Fleet server is Linux-only; elsewhere the ETag is hashed on read.
func setETag(uintptr, string) error { return errors.ErrUnsupported }

func getETag(uintptr) (string, error) { return "", errors.ErrUnsupported }
