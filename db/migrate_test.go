package db

import (
	"context"
	"path/filepath"
	"testing"

	"gorm.io/gorm"
)

func freshStore(t *testing.T) *gorm.DB {
	t.Helper()
	store, err := Open("sqlite", filepath.Join(t.TempDir(), "r1.db"))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := store.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	return store
}

func TestMigratePreservesORMValues(t *testing.T) {
	ctx := context.Background()
	store := freshStore(t)
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	d := Definition{ProcessKey: "p", Version: 1, ProfileRevision: "r1", Sources: `[]`, Plan: `{"limit":1.234567890123456789012345678901234e+6144}`}
	if err := store.Create(&d).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	var saved Definition
	if err := store.First(&saved, "process_key = ? AND version = ?", "p", 1).Error; err != nil {
		t.Fatal(err)
	}
	if saved.Plan != d.Plan || saved.CreatedAt != 0 {
		t.Fatalf("migration changed JSON precision or the supplied clock: %+v", saved)
	}
}

func TestMigrateRejectsIncompatibleLedger(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*gorm.DB) error
	}{
		{"unversioned tables", func(s *gorm.DB) error { return s.Migrator().CreateTable(&Definition{}) }},
		{"future version", func(s *gorm.DB) error { return s.Create(&SchemaMigration{Version: 5}).Error }},
		{"gap", func(s *gorm.DB) error { return s.Delete(&SchemaMigration{}, "version = ?", 2).Error }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := freshStore(t)
			ctx := context.Background()
			if tc.name != "unversioned tables" {
				if err := Migrate(ctx, store); err != nil {
					t.Fatal(err)
				}
			}
			if err := tc.setup(store); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(ctx, store); err == nil {
				t.Fatal("accepted incompatible store")
			}
		})
	}
}
