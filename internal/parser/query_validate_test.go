package parser

import "testing"

func TestValidateInsertColumns_NestedFunctionArguments(t *testing.T) {
	parser := &QueryParser{}
	table := &Table{
		Name: "deployments",
		Columns: []*Column{
			{Name: "title"},
			{Name: "last_state_at"},
		},
	}
	err := parser.validateInsertColumns(
		"INSERT INTO deployments (title, last_state_at) VALUES (?, strftime('%s', 'now'))",
		table,
	)
	if err != nil {
		t.Fatalf("valid INSERT with nested function arguments was rejected: %v", err)
	}
}

func TestValidateInsertColumns_SQLiteConflictClause(t *testing.T) {
	parser := &QueryParser{}
	table := &Table{Name: "project_tags", Columns: []*Column{{Name: "project_id"}, {Name: "tag_id"}}}
	if err := parser.validateInsertColumns(
		"INSERT OR IGNORE INTO project_tags (project_id, tag_id) VALUES (?, ?)",
		table,
	); err != nil {
		t.Fatalf("valid SQLite INSERT OR IGNORE was rejected: %v", err)
	}
}
