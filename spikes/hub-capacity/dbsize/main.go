// Command dbsize breaks a hub's SQLite database down by table, so the growth a
// capacity run measured can be split into what retention keeps 30 days
// (events) and what it keeps 24 hours (state snapshots). It opens the file
// read-only and uses SQLite's dbstat table for on-disk bytes, indexes
// included.
//
//	go run ./spikes/hub-capacity/dbsize path/to/vyshka.db
package main

import (
	"database/sql"
	"fmt"
	"os"
	"sort"

	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: dbsize <vyshka.db>")
		os.Exit(2)
	}
	db, err := sql.Open("sqlite", "file:"+os.Args[1]+"?mode=ro")
	if err != nil {
		fail(err)
	}
	defer db.Close()
	// Indexes are attributed to their table.
	owner := map[string]string{}
	rows, err := db.Query(`SELECT name, tbl_name FROM sqlite_master WHERE type IN ('table','index')`)
	if err != nil {
		fail(err)
	}
	for rows.Next() {
		var name, table string
		if err := rows.Scan(&name, &table); err != nil {
			fail(err)
		}
		owner[name] = table
	}
	rows.Close()
	rows, err = db.Query(`SELECT name, SUM(pgsize) FROM dbstat GROUP BY name`)
	if err != nil {
		fail(fmt.Errorf("dbstat: %w", err))
	}
	bytes := map[string]int64{}
	var total int64
	for rows.Next() {
		var name string
		var size int64
		if err := rows.Scan(&name, &size); err != nil {
			fail(err)
		}
		table := owner[name]
		if table == "" {
			table = name
		}
		bytes[table] += size
		total += size
	}
	rows.Close()
	tables := make([]string, 0, len(bytes))
	for t := range bytes {
		tables = append(tables, t)
	}
	sort.Slice(tables, func(i, j int) bool { return bytes[tables[i]] > bytes[tables[j]] })
	fmt.Printf("%-28s %12s %10s %7s\n", "table (with its indexes)", "rows", "MiB", "share")
	for _, t := range tables {
		if bytes[t] < 64<<10 {
			continue
		}
		var count int64
		if err := db.QueryRow(`SELECT COUNT(*) FROM "` + t + `"`).Scan(&count); err != nil {
			count = -1
		}
		fmt.Printf("%-28s %12d %10.1f %6.1f%%\n", t, count, float64(bytes[t])/(1<<20), 100*float64(bytes[t])/float64(total))
	}
	fmt.Printf("%-28s %12s %10.1f\n", "total", "", float64(total)/(1<<20))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
