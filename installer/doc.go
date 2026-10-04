// Package installer holds the Windows installer inputs for client2api: the
// NSIS script, the icon generator, and the guard tests that keep the packaged
// version and payload in sync with the source tree.
//
// It has no Go code to run -- the build entry point is build.ps1:
//
//	pwsh -File installer/build.ps1
//
// The tests here are the reason this is a package at all.  The installer
// repeats two facts that already live elsewhere (the source version string and
// the list of files that get staged), and repetition that nothing checks is
// repetition that rots.
package installer
