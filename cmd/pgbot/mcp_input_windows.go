//go:build windows

package main

import "io"

func prepareMCPInput(in io.Reader) (io.Reader, func(), error) {
	return in, func() {}, nil
}
