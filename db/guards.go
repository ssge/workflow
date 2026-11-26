package db

import "gorm.io/gorm"

// GORM has no trigger API. These database-level guards must also protect
// conditional and bulk writes; ORM callbacks alone cannot enforce them.
// All identifiers, events and predicates below are migration constants.
func guard(db *gorm.DB, name, table, event, predicate string) error {
	ddl := "CREATE TRIGGER " + name + " BEFORE " + event + " ON " + table + " FOR EACH ROW"
	if predicate != "" {
		ddl += " WHEN (" + predicate + ")"
	}
	if db.Dialector.Name() == "sqlite" {
		ddl += " BEGIN SELECT RAISE(ABORT, 'immutable record'); END"
	} else {
		ddl += " EXECUTE FUNCTION reject_immutable_write()"
	}
	return db.Exec(ddl).Error
}

func runtimeGuards(tx *gorm.DB) error {
	if tx.Dialector.Name() == "postgres" {
		if err := tx.Exec(`CREATE FUNCTION reject_immutable_write() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN RAISE EXCEPTION 'Immutable record in %', TG_TABLE_NAME USING ERRCODE = '23514'; END; $$`).Error; err != nil {
			return err
		}
	}
	for _, g := range [][4]string{
		{"definitions_no_update", "definitions", "UPDATE", ""},
		{"definitions_no_delete", "definitions", "DELETE", ""},
		{"operations_request_immutable", "operations", "UPDATE OF operation_id, idempotency_key, kind, target, payload", ""},
		{"operations_terminal", "operations", "UPDATE", "OLD.status <> 'PENDING'"},
		{"operations_no_delete", "operations", "DELETE", ""},
		{"instances_keep_pin", "instances", "UPDATE OF process_key, version", "NEW.process_key <> OLD.process_key OR NEW.version <> OLD.version"},
		{"transitions_no_update", "transitions", "UPDATE", ""},
		{"transitions_no_delete", "transitions", "DELETE", ""},
	} {
		if err := guard(tx, g[0], g[1], g[2], g[3]); err != nil {
			return err
		}
	}
	return nil
}

func taskGuards(tx *gorm.DB) error {
	if err := guard(tx, "task_view_immutable", "user_tasks", "UPDATE OF task_id, activation_id, form_id, components, created_at", ""); err != nil {
		return err
	}
	return guard(tx, "task_terminal", "user_tasks", "UPDATE", "OLD.status <> 'OPEN'")
}
