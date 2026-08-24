//go:build gui && !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "GUI 版本当前仅支持 Windows。")
	os.Exit(1)
}
