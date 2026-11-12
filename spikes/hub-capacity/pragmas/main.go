// Command pragmas reports the SQLite durability settings the hub runs with:
// it opens a database through the same driver and DSN form the hub uses
// (modernc.org/sqlite, a bare file path), applies the hub's three pragmas, and
// prints journal_mode, synchronous, and wal_autocheckpoint. The hub sets no
// synchronous pragma of its own, so this is the driver's default under WAL.
//
//	go run ./spikes/hub-capacity/pragmas [path]
package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

func main() {
	path := filepath.Join(os.TempDir(), "vyshka-pragmas-probe.db")
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	defer os.Remove(path)
	defer os.Remove(path + "-wal")
	defer os.Remove(path + "-shm")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode = WAL", "PRAGMA busy_timeout = 5000", "PRAGMA foreign_keys = ON"} {
		if _, err := db.Exec(pragma); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	for _, name := range []string{"journal_mode", "synchronous", "wal_autocheckpoint", "page_size"} {
		var value string
		if err := db.QueryRow("PRAGMA " + name).Scan(&value); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("%s=%s\n", name, value)
	}
}
