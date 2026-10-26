//go:build !unix

package katahelper

import (
	"context"
	"errors"
)

// lock: the helper runs on Linux; without flock it allocates nothing.
func (h *helper) lock(context.Context) (func(), error) {
	return nil, errors.New("kata-helper needs flock")
}
