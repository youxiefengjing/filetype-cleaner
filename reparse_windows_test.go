//go:build windows

package main

import (
	"io/fs"
	"syscall"
	"testing"
	"time"
)

type irregularReparseInfo struct{}

func (irregularReparseInfo) Name() string       { return "msm_ion_ids.h" }
func (irregularReparseInfo) Size() int64        { return 0 }
func (irregularReparseInfo) Mode() fs.FileMode  { return osModeIrregular }
func (irregularReparseInfo) ModTime() time.Time { return time.Time{} }
func (irregularReparseInfo) IsDir() bool        { return false }
func (irregularReparseInfo) Sys() any {
	return &syscall.Win32FileAttributeData{FileAttributes: syscall.FILE_ATTRIBUTE_REPARSE_POINT}
}

const osModeIrregular = fs.ModeIrregular

func TestIrregularReparsePointIsSupported(t *testing.T) {
	isLinkLike, supported := classifyFileInfo(irregularReparseInfo{})
	if !isLinkLike || !supported {
		t.Fatalf("isLinkLike=%v, supported=%v; want true, true", isLinkLike, supported)
	}
}
