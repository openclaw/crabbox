package main

import (
	"os"

	"github.com/openclaw/crabbox/internal/applevmhelper"
)

func main() {
	os.Exit(applevmhelper.RunCLI(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
