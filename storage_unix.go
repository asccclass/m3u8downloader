//go:build !windows

package main

import "os"

func secureDirectory(path string) error {
	if e := os.MkdirAll(path, 0700); e != nil {
		return e
	}
	return os.Chmod(path, 0700)
}
func replaceFile(from, to string) error { return os.Rename(from, to) }
func secureFile(path string) error      { return os.Chmod(path, 0600) }
