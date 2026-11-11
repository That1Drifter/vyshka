package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/That1Drifter/vyshka/hub"
	"github.com/That1Drifter/vyshka/hub/store"
)

// openForMaintenance opens a database that already exists and is at the
// schema this binary would migrate it to. It never creates a database (a
// typo in -db would otherwise leave an empty file and report on it) and never
// migrates one: these commands are meant to be safe to point at a database a
// hub owns, and a migration is that hub's job at its next boot. readOnly
// opens it so that not a byte of the file changes (store.OpenReadOnly), which
// is what storage needs to report on a snapshot without altering it.
func openForMaintenance(ctx context.Context, dsn string, readOnly bool) (*store.Store, error) {
	if path, isSQLite := store.SQLitePath(dsn); isSQLite {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("database: %w", err)
		}
	}
	open := store.Open
	if readOnly {
		open = store.OpenReadOnly
	}
	st, err := open(ctx, dsn)
	if err != nil {
		return nil, err
	}
	migrations, err := store.Migrations()
	if err != nil {
		st.Close()
		return nil, err
	}
	version, err := st.SchemaVersion(ctx)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("schema version: %w", err)
	}
	if want := migrations[len(migrations)-1].Version; version != want {
		st.Close()
		return nil, fmt.Errorf("the database is at schema %d and this binary expects %d; boot a hub of this version on it once first", version, want)
	}
	return st, nil
}

func maintenanceFlags(name string) (*flag.FlagSet, *string) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	configFlag(flags)
	// Not defaulted from the environment for the same reason serve's is not:
	// a Postgres DSN carries a password, and flag defaults are printed.
	dsn := flags.String("db", "", "database DSN, or file:/path/to/secret; empty means local SQLite (env DATABASE_URL)")
	return flags, dsn
}

// maintenanceDSN settles -db for a maintenance command: the flag, then
// DATABASE_URL, then the db key of the config file named by -config (the
// same file serve reads, whose other keys these commands ignore but still
// hold to serve's names and types), with file: indirection from each.
func maintenanceDSN(flags *flag.FlagSet) (string, error) {
	resolved, err := resolveSettings(flags, func() (*flag.FlagSet, error) {
		serveFlags := flag.NewFlagSet("serve", flag.ContinueOnError)
		_, err := declareServeFlags(serveFlags)
		return serveFlags, err
	})
	if err != nil {
		return "", err
	}
	if resolved.warning != nil {
		fmt.Fprintln(os.Stderr, "warning: "+resolved.warning.text())
	}
	return resolved.secret("db", flags.Lookup("db").Value.String())
}

func runStorage(args []string) error {
	flags, _ := maintenanceFlags("storage")
	asJSON := flags.Bool("json", false, "print the report as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	database, err := maintenanceDSN(flags)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	st, err := openForMaintenance(ctx, database, true)
	if err != nil {
		return err
	}
	defer st.Close()

	report, err := st.Report(ctx, store.ReportOptions{WebhookDeliveryRetention: hub.DefaultWebhookDeliveryRetention})
	if err != nil {
		return err
	}
	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}

	fmt.Printf("%s database %s, at %s\n", report.Driver, report.Target, report.At.Format(time.RFC3339))
	if report.Driver == "sqlite" {
		fmt.Printf("file %s, of which free %s (a vacuum returns it), WAL %s\n",
			bytes(report.FileBytes), bytes(report.FreeBytes), bytes(report.WALBytes))
	}
	fmt.Println()
	table := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(table, "TABLE\tROWS\tSIZE\t")
	for _, t := range report.Tables {
		fmt.Fprintf(table, "%s\t%d\t%s\t\n", t.Name, t.Rows, bytes(t.Bytes))
	}
	table.Flush()
	fmt.Println()
	fmt.Println("What the background passes would do if they ran now (each pass works in bounded batches):")
	table = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "PASS\tTABLE\tEFFECT\tROWS")
	for _, sweep := range report.Sweeps {
		fmt.Fprintf(table, "%s\t%s\t%s\t%d\n", sweep.Sweep, sweep.Table, sweep.Effect, sweep.Rows)
	}
	if err := table.Flush(); err != nil {
		return err
	}
	fmt.Println("(event_batches counts every expired dedup marker; the event pass removes a server's")
	fmt.Println(" markers only once that server has no expired event left, so while events remain it is")
	fmt.Println(" an upper bound on what one pass removes)")
	return nil
}

func runVacuum(args []string) error {
	flags, _ := maintenanceFlags("vacuum")
	if err := flags.Parse(args); err != nil {
		return err
	}
	database, err := maintenanceDSN(flags)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	st, err := openForMaintenance(ctx, database, false)
	if err != nil {
		return err
	}
	defer st.Close()

	before, err := st.Report(ctx, store.ReportOptions{WebhookDeliveryRetention: hub.DefaultWebhookDeliveryRetention})
	if err != nil {
		return err
	}
	started := time.Now()
	if err := st.Vacuum(ctx); err != nil {
		if errors.Is(err, store.ErrSnapshotUnsupported) {
			return errors.New("vacuum is for SQLite; Postgres reclaims space with autovacuum (or VACUUM FULL, which locks each table it rewrites)")
		}
		return err
	}
	after, err := st.Report(ctx, store.ReportOptions{WebhookDeliveryRetention: hub.DefaultWebhookDeliveryRetention})
	if err != nil {
		return err
	}
	fmt.Printf("vacuumed %s in %s: %s -> %s\n", after.Target, time.Since(started).Round(time.Millisecond),
		bytes(before.FileBytes), bytes(after.FileBytes))
	return nil
}

func bytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
