//go:build integration

package main

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/doltpool"
)

// startControlReadyTokenDolt launches a throwaway dolt sql-server with a
// database shaped like a beads store on the axis the change token depends on:
// versioned issue tables, the wisp tables and events under dolt_ignore (the
// patterns beads' schema registers), and a view.
func startControlReadyTokenDolt(t *testing.T) *sql.DB {
	t.Helper()
	doltBin, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt binary not in PATH")
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	if err := lis.Close(); err != nil {
		t.Fatalf("release test port: %v", err)
	}
	dataDir := t.TempDir()
	cmd := exec.Command(doltBin, "sql-server", "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--data-dir", dataDir)
	cmd.Env = append(os.Environ(), "DOLT_ROOT_PATH="+dataDir)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dolt sql-server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	admin, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/", port))
	if err != nil {
		t.Fatalf("open dolt connection: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	deadline := time.Now().Add(30 * time.Second)
	for admin.Ping() != nil {
		if time.Now().After(deadline) {
			t.Fatalf("dolt sql-server did not become ready on port %d", port)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := admin.Exec("CREATE DATABASE beads"); err != nil {
		t.Fatalf("create database: %v", err)
	}

	db, err := doltpool.Open("127.0.0.1", strconv.Itoa(port), "root", "", "beads")
	if err != nil {
		t.Fatalf("open pooled handle: %v", err)
	}
	for _, stmt := range []string{
		"INSERT INTO dolt_ignore VALUES ('wisps', true), ('wisp_%', true), ('events', true)",
		"CREATE TABLE issues (id VARCHAR(64) PRIMARY KEY, status VARCHAR(32))",
		"CREATE TABLE dependencies (issue_id VARCHAR(64), depends_on_id VARCHAR(64), PRIMARY KEY (issue_id, depends_on_id))",
		"CREATE TABLE wisps (id VARCHAR(64) PRIMARY KEY, status VARCHAR(32))",
		"CREATE TABLE wisp_dependencies (issue_id VARCHAR(64), depends_on_id VARCHAR(64), PRIMARY KEY (issue_id, depends_on_id))",
		"CREATE TABLE events (id INT PRIMARY KEY)",
		"CREATE VIEW open_issues AS SELECT id FROM issues WHERE status = 'open'",
		"INSERT INTO issues VALUES ('bd-1', 'open')",
		"INSERT INTO wisps VALUES ('bd-wisp-1', 'open')",
		"CALL DOLT_COMMIT('-Am', 'seed')",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return db
}

// TestControlReadyDatabaseTokenTracksIgnoredWispTables proves the token moves
// with every write a TierBoth ready read can see — including writes confined
// to dolt_ignore'd wisp tables — and stays put across reads and no-op writes.
func TestControlReadyDatabaseTokenTracksIgnoredWispTables(t *testing.T) {
	db := startControlReadyTokenDolt(t)
	ctx := context.Background()
	token := func() string {
		t.Helper()
		got, err := controlReadyDatabaseToken(ctx, db)
		if err != nil {
			t.Fatalf("controlReadyDatabaseToken: %v", err)
		}
		return got
	}

	prev := token()
	if again := token(); again != prev {
		t.Fatalf("token moved without a write: %q -> %q", prev, again)
	}

	// table names the component of the token that must move on its own: the
	// per-table hashes are what keep the token honest for dolt_ignore'd tables
	// on any Dolt whose DOLT_HASHOF_DB() leaves them out, so the test pins
	// them directly rather than trusting the database hash to move.
	for _, step := range []struct {
		name    string
		stmt    string
		changes bool
		table   string
	}{
		{"no-op write", "UPDATE issues SET status = 'open' WHERE id = 'bd-1'", false, ""},
		{"create a wisp", "INSERT INTO wisps VALUES ('bd-wisp-2', 'open')", true, "wisps"},
		{"close a wisp", "UPDATE wisps SET status = 'closed' WHERE id = 'bd-wisp-1'", true, "wisps"},
		{"wire a wisp dependency", "INSERT INTO wisp_dependencies VALUES ('bd-wisp-2', 'bd-1')", true, "wisp_dependencies"},
		{"drop a wisp dependency", "DELETE FROM wisp_dependencies WHERE issue_id = 'bd-wisp-2'", true, "wisp_dependencies"},
		{"close an issue", "UPDATE issues SET status = 'closed' WHERE id = 'bd-1'", true, "issues"},
		{"add an issue dependency", "INSERT INTO dependencies VALUES ('bd-1', 'bd-wisp-2')", true, "dependencies"},
		{"commit only", "CALL DOLT_COMMIT('-Am', 'checkpoint')", false, ""},
	} {
		if _, err := db.Exec(step.stmt); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		next := token()
		if moved := next != prev; moved != step.changes {
			t.Fatalf("%s: token moved = %t, want %t (%q -> %q)", step.name, moved, step.changes, prev, next)
		}
		if step.table != "" {
			if before, after := tokenPart(t, prev, step.table), tokenPart(t, next, step.table); before == after {
				t.Fatalf("%s: the %s component did not move (%q)", step.name, step.table, after)
			}
		}
		prev = next
	}
}

// tokenPart returns the hash recorded for table in a controlReadyDatabaseToken.
func tokenPart(t *testing.T, token, table string) string {
	t.Helper()
	for _, part := range strings.Split(token, ";") {
		if name, hash, ok := strings.Cut(part, "="); ok && name == table {
			return hash
		}
	}
	t.Fatalf("token %q has no component for table %s", token, table)
	return ""
}
