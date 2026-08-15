package main

import "os"

// main is the process entry point. All command wiring happens in root.go via
// package init() and RegisterCommand, so main itself only needs to invoke the
// root command and propagate its exit code to the operating system.
func main() {
	os.Exit(Execute())
}
