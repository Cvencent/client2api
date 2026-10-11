//go:build !windows

package doubao

import "fmt"

// sqlite_other.go keeps the client-import path compiling off Windows.  The
// Doubao desktop client ships for Windows, so there is nothing to read there;
// the reader exists only so the portable clientimport.go builds and so
// profileSignedIn can report "not signed in" instead of failing to compile.

func sqliteSupported() bool { return false }

// sqliteDB is the platform stub; it carries no state.
type sqliteDB struct{}

func openSQLite(path string) (*sqliteDB, error) {
	return nil, fmt.Errorf("doubao: reading the desktop client cookie database is supported only on Windows")
}

func closeSQLite(*sqliteDB) error { return nil }

func (db *sqliteDB) queryRows(string) ([][]([]byte), error) {
	return nil, fmt.Errorf("doubao: reading the desktop client cookie database is supported only on Windows")
}
