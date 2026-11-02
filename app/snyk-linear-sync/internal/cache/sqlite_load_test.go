package cache

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"

	sq "github.com/Masterminds/squirrel"
)

// errStubIteration is the sentinel error the stub driver returns from Rows.Next
// when a failure is armed for the table under test.
var errStubIteration = errors.New("stub iteration failure")

// stubRows implements driver.Rows for one of Load's queries. When failure is
// armed it yields its good rows and then returns errStubIteration from Next
// instead of io.EOF, mimicking a mid-iteration read failure.
type stubRows struct {
	columns []string
	rows    [][]driver.Value
	failing bool
	next    int
}

func (r *stubRows) Columns() []string { return r.columns }

func (r *stubRows) Close() error { return nil }

func (r *stubRows) Next(dest []driver.Value) error {
	if r.next >= len(r.rows) {
		if r.failing {
			return errStubIteration
		}
		return io.EOF
	}
	copy(dest, r.rows[r.next])
	r.next++
	return nil
}

// stubConn implements driver.Conn plus driver.QueryerContext, so database/sql
// routes every query straight to QueryContext without preparing statements.
type stubConn struct {
	// sentinel-armed marks which queries fail mid-iteration.
	armed map[string]bool
}

func (c *stubConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("stub driver: Prepare not supported")
}

func (c *stubConn) Close() error { return nil }

func (c *stubConn) Begin() (driver.Tx, error) {
	return nil, errors.New("stub driver: Begin not supported")
}

func (c *stubConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "sync_meta"):
		return &stubRows{
			columns: []string{"value"},
			rows:    [][]driver.Value{{"sig-v1"}},
			failing: c.armed["sync_meta"],
		}, nil
	case strings.Contains(query, "snyk_findings"):
		return &stubRows{
			columns: []string{"fingerprint", "hash"},
			rows: [][]driver.Value{
				{"fp-1", "hash-a"},
				{"fp-2", "hash-b"},
			},
			failing: c.armed["snyk_findings"],
		}, nil
	case strings.Contains(query, "linear_issues"):
		return &stubRows{
			columns: []string{"fingerprint", "hash"},
			rows: [][]driver.Value{
				{"fp-1", "hash-a"},
				{"fp-2", "hash-b"},
			},
			failing: c.armed["linear_issues"],
		}, nil
	}
	return nil, errors.New("stub driver: unexpected query: " + query)
}

type stubConnector struct {
	conn *stubConn
}

func (c *stubConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }

func (c *stubConnector) Driver() driver.Driver { return stubDriver{} }

type stubDriver struct{}

func (stubDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("stub driver: Open not supported")
}

// newStubStore builds a Store on the stub driver, bypassing Open so no real
// SQLite file is created. Store and Load never need Prepare or Begin here.
func newStubStore(armed map[string]bool) *Store {
	connector := &stubConnector{conn: &stubConn{armed: armed}}
	return &Store{
		db:      sql.OpenDB(connector),
		builder: sq.StatementBuilder.PlaceholderFormat(sq.Question),
	}
}

func TestLoadReportsMidIterationFailure(t *testing.T) {
	cases := []struct {
		name      string
		table     string
		tableName string
	}{
		{name: "schema signature", table: "sync_meta", tableName: "schema signature"},
		{name: "Snyk cache rows", table: "snyk_findings", tableName: "Snyk cache"},
		{name: "Linear cache rows", table: "linear_issues", tableName: "Linear cache"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newStubStore(map[string]bool{tc.table: true})
			defer store.Close()

			snapshot, err := store.Load(context.Background())
			if err == nil {
				t.Fatalf("Load() succeeded on a failing %s iteration, got snapshot %+v", tc.table, snapshot)
			}
			if !errors.Is(err, errStubIteration) {
				t.Fatalf("Load() error = %v, want it to wrap errStubIteration", err)
			}
			if !strings.Contains(err.Error(), tc.tableName) {
				t.Fatalf("Load() error = %v, want it to name %q", err, tc.tableName)
			}
			if snapshot.SchemaSignature != "" || len(snapshot.SnykHashes) != 0 || len(snapshot.LinearHashes) != 0 {
				t.Fatalf("Load() returned a partially loaded snapshot on error, got %+v", snapshot)
			}
		})
	}
}

func TestLoadHealthyStubReturnsAllRows(t *testing.T) {
	store := newStubStore(nil)
	defer store.Close()

	snapshot, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if snapshot.SchemaSignature != "sig-v1" {
		t.Fatalf("SchemaSignature = %q, want %q", snapshot.SchemaSignature, "sig-v1")
	}
	wantHashes := map[string]string{"fp-1": "hash-a", "fp-2": "hash-b"}
	for name, got := range map[string]map[string]string{
		"SnykHashes":   snapshot.SnykHashes,
		"LinearHashes": snapshot.LinearHashes,
	} {
		if len(got) != len(wantHashes) {
			t.Fatalf("%s = %v, want %v", name, got, wantHashes)
		}
		for fp, hash := range wantHashes {
			if got[fp] != hash {
				t.Fatalf("%s[%q] = %q, want %q", name, fp, got[fp], hash)
			}
		}
	}
}
