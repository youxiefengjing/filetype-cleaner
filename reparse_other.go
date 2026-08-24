//go:build !windows

package main

import "io/fs"

func isReparsePoint(info fs.FileInfo) bool {
	return false
}
