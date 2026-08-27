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
