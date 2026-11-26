package db

import (
	"database/sql/driver"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// Preserve JSON numbers as supplied, without a float64 round trip.
type JSONObject string
type JSONArray string

func (v JSONObject) Value() (driver.Value, error) { return string(v), nil }
func (v JSONArray) Value() (driver.Value, error)  { return string(v), nil }

func jsonColumn(db *gorm.DB, f *schema.Field, shape string) string {
	column := `"` + f.DBName + `"`
	if db.Dialector.Name() == "sqlite" {
		return fmt.Sprintf("TEXT CHECK (%[1]s IS NULL OR (json_valid(%[1]s) AND json_type(%[1]s) = '%[2]s'))", column, shape)
	}
	return fmt.Sprintf("json CHECK (json_typeof(%s) = '%s')", column, shape)
}

func (JSONObject) GormDBDataType(db *gorm.DB, f *schema.Field) string {
	return jsonColumn(db, f, "object")
}
func (JSONArray) GormDBDataType(db *gorm.DB, f *schema.Field) string {
	return jsonColumn(db, f, "array")
}

type CommandKey string

func (CommandKey) GormDBDataType(db *gorm.DB, _ *schema.Field) string {
	if db.Dialector.Name() == "sqlite" {
		return "TEXT CHECK (length(CAST(idempotency_key AS BLOB)) BETWEEN 1 AND 128 AND instr(idempotency_key, char(0)) = 0 AND idempotency_key NOT GLOB '*[^!-~]*')"
	}
	return `text COLLATE "C" CHECK (octet_length(idempotency_key) BETWEEN 1 AND 128 AND idempotency_key !~ '[^!-~]')`
}

// Migration 3 adds a foreign key that applies only to TIMER work.
type timerWorkColumn struct {
	TimerActivationID *timerReference `gorm:"->"`
}

func (timerWorkColumn) TableName() string { return "work_items" }

type timerReference string

func (timerReference) GormDBDataType(db *gorm.DB, _ *schema.Field) string {
	storage := "STORED"
	if db.Dialector.Name() == "sqlite" {
		storage = "VIRTUAL"
	}
	return "TEXT GENERATED ALWAYS AS (CASE WHEN kind = 'TIMER' THEN activation_id END) " + storage + " REFERENCES boundary_timers(activation_id)"
}
