package main

import (
	"fmt"
	"runtime"
)

func cmdVersion(args []string) {
	if wantsHelp(args) {
		showHelp(helpSpec{
			name:    "version",
			summary: "print the opod binary version + build info",
			usage:   "opod version",
		})
	}
	fmt.Printf("opod %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
}
