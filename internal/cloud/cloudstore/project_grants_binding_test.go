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
	argument any
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
	c.driver.argument = args[0].Value
	return projectGrantBindingRows{}, nil
}

type projectGrantBindingRows struct{}

func (projectGrantBindingRows) Columns() []string { return nil }

func (projectGrantBindingRows) Close() error { return nil }

func (projectGrantBindingRows) Next([]driver.Value) error { return io.EOF }

func TestListProjectGrantsBindsNumericPrincipalID(t *testing.T) {
	projectGrantBindingDriver.argument = nil
	db, err := sql.Open(projectGrantBindingDriverName, "")
	if err != nil {
		t.Fatalf("open capture database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := &CloudStore{db: db}
	if _, err := store.ListProjectGrants(context.Background(), " 3 "); err != nil {
		t.Fatalf("list project grants: %v", err)
	}

	if _, ok := projectGrantBindingDriver.argument.(int64); !ok {
		t.Fatalf("ListProjectGrants bound principal ID as %T; want int64", projectGrantBindingDriver.argument)
	}
}
