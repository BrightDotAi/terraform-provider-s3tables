// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"errors"
	"fmt"
	"testing"

	iceberg "github.com/apache/iceberg-go"
	itable "github.com/apache/iceberg-go/table"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// ── unit tests ────────────────────────────────────────────────────────────────

func TestParseIcebergType(t *testing.T) {
	valid := []string{
		"boolean", "int", "long", "float", "double",
		"date", "time", "timestamp", "timestamptz",
		"string", "binary", "uuid",
		"fixed[1]", "fixed[16]",
		"decimal(10,2)", "decimal(10, 2)", "decimal(38,18)",
	}
	for _, s := range valid {
		t.Run(s, func(t *testing.T) {
			if _, err := parseIcebergType(s); err != nil {
				t.Errorf("parseIcebergType(%q) unexpected error: %v", s, err)
			}
		})
	}

	invalid := []string{
		"invalid", "INT", "Integer", "varchar",
		"fixed[0]", "fixed[-1]", "fixed[abc]",
		"decimal(0,2)", "decimal(10,-1)", "decimal(a,b)",
	}
	for _, s := range invalid {
		t.Run(s, func(t *testing.T) {
			if _, err := parseIcebergType(s); err == nil {
				t.Errorf("parseIcebergType(%q) expected error, got nil", s)
			}
		})
	}
}

func TestBuildSchema(t *testing.T) {
	fields := []FieldModel{
		{
			Name:     types.StringValue("id"),
			Type:     types.StringValue("long"),
			Required: types.BoolValue(true),
			Default:  types.StringNull(),
			Doc:      types.StringValue("primary key"),
		},
		{
			Name:     types.StringValue("value"),
			Type:     types.StringValue("string"),
			Required: types.BoolValue(false),
			Default:  types.StringNull(),
			Doc:      types.StringValue(""),
		},
	}

	s, err := BuildSchema(fields)
	if err != nil {
		t.Fatalf("BuildSchema() error: %v", err)
	}
	if s.NumFields() != 2 {
		t.Fatalf("expected 2 fields, got %d", s.NumFields())
	}

	f0 := s.Field(0)
	if f0.Name != "id" {
		t.Errorf("field[0].Name = %q, want %q", f0.Name, "id")
	}
	if !f0.Required {
		t.Error("field[0].Required = false, want true")
	}
	if f0.Doc != "primary key" {
		t.Errorf("field[0].Doc = %q, want %q", f0.Doc, "primary key")
	}

	f1 := s.Field(1)
	if f1.Required {
		t.Error("field[1].Required = true, want false (default)")
	}
	if f1.Doc != "" {
		t.Errorf("field[1].Doc = %q, want empty string (default)", f1.Doc)
	}
}

func TestBuildSchema_InvalidType(t *testing.T) {
	fields := []FieldModel{
		{
			Name:     types.StringValue("x"),
			Type:     types.StringValue("notatype"),
			Required: types.BoolValue(false),
			Default:  types.StringNull(),
			Doc:      types.StringValue(""),
		},
	}
	if _, err := BuildSchema(fields); err == nil {
		t.Error("BuildSchema() with invalid type: expected error, got nil")
	}
}

func TestFieldDefaultValues(t *testing.T) {
	fields := []FieldModel{
		{
			Name:     types.StringValue("score"),
			Type:     types.StringValue("double"),
			Required: types.BoolValue(false), // schema default
			Default:  types.StringValue("0"),
			Doc:      types.StringValue(""), // schema default
		},
	}

	s, err := BuildSchema(fields)
	if err != nil {
		t.Fatalf("BuildSchema() error: %v", err)
	}
	f := s.Field(0)
	if f.Required {
		t.Error("field should not be required (default is false)")
	}
	if f.Doc != "" {
		t.Errorf("field doc should be empty (default), got %q", f.Doc)
	}
	if f.WriteDefault == nil {
		t.Error("WriteDefault should be set from the default value")
	}
}

func TestToNestedField(t *testing.T) {
	tests := []struct {
		name        string
		fieldType   string
		defaultVal  types.String
		wantDefault any
	}{
		{"omitted_default", "long", types.StringNull(), nil},
		{"boolean", "boolean", types.StringValue("true"), true},
		{"int", "int", types.StringValue("7"), int32(7)},
		{"long", "long", types.StringValue("42"), int64(42)},
		{"float", "float", types.StringValue("2.5"), float32(2.5)},
		{"double", "double", types.StringValue("3.14"), float64(3.14)},
		{"string", "string", types.StringValue("hello"), "hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := FieldModel{
				Name:     types.StringValue("col"),
				Type:     types.StringValue(tt.fieldType),
				Required: types.BoolValue(false),
				Default:  tt.defaultVal,
				Doc:      types.StringValue(""),
			}
			nf, err := f.toNestedField(0)
			if err != nil {
				t.Fatalf("toNestedField() unexpected error: %v", err)
			}
			if nf.WriteDefault != tt.wantDefault {
				t.Errorf("WriteDefault = %v (%T), want %v (%T)", nf.WriteDefault, nf.WriteDefault, tt.wantDefault, tt.wantDefault)
			}
			if nf.InitialDefault != tt.wantDefault {
				t.Errorf("InitialDefault = %v (%T), want %v (%T)", nf.InitialDefault, nf.InitialDefault, tt.wantDefault, tt.wantDefault)
			}
		})
	}
}

func TestBuildProperties(t *testing.T) {
	props := []PropertyModel{
		{Name: types.StringValue("write.metadata.compression-codec"), Value: types.StringValue("gzip")},
		{Name: types.StringValue("write.target-file-size-bytes"), Value: types.StringValue("134217728")},
	}

	p, err := BuildProperties(props)
	if err != nil {
		t.Fatalf("BuildProperties() error: %v", err)
	}
	if len(*p) != 2 {
		t.Fatalf("expected 2 properties, got %d", len(*p))
	}
	if (*p)["write.metadata.compression-codec"] != "gzip" {
		t.Errorf("unexpected value for compression-codec: %q", (*p)["write.metadata.compression-codec"])
	}
}

func TestBuildPartitionSpec_Unpartitioned(t *testing.T) {
	s, _ := BuildSchema([]FieldModel{
		{Name: types.StringValue("ts"), Type: types.StringValue("timestamp"), Required: types.BoolValue(false), Default: types.StringNull(), Doc: types.StringValue("")},
	})
	spec, err := BuildPartitionSpec(nil, s)
	if err != nil {
		t.Fatalf("BuildPartitionSpec() error: %v", err)
	}
	if spec.NumFields() != 0 {
		t.Errorf("expected unpartitioned spec, got %d fields", spec.NumFields())
	}
}

// ── mocks for Apply* tests ────────────────────────────────────────────────────

type addColumnArgs struct {
	path     []string
	typ      iceberg.Type
	doc      string
	required bool
	defVal   iceberg.Literal
}
type updateColumnArgs struct {
	path   []string
	update itable.ColumnUpdate
}
type addFieldArgs struct {
	sourceName string
	transform  iceberg.Transform
	name       string
}

type mockSchemaUpdater struct {
	deletedCols  [][]string
	addedCols    []addColumnArgs
	updatedCols  []updateColumnArgs
	commitCalled bool
	commitErr    error
}

func (m *mockSchemaUpdater) DeleteColumn(path []string) *itable.UpdateSchema {
	m.deletedCols = append(m.deletedCols, path)
	return nil
}
func (m *mockSchemaUpdater) AddColumn(path []string, typ iceberg.Type, doc string, required bool, dv iceberg.Literal) *itable.UpdateSchema {
	m.addedCols = append(m.addedCols, addColumnArgs{path, typ, doc, required, dv})
	return nil
}
func (m *mockSchemaUpdater) UpdateColumn(path []string, u itable.ColumnUpdate) *itable.UpdateSchema {
	m.updatedCols = append(m.updatedCols, updateColumnArgs{path, u})
	return nil
}
func (m *mockSchemaUpdater) Commit() error {
	m.commitCalled = true
	return m.commitErr
}

type mockPartitionUpdater struct {
	removedFields []string
	addedFields   []addFieldArgs
	commitCalled  bool
	commitErr     error
}

func (m *mockPartitionUpdater) RemoveField(name string) *itable.UpdateSpec {
	m.removedFields = append(m.removedFields, name)
	return nil
}
func (m *mockPartitionUpdater) AddField(src string, t iceberg.Transform, name string) *itable.UpdateSpec {
	m.addedFields = append(m.addedFields, addFieldArgs{src, t, name})
	return nil
}
func (m *mockPartitionUpdater) Commit() error {
	m.commitCalled = true
	return m.commitErr
}

type mockTransaction struct {
	schema    *mockSchemaUpdater
	partition *mockPartitionUpdater
}

func (m *mockTransaction) UpdateSchema(_, _ bool) schemaUpdater    { return m.schema }
func (m *mockTransaction) UpdateSpec(_ bool) partitionUpdater       { return m.partition }

// ── Apply* unit tests ─────────────────────────────────────────────────────────

func TestApplySchemaChanges(t *testing.T) {
	f := func(name, typ string, required bool) FieldModel {
		return FieldModel{
			Name:     types.StringValue(name),
			Type:     types.StringValue(typ),
			Required: types.BoolValue(required),
			Default:  types.StringNull(),
			Doc:      types.StringValue(""),
		}
	}

	t.Run("no_changes", func(t *testing.T) {
		mock := &mockSchemaUpdater{}
		txn := &mockTransaction{schema: mock}
		if err := ApplySchemaChanges(txn, []FieldModel{f("id", "long", false)}, []FieldModel{f("id", "long", false)}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mock.commitCalled {
			t.Error("Commit should not be called when there are no changes")
		}
	})

	t.Run("add_column", func(t *testing.T) {
		mock := &mockSchemaUpdater{}
		txn := &mockTransaction{schema: mock}
		if err := ApplySchemaChanges(txn, nil, []FieldModel{f("score", "double", false)}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.addedCols) != 1 || mock.addedCols[0].path[0] != "score" {
			t.Errorf("expected AddColumn(score), got %v", mock.addedCols)
		}
		if !mock.commitCalled {
			t.Error("expected Commit to be called")
		}
	})

	t.Run("delete_column", func(t *testing.T) {
		mock := &mockSchemaUpdater{}
		txn := &mockTransaction{schema: mock}
		if err := ApplySchemaChanges(txn, []FieldModel{f("old", "string", false)}, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.deletedCols) != 1 || mock.deletedCols[0][0] != "old" {
			t.Errorf("expected DeleteColumn(old), got %v", mock.deletedCols)
		}
		if !mock.commitCalled {
			t.Error("expected Commit to be called")
		}
	})

	t.Run("update_column", func(t *testing.T) {
		state := FieldModel{Name: types.StringValue("id"), Type: types.StringValue("long"), Required: types.BoolValue(false), Default: types.StringNull(), Doc: types.StringValue("")}
		plan := FieldModel{Name: types.StringValue("id"), Type: types.StringValue("long"), Required: types.BoolValue(true), Default: types.StringNull(), Doc: types.StringValue("pk")}
		mock := &mockSchemaUpdater{}
		txn := &mockTransaction{schema: mock}
		if err := ApplySchemaChanges(txn, []FieldModel{state}, []FieldModel{plan}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.updatedCols) != 1 || mock.updatedCols[0].path[0] != "id" {
			t.Errorf("expected UpdateColumn(id), got %v", mock.updatedCols)
		}
		if len(mock.addedCols) != 0 {
			t.Errorf("expected no AddColumn, got %v", mock.addedCols)
		}
		if !mock.commitCalled {
			t.Error("expected Commit to be called")
		}
	})

	t.Run("commit_error", func(t *testing.T) {
		mock := &mockSchemaUpdater{commitErr: errors.New("boom")}
		txn := &mockTransaction{schema: mock}
		err := ApplySchemaChanges(txn, nil, []FieldModel{f("x", "int", false)})
		if err == nil || err.Error() != "boom" {
			t.Errorf("expected commit error, got %v", err)
		}
	})

	t.Run("invalid_type", func(t *testing.T) {
		mock := &mockSchemaUpdater{}
		txn := &mockTransaction{schema: mock}
		err := ApplySchemaChanges(txn, nil, []FieldModel{f("x", "notatype", false)})
		if err == nil {
			t.Error("expected error for invalid type, got nil")
		}
		if mock.commitCalled {
			t.Error("Commit must not be called when type parsing fails")
		}
	})
}

func TestApplyPartitionChanges(t *testing.T) {
	p := func(src, transform, name string) PartitionModel {
		return PartitionModel{
			SourceName: types.StringValue(src),
			Transform:  types.StringValue(transform),
			Name:       types.StringValue(name),
		}
	}

	t.Run("no_changes", func(t *testing.T) {
		mock := &mockPartitionUpdater{}
		txn := &mockTransaction{partition: mock}
		pm := []PartitionModel{p("ts", "identity", "ts_part")}
		if err := ApplyPartitionChanges(txn, pm, pm); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mock.commitCalled {
			t.Error("Commit should not be called when there are no changes")
		}
	})

	t.Run("add_field", func(t *testing.T) {
		mock := &mockPartitionUpdater{}
		txn := &mockTransaction{partition: mock}
		if err := ApplyPartitionChanges(txn, nil, []PartitionModel{p("ts", "identity", "ts_part")}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.addedFields) != 1 || mock.addedFields[0].name != "ts_part" || mock.addedFields[0].sourceName != "ts" {
			t.Errorf("expected AddField(ts, identity, ts_part), got %v", mock.addedFields)
		}
		if !mock.commitCalled {
			t.Error("expected Commit to be called")
		}
	})

	t.Run("remove_field", func(t *testing.T) {
		mock := &mockPartitionUpdater{}
		txn := &mockTransaction{partition: mock}
		if err := ApplyPartitionChanges(txn, []PartitionModel{p("ts", "identity", "ts_part")}, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.removedFields) != 1 || mock.removedFields[0] != "ts_part" {
			t.Errorf("expected RemoveField(ts_part), got %v", mock.removedFields)
		}
		if !mock.commitCalled {
			t.Error("expected Commit to be called")
		}
	})

	t.Run("update_field", func(t *testing.T) {
		mock := &mockPartitionUpdater{}
		txn := &mockTransaction{partition: mock}
		err := ApplyPartitionChanges(txn,
			[]PartitionModel{p("ts", "year", "ts_year")},
			[]PartitionModel{p("ts", "month", "ts_year")},
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.removedFields) != 1 || mock.removedFields[0] != "ts_year" {
			t.Errorf("expected RemoveField(ts_year), got %v", mock.removedFields)
		}
		if len(mock.addedFields) != 1 || mock.addedFields[0].name != "ts_year" {
			t.Errorf("expected AddField(..., ts_year), got %v", mock.addedFields)
		}
		if !mock.commitCalled {
			t.Error("expected Commit to be called")
		}
	})

	t.Run("commit_error", func(t *testing.T) {
		mock := &mockPartitionUpdater{commitErr: errors.New("boom")}
		txn := &mockTransaction{partition: mock}
		err := ApplyPartitionChanges(txn, nil, []PartitionModel{p("ts", "identity", "p")})
		if err == nil || err.Error() != "boom" {
			t.Errorf("expected commit error, got %v", err)
		}
	})

	t.Run("invalid_transform", func(t *testing.T) {
		mock := &mockPartitionUpdater{}
		txn := &mockTransaction{partition: mock}
		err := ApplyPartitionChanges(txn, nil, []PartitionModel{p("ts", "notreal", "p")})
		if err == nil {
			t.Error("expected error for invalid transform, got nil")
		}
		if mock.commitCalled {
			t.Error("Commit must not be called when transform parsing fails")
		}
	})
}

func TestCheckPropChanges(t *testing.T) {
	pm := func(name, value string) PropertyModel {
		return PropertyModel{Name: types.StringValue(name), Value: types.StringValue(value)}
	}

	tests := []struct {
		name    string
		state   []PropertyModel
		plan    []PropertyModel
		wantErr bool
	}{
		{"equal", []PropertyModel{pm("k", "v")}, []PropertyModel{pm("k", "v")}, false},
		{"empty_both", nil, nil, false},
		{"plan_has_extra", nil, []PropertyModel{pm("k", "v")}, true},
		{"state_has_extra", []PropertyModel{pm("k", "v")}, nil, true},
		{"value_changed", []PropertyModel{pm("k", "1")}, []PropertyModel{pm("k", "2")}, true},
		{"key_differs", []PropertyModel{pm("a", "v")}, []PropertyModel{pm("b", "v")}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkPropChanges(tt.state, tt.plan)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkPropChanges() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// ── acceptance tests ──────────────────────────────────────────────────────────

func TestAccS3TableResource(t *testing.T) {
	warehouse := "123456789012:s3tablescatalog/test-bucket"
	region := "us-east-1"
	namespace := "test_namespace"
	name := "test_table"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Create with fields, field defaults, and properties.
			{
				Config: testAccS3TableResourceConfig(warehouse, region, namespace, name),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("brightai_s3_table.test", "warehouse", warehouse),
					resource.TestCheckResourceAttr("brightai_s3_table.test", "region", region),
					resource.TestCheckResourceAttr("brightai_s3_table.test", "namespace", namespace),
					resource.TestCheckResourceAttr("brightai_s3_table.test", "name", name),

					// field values
					resource.TestCheckResourceAttr("brightai_s3_table.test", "field.0.name", "id"),
					resource.TestCheckResourceAttr("brightai_s3_table.test", "field.0.type", "long"),
					resource.TestCheckResourceAttr("brightai_s3_table.test", "field.0.required", "true"),
					resource.TestCheckResourceAttr("brightai_s3_table.test", "field.1.name", "event_time"),
					resource.TestCheckResourceAttr("brightai_s3_table.test", "field.1.type", "timestamp"),

					// field defaults: required=false, doc="" when not specified
					resource.TestCheckResourceAttr("brightai_s3_table.test", "field.1.required", "false"),
					resource.TestCheckResourceAttr("brightai_s3_table.test", "field.1.doc", ""),

					// field with explicit default value
					resource.TestCheckResourceAttr("brightai_s3_table.test", "field.2.name", "score"),
					resource.TestCheckResourceAttr("brightai_s3_table.test", "field.2.type", "double"),

					// properties
					resource.TestCheckResourceAttr("brightai_s3_table.test", "property.0.name", "write.metadata.compression-codec"),
					resource.TestCheckResourceAttr("brightai_s3_table.test", "property.0.value", "gzip"),
				),
			},
			// Import
			{
				ResourceName:      "brightai_s3_table.test",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateId:     fmt.Sprintf("%s,%s,%s,%s", warehouse, region, namespace, name),
			},
		},
	})
}

func testAccS3TableResourceConfig(warehouse, region, namespace, name string) string {
	return fmt.Sprintf(`
resource "brightai_s3_table" "test" {
  warehouse = %[1]q
  region    = %[2]q
  namespace = %[3]q
  name      = %[4]q

  field {
    name     = "id"
    type     = "long"
    required = true
    doc      = "primary key"
  }

  field {
    name = "event_time"
    type = "timestamp"
    # required and doc intentionally omitted to exercise defaults
  }

  field {
    name    = "score"
    type    = "double"
    default = "0.0"
  }

  property {
    name  = "write.metadata.compression-codec"
    value = "gzip"
  }
}
`, warehouse, region, namespace, name)
}
