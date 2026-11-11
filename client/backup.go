package client

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// TakeBackup has the hub snapshot its SQLite database into its configured
// backup directory, under a name the hub chooses, and rotate that directory
// down to its bound. It answers once the file is complete. A hub on Postgres
// answers 409 conflict. Requires admin. This is an operation of the reference
// hub, not of the protocol.
func (c *Client) TakeBackup(ctx context.Context) (BackupResult, error) {
	var result BackupResult
	err := c.do(ctx, http.MethodPost, []string{"api", "v1", "admin", "backup"}, nil, nil, &result)
	return result, err
}

// BackupResult is the answer to TakeBackup.
type BackupResult struct {
	Backup Backup `json:"backup"`
	// Kept is every snapshot in the directory after rotation, newest first.
	Kept []string `json:"kept"`
	// Pruned names the snapshots rotation deleted.
	Pruned []string        `json:"pruned"`
	Raw    json.RawMessage `json:"-"`
}

func (r *BackupResult) UnmarshalJSON(data []byte) error {
	type plain BackupResult
	return jsonUnmarshalRaw(data, (*plain)(r), &r.Raw)
}

// Backup is one snapshot file.
type Backup struct {
	Name string `json:"name"`
	// Path is the file's absolute path on the hub's host.
	Path       string    `json:"path"`
	SizeBytes  int64     `json:"sizeBytes"`
	TakenAt    time.Time `json:"takenAt"`
	DurationMs int64     `json:"durationMs"`
}
