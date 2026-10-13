//go:build darwin || linux

package main

import (
	"golang.org/x/sys/unix"
	"io"
	"os"
	"sync"
)

// prepareMCPInput gives inherited stdin a command-owned wakeup descriptor.
// os.Stdin is created from an inherited blocking descriptor, so Close does not
// reliably interrupt an outstanding Read on all supported Unix platforms.
func prepareMCPInput(in io.Reader) (io.Reader, func(), error) {
	file, ok := in.(*os.File)
	if !ok || file != os.Stdin {
		return in, func() {}, nil
	}
	wakeRead, wakeWrite, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	r := &interruptibleStdin{
		input:     file,
		wakeRead:  wakeRead,
		wakeWrite: wakeWrite,
	}
	return r, func() {
		_ = r.Close()
		_ = wakeRead.Close()
	}, nil
}

type interruptibleStdin struct {
	input     *os.File
	wakeRead  *os.File
	wakeWrite *os.File
	closeOnce sync.Once
}

func (r *interruptibleStdin) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	fds := []unix.PollFd{
		{Fd: int32(r.input.Fd()), Events: unix.POLLIN},
		{Fd: int32(r.wakeRead.Fd()), Events: unix.POLLIN},
	}
	for {
		if _, err := unix.Poll(fds, -1); err != nil {
			if err == unix.EINTR {
				continue
			}
			return 0, err
		}
		if fds[1].Revents != 0 {
			return 0, os.ErrClosed
		}
		if fds[0].Revents&unix.POLLNVAL != 0 {
			return 0, os.ErrClosed
		}
		if fds[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0 {
			return r.input.Read(p)
		}
	}
}

func (r *interruptibleStdin) Close() error {
	var err error
	r.closeOnce.Do(func() { err = r.wakeWrite.Close() })
	return err
}
