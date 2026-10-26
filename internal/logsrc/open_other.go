//go:build !windows

package logsrc

import "os"

func openShared(path string) (*os.File, error) { return os.Open(path) }
