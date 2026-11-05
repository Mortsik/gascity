package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doltpool"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Change-token-gated reuse of a control-ready scan's bd leg (agentforge fork).
//
// controlReadyLegsReady reads every ledger leg on every serve tick. On a quiet
// rig the bd leg is the same answer hundreds of times an hour: measured on a
// Dolt-backed rig (2026-11-05), 803 scans/h, every one queue=0, ~606 ready
// rows hydrated per `bd ready`, 74% of the hour spent inside the read — while
// the rig's issues table took 8 writes in that hour.
//
// The reuse is keyed on the content of the database that leg's `bd` reads:
// DOLT_HASHOF_DB() plus DOLT_HASHOF_TABLE of every base table, ignored wisp
// tables included (controlReadyDatabaseToken). Both are working-set hashes, so
// uncommitted writes change them and a no-op write does not. The endpoint the
// token is taken from is derived from the very environment gascity hands that
// leg's `bd` (controlReadyBdLegEnv), so the token and the read cannot name two
// different databases — not for a legacy rig endpoint override, not for a
// doltlite or proxied scope (those are refused outright).
//
// The token is taken BEFORE the read it labels: a write that lands between the
// two leaves the next token different, so it can only cost a fresh read, never
// hide a change. Every write that changes readiness — the dispatcher closing a
// control bead, a worker closing a blocker — changes the hash, so neither stale
// arm described on controlReadyLegsReady (ga-vnycm2.9) can come back. The only
// readiness change without a write is a defer_until passing;
// controlReadyReuseMaxAge bounds that delay.
//
// Only the scope's own leg is ever reused, and only when its engine is a
// *beads.BdStore. Every other leg — the shared graph binding of a federated
// scope, the binding of a relocated one, file, SQLite and native legs — is read
// afresh on every scan exactly as before, and any token error reads afresh too.

// controlReadyReuseMaxAgeEnv overrides controlReadyReuseDefaultMaxAge with a Go
// duration; "0" disables reuse (kill switch).
const controlReadyReuseMaxAgeEnv = "GC_CONTROL_READY_REUSE_MAX_AGE"

const (
	controlReadyReuseDefaultMaxAge = 30 * time.Second
	controlReadyTokenTimeout       = 2 * time.Second
)

type controlReadyReuseEntry struct {
	token string
	ready []beads.Bead
	at    time.Time
}

var (
	controlReadyReuseMu    sync.Mutex
	controlReadyReuseCache = map[string]controlReadyReuseEntry{}

	// Test seams.
	controlReadyChangeTokenFn = controlReadyChangeToken
	controlReadyNow           = time.Now
	controlReadyLegReusable   = controlReadyLegReusableDefault
)

// controlReadyLegReusableDefault admits only bd legs: their reads go through
// the bd environment the token's endpoint is derived from.
func controlReadyLegReusableDefault(engine beads.Store) bool {
	_, ok := engine.(*beads.BdStore)
	return ok
}

// controlReadyReuseMaxAge reads the kill switch / override; an unparseable
// value is logged and treated as the default.
func controlReadyReuseMaxAge() time.Duration {
	raw := strings.TrimSpace(os.Getenv(controlReadyReuseMaxAgeEnv))
	if raw == "" {
		return controlReadyReuseDefaultMaxAge
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		log.Printf("control-ready reuse: ignoring %s=%q (want a non-negative Go duration)", controlReadyReuseMaxAgeEnv, raw)
		return controlReadyReuseDefaultMaxAge
	}
	return d
}

// controlReadyReadScopedLeg answers the scope's own bd leg, reusing the last
// answer when the database it reads is provably unchanged.
func controlReadyReadScopedLeg(dir, cityPath string, cfg *config.City, engine beads.Store) ([]beads.Bead, error) {
	maxAge := controlReadyReuseMaxAge()
	if maxAge <= 0 || !controlReadyLegReusable(engine) {
		return controlReadyLegReady(dir, engine, true)
	}
	at := controlReadyNow()
	token := controlReadyChangeTokenFn(dir, cityPath, cfg)
	if token == "" {
		// No proof the database is unchanged: drop the old answer now, so a read
		// failing below cannot leave it behind for a later token.
		controlReadyReuseStore(dir, "", nil, at)
	}
	if cached, hit := controlReadyReuseLookup(dir, token, maxAge); hit {
		workflowTracef("control-ready scan reused unchanged bd leg ready=%d", len(cached))
		return cached, nil
	}
	leg, err := controlReadyLegReady(dir, engine, true)
	if err != nil {
		return nil, err
	}
	controlReadyReuseStore(dir, token, leg, at)
	return leg, nil
}

// controlReadyEndpoint is the Dolt endpoint a scope's bd process connects to.
type controlReadyEndpoint struct {
	host, port, socket, user, password, database string
}

// controlReadyChangeToken returns the content hash of the database the scope's
// bd leg reads, or "" when that cannot be established — "" always means "read
// afresh".
func controlReadyChangeToken(dir, cityPath string, cfg *config.City) string {
	scopeRoot := resolveStoreScopeRoot(cityPath, dir)
	if scopeUsesProxiedDoltMode(cityPath, scopeRoot) {
		return ""
	}
	env, err := controlReadyBdLegEnv(scopeRoot, cityPath, cfg)
	if err != nil {
		return ""
	}
	database := ""
	if target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityPath, scopeRoot); err == nil {
		database = target.Database
	}
	endpoint, ok := controlReadyEndpointFromEnv(env, database)
	if !ok {
		return ""
	}
	var db *sql.DB
	if endpoint.socket != "" {
		db, err = doltpool.OpenSocket(endpoint.socket, endpoint.user, endpoint.password, endpoint.database)
	} else {
		db, err = doltpool.Open(managedDoltConnectHost(endpoint.host), endpoint.port, endpoint.user, endpoint.password, endpoint.database)
	}
	if err != nil {
		return ""
	}
	// The handle belongs to internal/doltpool; it must not be closed here.
	ctx, cancel := context.WithTimeout(context.Background(), controlReadyTokenTimeout)
	defer cancel()
	token, err := controlReadyDatabaseToken(ctx, db)
	if err != nil {
		return ""
	}
	return token
}

// controlReadyBdLegEnv rebuilds the environment the scope's control bd store
// runs bd with, choosing the city or rig builder exactly as
// openControlStoreAtForCity chooses controlBdStoreForCity or
// controlBdStoreForRig. The NoRecovery variants never start or repair a
// server: a token probe must not have side effects.
func controlReadyBdLegEnv(scopeRoot, cityPath string, cfg *config.City) (map[string]string, error) {
	if samePath(scopeRoot, cityPath) {
		return bdRuntimeEnvWithErrorNoRecovery(cityPath)
	}
	return bdRuntimeEnvForRigWithErrorNoRecovery(cityPath, cfg, scopeRoot)
}

// controlReadyEndpointFromEnv reads the endpoint bd will use out of its
// environment. A doltlite backend reads a local file, not a server, so it has
// no endpoint; neither does an environment naming no socket and no port. The
// database is the env's when it names one, else the scope's own record of it
// (.beads metadata, which bd itself falls back to).
func controlReadyEndpointFromEnv(env map[string]string, database string) (controlReadyEndpoint, bool) {
	for _, key := range []string{"BEADS_BACKEND", "GC_BEADS_BACKEND"} {
		if strings.EqualFold(strings.TrimSpace(env[key]), "doltlite") {
			return controlReadyEndpoint{}, false
		}
	}
	endpoint := controlReadyEndpoint{
		host:     strings.TrimSpace(env["BEADS_DOLT_SERVER_HOST"]),
		port:     strings.TrimSpace(env["BEADS_DOLT_SERVER_PORT"]),
		socket:   strings.TrimSpace(env["BEADS_DOLT_SERVER_SOCKET"]),
		user:     strings.TrimSpace(env["BEADS_DOLT_SERVER_USER"]),
		password: env["BEADS_DOLT_PASSWORD"],
		database: strings.TrimSpace(env["BEADS_DOLT_SERVER_DATABASE"]),
	}
	if endpoint.database == "" {
		endpoint.database = strings.TrimSpace(database)
	}
	if endpoint.user == "" {
		endpoint.user = "root"
	}
	if endpoint.database == "" || (endpoint.socket == "" && endpoint.port == "") {
		return controlReadyEndpoint{}, false
	}
	return endpoint, true
}

// controlReadyDatabaseToken hashes the database a ready read sees: the
// database hash plus the hash of every base table, one by one. The per-table
// hashes are not redundant. beads keeps wisps, wisp_% and other tables in
// dolt_ignore, Dolt documents DOLT_HASHOF_DB() as excluding ignored tables, and
// a TierBoth ready read draws on exactly those wisp tables — so the database
// hash alone cannot be trusted to move when a wisp is created, closed or
// re-wired. DOLT_HASHOF_TABLE reads the working set whether or not the table is
// ignored. Views carry no hash of their own and are skipped (their base tables
// are covered).
func controlReadyDatabaseToken(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, "SHOW FULL TABLES WHERE Table_type = 'BASE TABLE'")
	if err != nil {
		return "", err
	}
	var tables []string
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			_ = rows.Close()
			return "", err
		}
		tables = append(tables, name)
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	sort.Strings(tables)

	query := strings.Builder{}
	query.WriteString("SELECT DOLT_HASHOF_DB()")
	args := make([]any, 0, len(tables))
	for _, table := range tables {
		query.WriteString(", DOLT_HASHOF_TABLE(?)")
		args = append(args, table)
	}
	hashes := make([]sql.NullString, len(tables)+1)
	dest := make([]any, len(hashes))
	for i := range hashes {
		dest[i] = &hashes[i]
	}
	if err := db.QueryRowContext(ctx, query.String(), args...).Scan(dest...); err != nil {
		return "", err
	}
	parts := make([]string, 0, len(hashes))
	for i, h := range hashes {
		if !h.Valid || strings.TrimSpace(h.String) == "" {
			return "", fmt.Errorf("empty hash at column %d", i)
		}
		name := "db"
		if i > 0 {
			name = tables[i-1]
		}
		parts = append(parts, name+"="+strings.TrimSpace(h.String))
	}
	return strings.Join(parts, ";"), nil
}

// controlReadyReuseLookup returns the cached ready set for dir when its token
// matches and it is younger than maxAge.
func controlReadyReuseLookup(dir, token string, maxAge time.Duration) ([]beads.Bead, bool) {
	if token == "" || maxAge <= 0 {
		return nil, false
	}
	controlReadyReuseMu.Lock()
	defer controlReadyReuseMu.Unlock()
	entry, ok := controlReadyReuseCache[dir]
	if !ok || entry.token != token || controlReadyNow().Sub(entry.at) >= maxAge {
		return nil, false
	}
	return append([]beads.Bead(nil), entry.ready...), true
}

// controlReadyReuseStore records ready under token, or drops dir's entry when
// there is no token to key it on.
func controlReadyReuseStore(dir, token string, ready []beads.Bead, at time.Time) {
	controlReadyReuseMu.Lock()
	defer controlReadyReuseMu.Unlock()
	if token == "" {
		delete(controlReadyReuseCache, dir)
		return
	}
	controlReadyReuseCache[dir] = controlReadyReuseEntry{
		token: token,
		ready: append([]beads.Bead(nil), ready...),
		at:    at,
	}
}
