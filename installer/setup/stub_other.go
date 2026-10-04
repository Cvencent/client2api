//go:build !windows

// The installer is Windows-only.  This stub exists so that `go build ./...`,
// `go vet ./...` and `go test ./...` keep working on the Linux and macOS
// machines CI runs on, instead of failing with "build constraints exclude all
// Go files".
package main

func main() {}
