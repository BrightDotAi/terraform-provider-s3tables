// Copyright BrightAI 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	iceberg "github.com/apache/iceberg-go"
	itable "github.com/apache/iceberg-go/table"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lakeformation"
	lftypes "github.com/aws/aws-sdk-go-v2/service/lakeformation/types"
	"github.com/aws/aws-sdk-go-v2/service/s3tables"
	"github.com/aws/aws-sdk-go-v2/service/sts"
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
			Name:          types.StringValue("id"),
			Type:          types.StringValue("long"),
			Required:      types.BoolValue(true),
			DefaultString: types.StringNull(),
			DefaultNumber: types.NumberNull(),
			DefaultBool:   types.BoolNull(),
			Doc:           types.StringValue("primary key"),
		},
		{
			Name:          types.StringValue("value"),
			Type:          types.StringValue("string"),
			Required:      types.BoolValue(false),
			DefaultString: types.StringNull(),
			DefaultNumber: types.NumberNull(),
			DefaultBool:   types.BoolNull(),
			Doc:           types.StringValue(""),
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
			Name:          types.StringValue("x"),
			Type:          types.StringValue("notatype"),
			Required:      types.BoolValue(false),
			DefaultString: types.StringNull(),
			DefaultNumber: types.NumberNull(),
			DefaultBool:   types.BoolNull(),
			Doc:           types.StringValue(""),
		},
	}
	if _, err := BuildSchema(fields); err == nil {
		t.Error("BuildSchema() with invalid type: expected error, got nil")
	}
}

func TestFieldDefaultValues(t *testing.T) {
	fields := []FieldModel{
		{
			Name:          types.StringValue("score"),
			Type:          types.StringValue("double"),
			Required:      types.BoolValue(false),
			DefaultString: types.StringNull(),
			DefaultNumber: types.NumberValue(big.NewFloat(0.0)),
			DefaultBool:   types.BoolNull(),
			Doc:           types.StringValue(""),
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
	noDefault := func(typ string) FieldModel {
		return FieldModel{
			Name:          types.StringValue("col"),
			Type:          types.StringValue(typ),
			Required:      types.BoolValue(false),
			DefaultString: types.StringNull(),
			DefaultNumber: types.NumberNull(),
			DefaultBool:   types.BoolNull(),
			Doc:           types.StringValue(""),
		}
	}

	tests := []struct {
		name        string
		field       FieldModel
		wantDefault any
		wantErr     bool
	}{
		{name: "omitted_default", field: noDefault("long"), wantDefault: nil},
		{
			name: "boolean",
			field: FieldModel{
				Name:          types.StringValue("col"),
				Type:          types.StringValue("boolean"),
				Required:      types.BoolValue(false),
				DefaultString: types.StringNull(),
				DefaultNumber: types.NumberNull(),
				DefaultBool:   types.BoolValue(true),
				Doc:           types.StringValue(""),
			},
			wantDefault: true,
		},
		{
			name: "int",
			field: FieldModel{
				Name:          types.StringValue("col"),
				Type:          types.StringValue("int"),
				Required:      types.BoolValue(false),
				DefaultString: types.StringNull(),
				DefaultNumber: types.NumberValue(big.NewFloat(7)),
				DefaultBool:   types.BoolNull(),
				Doc:           types.StringValue(""),
			},
			wantDefault: int32(7),
		},
		{
			name: "long",
			field: FieldModel{
				Name:          types.StringValue("col"),
				Type:          types.StringValue("long"),
				Required:      types.BoolValue(false),
				DefaultString: types.StringNull(),
				DefaultNumber: types.NumberValue(big.NewFloat(42)),
				DefaultBool:   types.BoolNull(),
				Doc:           types.StringValue(""),
			},
			wantDefault: int64(42),
		},
		{
			name: "float",
			field: FieldModel{
				Name:          types.StringValue("col"),
				Type:          types.StringValue("float"),
				Required:      types.BoolValue(false),
				DefaultString: types.StringNull(),
				DefaultNumber: types.NumberValue(big.NewFloat(2.5)),
				DefaultBool:   types.BoolNull(),
				Doc:           types.StringValue(""),
			},
			wantDefault: float32(2.5),
		},
		{
			name: "double",
			field: FieldModel{
				Name:          types.StringValue("col"),
				Type:          types.StringValue("double"),
				Required:      types.BoolValue(false),
				DefaultString: types.StringNull(),
				DefaultNumber: types.NumberValue(big.NewFloat(3.14)),
				DefaultBool:   types.BoolNull(),
				Doc:           types.StringValue(""),
			},
			wantDefault: float64(3.14),
		},
		{
			name: "string",
			field: FieldModel{
				Name:          types.StringValue("col"),
				Type:          types.StringValue("string"),
				Required:      types.BoolValue(false),
				DefaultString: types.StringValue("hello"),
				DefaultNumber: types.NumberNull(),
				DefaultBool:   types.BoolNull(),
				Doc:           types.StringValue(""),
			},
			wantDefault: "hello",
		},
		// Negative: number and bool defaults both set → error
		{
			name: "multiple_defaults_error",
			field: FieldModel{
				Name:          types.StringValue("col"),
				Type:          types.StringValue("long"),
				Required:      types.BoolValue(false),
				DefaultString: types.StringNull(),
				DefaultNumber: types.NumberValue(big.NewFloat(1)),
				DefaultBool:   types.BoolValue(true),
				Doc:           types.StringValue(""),
			},
			wantErr: true,
		},
		// Negative: bool default on a long field → error
		{
			name: "wrong_default_type_bool_on_long",
			field: FieldModel{
				Name:          types.StringValue("col"),
				Type:          types.StringValue("long"),
				Required:      types.BoolValue(false),
				DefaultString: types.StringNull(),
				DefaultNumber: types.NumberNull(),
				DefaultBool:   types.BoolValue(true),
				Doc:           types.StringValue(""),
			},
			wantErr: true,
		},
		// Negative: number default on a boolean field → error
		{
			name: "wrong_default_type_number_on_boolean",
			field: FieldModel{
				Name:          types.StringValue("col"),
				Type:          types.StringValue("boolean"),
				Required:      types.BoolValue(false),
				DefaultString: types.StringNull(),
				DefaultNumber: types.NumberValue(big.NewFloat(1)),
				DefaultBool:   types.BoolNull(),
				Doc:           types.StringValue(""),
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nf, err := tt.field.toNestedField(0)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
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
	t.Run("user_props_plus_defaults", func(t *testing.T) {
		props := []PropertyModel{
			{Name: types.StringValue("write.metadata.compression-codec"), Value: types.StringValue("gzip")},
			{Name: types.StringValue("write.target-file-size-bytes"), Value: types.StringValue("134217728")},
		}
		p, err := BuildProperties(props, "2")
		if err != nil {
			t.Fatalf("BuildProperties() error: %v", err)
		}
		wantLen := 2 + len(prop_defaults)
		if len(*p) != wantLen {
			t.Fatalf("expected %d properties (user + defaults), got %d", wantLen, len(*p))
		}
		if (*p)["write.metadata.compression-codec"] != "gzip" {
			t.Errorf("compression-codec = %q, want %q", (*p)["write.metadata.compression-codec"], "gzip")
		}
		for name, val := range prop_defaults {
			if (*p)[name] != val {
				t.Errorf("default property %q = %q, want %q", name, (*p)[name], val)
			}
		}
	})

	t.Run("user_overrides_default", func(t *testing.T) {
		props := []PropertyModel{
			{Name: types.StringValue("write_compression"), Value: types.StringValue("snappy")},
		}
		p, err := BuildProperties(props, "2")
		if err != nil {
			t.Fatalf("BuildProperties() error: %v", err)
		}
		if (*p)["write_compression"] != "snappy" {
			t.Errorf("write_compression = %q, want snappy (user value should override default)", (*p)["write_compression"])
		}
	})

	t.Run("empty_input_gets_defaults", func(t *testing.T) {
		p, err := BuildProperties(nil, "2")
		if err != nil {
			t.Fatalf("BuildProperties() error: %v", err)
		}
		if len(*p) != len(prop_defaults) {
			t.Fatalf("expected %d default properties, got %d", len(prop_defaults), len(*p))
		}
		for name, val := range prop_defaults {
			if (*p)[name] != val {
				t.Errorf("default property %q = %q, want %q", name, (*p)[name], val)
			}
		}
	})

	t.Run("format_version_2_no_property", func(t *testing.T) {
		p, err := BuildProperties(nil, "2")
		if err != nil {
			t.Fatalf("BuildProperties() error: %v", err)
		}
		if _, ok := (*p)["format-version"]; ok {
			t.Error("format-version property must not be set for version 2 (it is the default)")
		}
	})

	t.Run("format_version_3_sets_property", func(t *testing.T) {
		p, err := BuildProperties(nil, "3")
		if err != nil {
			t.Fatalf("BuildProperties() error: %v", err)
		}
		if (*p)["format-version"] != "3" {
			t.Errorf("format-version = %q, want %q", (*p)["format-version"], "3")
		}
	})

	t.Run("invalid_format_version_error", func(t *testing.T) {
		_, err := BuildProperties(nil, "1")
		if err == nil {
			t.Error("expected error for unsupported format version 1, got nil")
		}
	})

	t.Run("empty_format_version_error", func(t *testing.T) {
		_, err := BuildProperties(nil, "")
		if err == nil {
			t.Error("expected error for empty format version, got nil")
		}
	})
}

func TestPropertiesToPropertyModels(t *testing.T) {
	t.Run("default_props_filtered_out", func(t *testing.T) {
		props := iceberg.Properties{
			"table_type":       "iceberg",
			"write_compression": "zstd",
		}
		models := propertiesToPropertyModels(props)
		if len(models) != 0 {
			t.Errorf("expected default-only properties to produce 0 models, got %d: %v", len(models), models)
		}
	})

	t.Run("non_default_props_included", func(t *testing.T) {
		props := iceberg.Properties{
			"table_type":                      "iceberg",
			"write_compression":               "zstd",
			"write.metadata.compression-codec": "gzip",
		}
		models := propertiesToPropertyModels(props)
		if len(models) != 1 {
			t.Fatalf("expected 1 model (non-default prop), got %d", len(models))
		}
		if models[0].Name.ValueString() != "write.metadata.compression-codec" {
			t.Errorf("model name = %q, want %q", models[0].Name.ValueString(), "write.metadata.compression-codec")
		}
		if models[0].Value.ValueString() != "gzip" {
			t.Errorf("model value = %q, want %q", models[0].Value.ValueString(), "gzip")
		}
	})

	t.Run("overridden_default_included", func(t *testing.T) {
		props := iceberg.Properties{
			"table_type":       "iceberg",
			"write_compression": "snappy",
		}
		models := propertiesToPropertyModels(props)
		if len(models) != 1 {
			t.Fatalf("expected 1 model (overridden default), got %d", len(models))
		}
		if models[0].Name.ValueString() != "write_compression" || models[0].Value.ValueString() != "snappy" {
			t.Errorf("unexpected model: %+v", models[0])
		}
	})
}

func TestBuildPartitionSpec_Unpartitioned(t *testing.T) {
	s, _ := BuildSchema([]FieldModel{
		{Name: types.StringValue("ts"), Type: types.StringValue("timestamp"), Required: types.BoolValue(false), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(), Doc: types.StringValue("")},
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
			Name:          types.StringValue(name),
			Type:          types.StringValue(typ),
			Required:      types.BoolValue(required),
			DefaultString: types.StringNull(),
			DefaultNumber: types.NumberNull(),
			DefaultBool:   types.BoolNull(),
			Doc:           types.StringValue(""),
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
		state := FieldModel{Name: types.StringValue("id"), Type: types.StringValue("long"), Required: types.BoolValue(false), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(), Doc: types.StringValue("")}
		plan := FieldModel{Name: types.StringValue("id"), Type: types.StringValue("long"), Required: types.BoolValue(true), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(), Doc: types.StringValue("pk")}
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

// testAccSetup loads AWS config, creates a temporary S3Tables bucket and
// namespace in us-west-2, grants the necessary Lake Formation permissions on
// both for the current caller, registers cleanup (revoke LF permissions, then
// delete namespace and bucket), and returns the warehouse identifier, region,
// and namespace name.
func testAccSetup(t *testing.T) (warehouse, region, namespace string) {
	t.Helper()
	region = "us-west-2"
	namespace = "test_namespace"
	ctx := context.Background()

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		t.Fatalf("failed to load AWS config: %v", err)
	}

	stsClient := sts.NewFromConfig(cfg)
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		t.Fatalf("failed to get caller identity: %v", err)
	}
	accountID := aws.ToString(identity.Account)
	roleARN := iamRoleARN(aws.ToString(identity.Arn))

	bucketName := fmt.Sprintf("tf-acc-%d", time.Now().UnixNano()%10000000000)
	warehouse = fmt.Sprintf("%s:s3tablescatalog/%s", accountID, bucketName)

	s3t := s3tables.NewFromConfig(cfg)
	bucketOut, err := s3t.CreateTableBucket(ctx, &s3tables.CreateTableBucketInput{
		Name: aws.String(bucketName),
	})
	if err != nil {
		t.Fatalf("failed to create S3Tables bucket %q: %v", bucketName, err)
	}
	bucketARN := aws.ToString(bucketOut.Arn)

	_, err = s3t.CreateNamespace(ctx, &s3tables.CreateNamespaceInput{
		TableBucketARN: aws.String(bucketARN),
		Namespace:      []string{namespace},
	})
	if err != nil {
		_, _ = s3t.DeleteTableBucket(ctx, &s3tables.DeleteTableBucketInput{TableBucketARN: aws.String(bucketARN)})
		t.Fatalf("failed to create namespace %q: %v", namespace, err)
	}

	lfClient := lakeformation.NewFromConfig(cfg)
	principal := &lftypes.DataLakePrincipal{DataLakePrincipalIdentifier: aws.String(roleARN)}
	dataLocationResource := &lftypes.Resource{
		DataLocation: &lftypes.DataLocationResource{
			CatalogId:   aws.String(warehouse),
			ResourceArn: aws.String(bucketARN),
		},
	}

	// Grant DATA_LOCATION_ACCESS on the bucket. The account-level wildcard
	// registration covers all S3 Tables buckets as managed data locations; a
	// per-bucket grant within that wildcard gives the role access to all tables
	// in this bucket without naming individual namespaces or tables.
	if _, err := lfClient.GrantPermissions(ctx, &lakeformation.GrantPermissionsInput{
		Principal:   principal,
		Resource:    dataLocationResource,
		Permissions: []lftypes.Permission{lftypes.PermissionDataLocationAccess},
	}); err != nil {
		_, _ = s3t.DeleteNamespace(ctx, &s3tables.DeleteNamespaceInput{TableBucketARN: aws.String(bucketARN), Namespace: aws.String(namespace)})
		_, _ = s3t.DeleteTableBucket(ctx, &s3tables.DeleteTableBucketInput{TableBucketARN: aws.String(bucketARN)})
		t.Fatalf("failed to grant LF data location access on bucket %q: %v", bucketARN, err)
	}

	t.Cleanup(func() {
		if _, err := lfClient.RevokePermissions(ctx, &lakeformation.RevokePermissionsInput{
			Principal:   principal,
			Resource:    dataLocationResource,
			Permissions: []lftypes.Permission{lftypes.PermissionDataLocationAccess},
		}); err != nil {
			t.Logf("cleanup: failed to revoke LF data location access on bucket %q: %v", bucketARN, err)
		}
		if _, err := s3t.DeleteNamespace(ctx, &s3tables.DeleteNamespaceInput{
			TableBucketARN: aws.String(bucketARN),
			Namespace:      aws.String(namespace),
		}); err != nil {
			t.Logf("cleanup: failed to delete namespace %q: %v", namespace, err)
		}
		if _, err := s3t.DeleteTableBucket(ctx, &s3tables.DeleteTableBucketInput{
			TableBucketARN: aws.String(bucketARN),
		}); err != nil {
			t.Logf("cleanup: failed to delete bucket %q: %v", bucketARN, err)
		}
	})

	return warehouse, region, namespace
}

// iamRoleARN converts an STS assumed-role ARN to the underlying IAM role ARN.
// arn:aws:sts::ACCOUNT:assumed-role/ROLE/SESSION → arn:aws:iam::ACCOUNT:role/ROLE
// Any other ARN format (IAM user, etc.) is returned unchanged.
func iamRoleARN(callerARN string) string {
	const marker = ":assumed-role/"
	idx := strings.Index(callerARN, marker)
	if idx < 0 {
		return callerARN
	}
	prefix := strings.Replace(callerARN[:idx], ":sts:", ":iam:", 1)
	roleName := strings.SplitN(callerARN[idx+len(marker):], "/", 2)[0]
	return prefix + ":role/" + roleName
}


func TestAccS3TableResource(t *testing.T) {
	warehouse, region, namespace := testAccSetup(t)
	name := "test_table"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Create with default format version (2), fields, field defaults, and properties.
			{
				Config: testAccS3TableResourceConfig(warehouse, region, namespace, name, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("bai_s3tables_table.test", "warehouse", warehouse),
					resource.TestCheckResourceAttr("bai_s3tables_table.test", "region", region),
					resource.TestCheckResourceAttr("bai_s3tables_table.test", "namespace", namespace),
					resource.TestCheckResourceAttr("bai_s3tables_table.test", "name", name),

					// default format version
					resource.TestCheckResourceAttr("bai_s3tables_table.test", "format_version", "2"),

					// field values
					resource.TestCheckResourceAttr("bai_s3tables_table.test", "field.0.name", "id"),
					resource.TestCheckResourceAttr("bai_s3tables_table.test", "field.0.type", "long"),
					resource.TestCheckResourceAttr("bai_s3tables_table.test", "field.0.required", "true"),
					resource.TestCheckResourceAttr("bai_s3tables_table.test", "field.1.name", "event_time"),
					resource.TestCheckResourceAttr("bai_s3tables_table.test", "field.1.type", "timestamp"),

					// field defaults: required=false, doc="" when not specified
					resource.TestCheckResourceAttr("bai_s3tables_table.test", "field.1.required", "false"),
					resource.TestCheckResourceAttr("bai_s3tables_table.test", "field.1.doc", ""),

					// field with explicit default value
					resource.TestCheckResourceAttr("bai_s3tables_table.test", "field.2.name", "score"),
					resource.TestCheckResourceAttr("bai_s3tables_table.test", "field.2.type", "double"),

					// properties
					resource.TestCheckResourceAttr("bai_s3tables_table.test", "property.0.name", "write.metadata.compression-codec"),
					resource.TestCheckResourceAttr("bai_s3tables_table.test", "property.0.value", "gzip"),
				),
			},
			// Import
			{
				ResourceName:      "bai_s3tables_table.test",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateId:     fmt.Sprintf("%s,%s,%s,%s", warehouse, region, namespace, name),
			},
		},
	})
}

func TestAccS3TableResource_FormatVersion3(t *testing.T) {
	warehouse, region, namespace := testAccSetup(t)
	name := "test_table_v3"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccS3TableResourceConfig(warehouse, region, namespace, name, `format_version = "3"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("bai_s3tables_table.test", "format_version", "3"),
				),
			},
		},
	})
}

func testAccS3TableResourceConfig(warehouse, region, namespace, name, extraArgs string) string {
	return fmt.Sprintf(`
resource "bai_s3tables_table" "test" {
  warehouse = %[1]q
  region    = %[2]q
  namespace = %[3]q
  name      = %[4]q
  %[5]s

  field {
    name     = "id"
    type     = "long"
    required = true
    doc      = "primary key"
  }

  field {
    name = "event_time"
    type = "timestamp"
  }

  field {
    name           = "score"
    type           = "double"
    default_number = 0.0
  }

  property {
    name  = "write.metadata.compression-codec"
    value = "gzip"
  }
}
`, warehouse, region, namespace, name, extraArgs)
}
