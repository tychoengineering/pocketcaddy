package pocketcaddy

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

// store holds the databases and named queries discovered under the data root.
type store struct {
	dbs     map[string]*sql.DB
	queries map[string]*Query
}

// dbSuffixes are the file extensions treated as SQLite databases.
var dbSuffixes = []string{".sqlite", ".sqlite3", ".db"}

// openStore scans root for SQLite databases and root/sqlDir for .sql query
// files. Databases are opened read-only; queries are validated against the
// database they name so misconfiguration surfaces at startup, not per-request.
func openStore(root, sqlDir string, maxConns int) (*store, error) {
	s := &store{
		dbs:     map[string]*sql.DB{},
		queries: map[string]*Query{},
	}

	if err := s.openDatabases(root, maxConns); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.loadQueries(sqlDir); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *store) openDatabases(root string, maxConns int) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("reading data dir %s: %w", root, err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if !slices.Contains(dbSuffixes, ext) {
			continue
		}

		key := strings.TrimSuffix(name, filepath.Ext(name))
		if _, dup := s.dbs[key]; dup {
			return fmt.Errorf("duplicate database name %q in %s", key, root)
		}

		path := filepath.Join(root, name)
		db, err := openReadOnly(path, maxConns)
		if err != nil {
			return fmt.Errorf("opening database %s: %w", path, err)
		}
		s.dbs[key] = db
	}

	if len(s.dbs) == 0 {
		return fmt.Errorf("no SQLite databases found in %s", root)
	}
	return nil
}

// openReadOnly opens a SQLite file in read-only mode. Both the URI `mode=ro`
// flag and a `query_only` pragma are set: the former blocks writes at the VFS
// layer, the latter blocks any statement the connection could otherwise use to
// mutate state (including ATTACH-ed databases).
func openReadOnly(path string, maxConns int) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	dsn := "file:" + url.PathEscape(abs) +
		"?mode=ro" +
		"&_pragma=query_only(1)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func (s *store) loadQueries(sqlDir string) error {
	info, err := os.Stat(sqlDir)
	if err != nil {
		return fmt.Errorf("reading sql dir %s: %w", sqlDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", sqlDir)
	}

	err = filepath.WalkDir(sqlDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".sql") {
			return nil
		}

		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(sqlDir, path)
		if err != nil {
			rel = filepath.Base(path)
		}
		// Nested files get slash-separated names: sql/reports/daily.sql ->
		// "reports/daily".
		defaultName := strings.TrimSuffix(filepath.ToSlash(rel), filepath.Ext(rel))

		q, err := parseSQLFile(defaultName, string(src))
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if _, dup := s.queries[q.Name]; dup {
			return fmt.Errorf("%s: duplicate query name %q", path, q.Name)
		}
		if _, ok := s.dbs[q.DB]; !ok {
			return fmt.Errorf("%s: query %q references unknown database %q (have: %s)",
				path, q.Name, q.DB, strings.Join(s.dbNames(), ", "))
		}
		s.queries[q.Name] = q
		return nil
	})
	if err != nil {
		return err
	}

	if len(s.queries) == 0 {
		return fmt.Errorf("no .sql query files found in %s", sqlDir)
	}
	return nil
}

// validate prepares every query against its database so that malformed SQL
// fails at startup, and records each query's output columns for request-time
// validation of filter/order/select terms.
func (s *store) validate(ctx context.Context) error {
	for _, name := range s.queryNames() {
		q := s.queries[name]

		stmt, err := s.dbs[q.DB].PrepareContext(ctx, q.SQL)
		if err != nil {
			return fmt.Errorf("query %q: %w", name, err)
		}
		stmt.Close()

		cols, err := s.outputColumns(ctx, q)
		if err != nil {
			return fmt.Errorf("query %q: determining output columns: %w", name, err)
		}
		q.Columns = cols
	}
	return nil
}

// outputColumns runs the query with a false predicate so no rows are scanned,
// then reads the column names off the empty result set. Declared parameters are
// bound to their defaults (or NULL) purely to satisfy the binder.
func (s *store) outputColumns(ctx context.Context, q *Query) (map[string]bool, error) {
	// Reuse the request-time binder so list placeholders are expanded exactly
	// as they will be when serving.
	vals := map[string]any{}
	for _, p := range q.Params {
		if p.Required {
			// Supply a placeholder value; the probe matches no rows anyway.
			if p.Type.IsList() {
				vals[p.Name] = []any{zeroFor(p.Type.Elem())}
			} else {
				vals[p.Name] = zeroFor(p.Type)
			}
		}
	}
	stmtSQL, args, err := bindArgs(q, vals)
	if err != nil {
		return nil, err
	}

	probe := "SELECT * FROM (\n" + stripTrailingSemicolon(stmtSQL) + "\n) AS _q WHERE 0"
	rows, err := s.dbs[q.DB].QueryContext(ctx, probe, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	names, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	cols := make(map[string]bool, len(names))
	for _, n := range names {
		cols[n] = true
	}
	return cols, rows.Err()
}

// zeroFor returns a representative value of the given scalar type, used only to
// satisfy the binder when probing a query's output columns.
func zeroFor(t ParamType) any {
	switch t {
	case TypeInt:
		return int64(0)
	case TypeFloat:
		return float64(0)
	case TypeBool:
		return false
	default:
		return ""
	}
}

func (s *store) dbNames() []string {
	names := make([]string, 0, len(s.dbs))
	for k := range s.dbs {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func (s *store) queryNames() []string {
	names := make([]string, 0, len(s.queries))
	for k := range s.queries {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func (s *store) Close() error {
	var firstErr error
	for _, db := range s.dbs {
		if err := db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

