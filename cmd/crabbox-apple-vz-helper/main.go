package main

import (
	"os"

	"github.com/openclaw/crabbox/internal/applevzhelper"
)

func main() {
	os.Exit(applevzhelper.RunCLI(os.Args[1:], os.Stdout, os.Stderr))
}
