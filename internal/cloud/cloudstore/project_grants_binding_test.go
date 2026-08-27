package cloudstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"testing"
)

const projectGrantBindingDriverName = "cloudstore-project-grant-binding"

var projectGrantBindingDriver = &capturingProjectGrantDriver{}

func init() {
	sql.Register(projectGrantBindingDriverName, projectGrantBindingDriver)
}

type capturingProjectGrantDriver struct {
	argument   any
	queryCalls int
}

func (d *capturingProjectGrantDriver) Open(string) (driver.Conn, error) {
	return projectGrantBindingConn{driver: d}, nil
}

type projectGrantBindingConn struct {
	driver *capturingProjectGrantDriver
}

func (c projectGrantBindingConn) Prepare(string) (driver.Stmt, error) {
	return nil, driver.ErrSkip
}

func (projectGrantBindingConn) Close() error { return nil }

func (projectGrantBindingConn) Begin() (driver.Tx, error) { return nil, driver.ErrSkip }

func (c projectGrantBindingConn) QueryContext(_ context.Context, _ string, args []driver.NamedValue) (driver.Rows, error) {
	c.driver.queryCalls++
	c.driver.argument = args[0].Value
	return projectGrantBindingRows{}, nil
}

type projectGrantBindingRows struct{}

func (projectGrantBindingRows) Columns() []string { return nil }

func (projectGrantBindingRows) Close() error { return nil }

func (projectGrantBindingRows) Next([]driver.Value) error { return io.EOF }

func TestListProjectGrantsBindsNumericPrincipalID(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{name: "trims padded ID", input: " 3 ", want: 3},
		{name: "accepts maximum signed bigint", input: "9223372036854775807", want: 9223372036854775807},
		{name: "accepts minimum signed bigint", input: "-9223372036854775808", want: -9223372036854775808},
		{name: "rejects non-numeric ID", input: "not-a-number", wantErr: true},
		{name: "rejects out-of-range ID", input: "9223372036854775808", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projectGrantBindingDriver.argument = nil
			projectGrantBindingDriver.queryCalls = 0
			db, err := sql.Open(projectGrantBindingDriverName, "")
			if err != nil {
				t.Fatalf("open capture database: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })

			store := &CloudStore{db: db}
			_, err = store.ListProjectGrants(context.Background(), tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ListProjectGrants returned nil error")
				}
				if projectGrantBindingDriver.queryCalls != 0 {
					t.Fatalf("QueryContext called %d times; want 0", projectGrantBindingDriver.queryCalls)
				}
				return
			}
			if err != nil {
				t.Fatalf("list project grants: %v", err)
			}
			if projectGrantBindingDriver.queryCalls != 1 {
				t.Fatalf("QueryContext called %d times; want 1", projectGrantBindingDriver.queryCalls)
			}
			bound, ok := projectGrantBindingDriver.argument.(int64)
			if !ok {
				t.Fatalf("ListProjectGrants bound principal ID as %T; want int64", projectGrantBindingDriver.argument)
			}
			if bound != tt.want {
				t.Fatalf("ListProjectGrants bound principal ID as %d; want %d", bound, tt.want)
			}
		})
	}
}
