//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "awgtunnel works only on Windows")
	os.Exit(2)
}
