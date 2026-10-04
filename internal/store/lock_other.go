//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package store

import (
	"errors"
	"os"
)

func lockDatabase(string) (*os.File, error) {
	return nil, errors.New("database process locking requires Linux, macOS or BSD")
}
