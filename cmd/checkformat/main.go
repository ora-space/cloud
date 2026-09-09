// checkformat makes formatting a failing gate instead of merely printing a diff.
package main

import (
	"fmt"
	"os"
	"os/exec"
)

func main() {
	output, e := exec.Command("go", "tool", "gofumpt", "-l", "-extra", ".").CombinedOutput()
	if e != nil {
		fmt.Fprintln(os.Stderr, string(output), e)
		os.Exit(1)
	}
	if len(output) != 0 {
		fmt.Fprintln(os.Stderr, "Run task format:\n"+string(output))
		os.Exit(1)
	}
}
