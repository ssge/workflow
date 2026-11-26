package db

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"

	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Open configures the same connection settings for migration and runtime use.
func Open(driver, connection string) (*gorm.DB, error) {
	if connection == "" {
		return nil, errors.New("database connection is required")
	}
	var dialect gorm.Dialector
	switch driver {
	case "sqlite":
		path, err := filepath.Abs(connection)
		if err != nil {
			return nil, err
		}
		u := url.URL{Scheme: "file", Path: path, RawQuery: "_foreign_keys=on&_recursive_triggers=on&_txlock=immediate"}
		dialect = sqlite.Open(u.String())
	case "postgres":
		dialect = postgres.Open(connection)
	default:
		return nil, fmt.Errorf("unknown database driver %q", driver)
	}
	return gorm.Open(dialect, &gorm.Config{})
}

type migration struct {
	models []any
	after  func(*gorm.DB) error
}

var migrations = []migration{
	{[]any{&SchemaMigration{}, &Definition{}, &DefinitionHead{}, &Operation{}, &Instance{}, &Transition{}, &Activation{}, &WorkItem{}, &Incident{}}, runtimeGuards},
	{[]any{&UserTask{}}, taskGuards},
	{[]any{&BoundaryTimer{}}, func(tx *gorm.DB) error {
		if err := tx.Migrator().AddColumn(&timerWorkColumn{}, "TimerActivationID"); err != nil {
			return err
		}
		return guard(tx, "timer_terminal", "boundary_timers", "UPDATE", "OLD.state <> 'ARMED'")
	}},
	{[]any{&Fork{}, &BranchActivation{}, &JoinArrival{}}, func(tx *gorm.DB) error {
		return guard(tx, "fork_terminal", "forks", "UPDATE", "OLD.state = 'CLOSED'")
	}},
}

// Migrate applies only the missing suffix. Run once per installation before runners start.
func Migrate(ctx context.Context, database *gorm.DB) error {
	tx := database.WithContext(ctx)
	if name := tx.Dialector.Name(); name != "sqlite" && name != "postgres" {
		return fmt.Errorf("unsupported migration dialect %q", name)
	}
	tables, err := tx.Migrator().GetTables()
	if err != nil {
		return err
	}
	var applied []int
	if slices.Contains(tables, "schema_migrations") {
		if err := tx.Model(&SchemaMigration{}).Order("version").Pluck("version", &applied).Error; err != nil {
			return err
		}
		for i, version := range applied {
			if version != i+1 || version > len(migrations) {
				return fmt.Errorf("incompatible schema versions: %v", applied)
			}
		}
		if len(applied) == 0 {
			return errors.New("empty schema_migrations ledger")
		}
	}
	for i, m := range migrations {
		for _, model := range m.models {
			stmt := gorm.Statement{DB: tx}
			if err := stmt.Parse(model); err != nil {
				return err
			}
			if slices.Contains(tables, stmt.Table) != (i < len(applied)) {
				return fmt.Errorf("table %s disagrees with schema ledger for migration %d", stmt.Table, i+1)
			}
		}
	}
	for i := len(applied); i < len(migrations); i++ {
		m := migrations[i]
		err := tx.Transaction(func(tx *gorm.DB) error {
			ddl := tx
			if tx.Dialector.Name() == "sqlite" {
				ddl = tx.Set("gorm:table_options", " STRICT")
			}
			for _, model := range m.models {
				if err := ddl.Migrator().CreateTable(model); err != nil {
					return err
				}
			}
			if err := m.after(tx); err != nil {
				return err
			}
			return tx.Create(&SchemaMigration{Version: int64(i + 1)}).Error
		})
		if err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	return nil
}
