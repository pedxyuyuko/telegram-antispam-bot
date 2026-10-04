//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package store

import (
	"fmt"
	"os"
	"syscall"
)

func lockDatabase(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("database already in use: %w", err)
	}
	return f, nil
}
