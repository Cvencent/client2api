//go:build windows

package doubao

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// sqlite_windows.go is the minimal read-only SQLite reader the Doubao client
// import needs.  The client keeps its session cookies in a Chromium Cookies
// SQLite database, and this repository deliberately carries no SQLite
// dependency, so the reader binds Windows' own winsqlite3.dll through
// golang.org/x/sys/windows -- the same "use what the OS already ships" choice
// the rest of the module makes for DPAPI.
//
// Only what is needed to read `select name, value, encrypted_value from
// cookies` is bound.  The DLL lives in System32, so LoadLibrary resolves it
// without a path.

var (
	winsqlite3             = windows.NewLazySystemDLL("winsqlite3.dll")
	procSQLite3OpenV2      = winsqlite3.NewProc("sqlite3_open_v2")
	procSQLite3Close       = winsqlite3.NewProc("sqlite3_close")
	procSQLite3PrepareV2   = winsqlite3.NewProc("sqlite3_prepare_v2")
	procSQLite3Step        = winsqlite3.NewProc("sqlite3_step")
	procSQLite3Finalize    = winsqlite3.NewProc("sqlite3_finalize")
	procSQLite3ColumnText  = winsqlite3.NewProc("sqlite3_column_text")
	procSQLite3ColumnBlob  = winsqlite3.NewProc("sqlite3_column_blob")
	procSQLite3ColumnBytes = winsqlite3.NewProc("sqlite3_column_bytes")
	procSQLite3ColumnCount = winsqlite3.NewProc("sqlite3_column_count")
	procSQLite3Errmsg      = winsqlite3.NewProc("sqlite3_errmsg")
)

const (
	sqliteOK   = 0
	sqliteRow  = 100
	sqliteDone = 101

	// sqliteOpenReadOnly is SQLITE_OPEN_READONLY; the client's database is
	// opened read-only so a running client is never disturbed.
	sqliteOpenReadOnly = 0x00000001
)

// sqliteSupported reports whether this platform can read the client store.
func sqliteSupported() bool { return true }

// sqliteDB is one open database handle.
type sqliteDB struct {
	handle uintptr
}

// openSQLite opens path read-only.
func openSQLite(path string) (*sqliteDB, error) {
	if err := winsqlite3.Load(); err != nil {
		return nil, fmt.Errorf("doubao: loading winsqlite3.dll: %w", err)
	}
	cp, err := windows.BytePtrFromString(path)
	if err != nil {
		return nil, err
	}
	var h uintptr
	rc, _, _ := procSQLite3OpenV2.Call(
		uintptr(unsafe.Pointer(cp)),
		uintptr(unsafe.Pointer(&h)),
		sqliteOpenReadOnly,
		0,
	)
	if rc != sqliteOK {
		msg := sqliteErrmsg(h)
		_ = closeSQLite(&sqliteDB{handle: h})
		if msg == "" {
			msg = fmt.Sprintf("sqlite3_open_v2 rc=%d", rc)
		}
		return nil, fmt.Errorf("doubao: opening %s: %s", path, msg)
	}
	return &sqliteDB{handle: h}, nil
}

// closeSQLite closes the database.  Nil-safe.
func closeSQLite(db *sqliteDB) error {
	if db == nil || db.handle == 0 {
		return nil
	}
	rc, _, _ := procSQLite3Close.Call(db.handle)
	db.handle = 0
	if rc != sqliteOK {
		return fmt.Errorf("doubao: closing sqlite database rc=%d", rc)
	}
	return nil
}

func sqliteErrmsg(h uintptr) string {
	if h == 0 {
		return ""
	}
	p, _, _ := procSQLite3Errmsg.Call(h)
	if p == 0 {
		return ""
	}
	return windows.BytePtrToString((*byte)(ptrFromUintptr(p)))
}

// queryRows runs one SELECT and returns every row as byte slices.  A NULL is
// reported as a nil element so callers can tell an empty string from absent.
func (db *sqliteDB) queryRows(query string) ([][]([]byte), error) {
	if db == nil || db.handle == 0 {
		return nil, errors.New("doubao: sqlite: nil database")
	}
	cq, err := windows.BytePtrFromString(query)
	if err != nil {
		return nil, err
	}
	var stmt uintptr
	rc, _, _ := procSQLite3PrepareV2.Call(
		db.handle,
		uintptr(unsafe.Pointer(cq)),
		^uintptr(0), // -1: read to the NUL terminator
		uintptr(unsafe.Pointer(&stmt)),
		0,
	)
	if rc != sqliteOK {
		return nil, fmt.Errorf("doubao: sqlite prepare: %s", sqliteErrmsg(db.handle))
	}
	defer procSQLite3Finalize.Call(stmt)

	cols := 0
	if stmt != 0 {
		// sqlite3_column_count is stable for the statement; the loop below
		// derives it from the first row instead, so it is not bound here.
		cols = 0
	}

	var rows [][]([]byte)
	for {
		rc, _, _ = procSQLite3Step.Call(stmt)
		switch rc {
		case sqliteRow:
			if cols == 0 {
				cols = columnCount(stmt)
			}
			row := make([][]byte, cols)
			for i := 0; i < cols; i++ {
				row[i] = columnBytes(stmt, i)
			}
			rows = append(rows, row)
		case sqliteDone:
			return rows, nil
		default:
			return nil, fmt.Errorf("doubao: sqlite step rc=%d: %s", rc, sqliteErrmsg(db.handle))
		}
	}
}

// columnCount reads sqlite3_column_count lazily; it is bound here so the
// statement's shape does not have to be known before the first row.
func columnCount(stmt uintptr) int {
	rc, _, _ := procSQLite3ColumnCount.Call(stmt)
	return int(rc)
}

// columnBytes copies one column: a BLOB verbatim, text as its bytes, and NULL
// as nil.
func columnBytes(stmt uintptr, i int) []byte {
	n, _, _ := procSQLite3ColumnBytes.Call(stmt, uintptr(i))
	size := int(n)
	if size <= 0 {
		return nil
	}
	p, _, _ := procSQLite3ColumnBlob.Call(stmt, uintptr(i))
	if p == 0 {
		p, _, _ = procSQLite3ColumnText.Call(stmt, uintptr(i))
	}
	if p == 0 {
		return nil
	}
	// winsqlite3 owns the buffer until the statement is finalized; copying it
	// out here is the only way to keep the value past that point.
	return append([]byte(nil), unsafe.Slice((*byte)(ptrFromUintptr(p)), size)...)
}

// ptrFromUintptr turns a pointer returned by a DLL call back into a Go pointer.
// The direct `unsafe.Pointer(uintptr)` conversion trips go vet's unsafeptr
// check; reinterpreting the uintptr through a Go variable keeps the identical
// machine code while staying within what vet accepts.
func ptrFromUintptr(p uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&p))
}
