// Copyright BrightAI 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"math/big"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	iceberg "github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/catalog"
	itable "github.com/apache/iceberg-go/table"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	fwpath "github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
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
			nf, err := tt.field.toNestedField(1)
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
			"table_type":        "iceberg",
			"write_compression": "zstd",
		}
		models := propertiesToPropertyModels(props, nil)
		if len(models) != 0 {
			t.Errorf("expected default-only properties to produce 0 models, got %d: %v", len(models), models)
		}
	})

	t.Run("non_default_props_included", func(t *testing.T) {
		props := iceberg.Properties{
			"table_type":                       "iceberg",
			"write_compression":                "zstd",
			"write.metadata.compression-codec": "gzip",
		}
		models := propertiesToPropertyModels(props, nil)
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
			"table_type":        "iceberg",
			"write_compression": "snappy",
		}
		models := propertiesToPropertyModels(props, nil)
		if len(models) != 1 {
			t.Fatalf("expected 1 model (overridden default), got %d", len(models))
		}
		if models[0].Name.ValueString() != "write_compression" || models[0].Value.ValueString() != "snappy" {
			t.Errorf("unexpected model: %+v", models[0])
		}
	})

	t.Run("system_managed_props_filtered_out", func(t *testing.T) {
		props := iceberg.Properties{
			"schema.name-mapping.default":      `[{"field-id":1,"names":["col"]}]`,
			"write.metadata.compression-codec": "gzip",
		}
		models := propertiesToPropertyModels(props, nil)
		if len(models) != 1 {
			t.Fatalf("expected 1 model (non-system prop), got %d: %v", len(models), models)
		}
		if models[0].Name.ValueString() != "write.metadata.compression-codec" {
			t.Errorf("unexpected model name: %q", models[0].Name.ValueString())
		}
	})

	t.Run("system_managed_only_produces_no_models", func(t *testing.T) {
		props := iceberg.Properties{
			"schema.name-mapping.default": `[{"field-id":1,"names":["col"]}]`,
		}
		models := propertiesToPropertyModels(props, nil)
		if len(models) != 0 {
			t.Errorf("expected 0 models for system-managed-only props, got %d", len(models))
		}
	})

	t.Run("ignore_properties_filters_named_prop", func(t *testing.T) {
		props := iceberg.Properties{
			"custom.engine.stats": "100",
			"write.metadata.compression-codec": "gzip",
		}
		ignore := ignorePropsSet(types.ListValueMust(types.StringType, []attr.Value{
			types.StringValue("custom.engine.stats"),
		}))
		models := propertiesToPropertyModels(props, ignore)
		if len(models) != 1 {
			t.Fatalf("expected 1 model after ignore, got %d: %v", len(models), models)
		}
		if models[0].Name.ValueString() != "write.metadata.compression-codec" {
			t.Errorf("unexpected model: %+v", models[0])
		}
	})

	t.Run("ignore_properties_null_list_no_filter", func(t *testing.T) {
		props := iceberg.Properties{
			"write.metadata.compression-codec": "gzip",
		}
		models := propertiesToPropertyModels(props, ignorePropsSet(types.ListNull(types.StringType)))
		if len(models) != 1 {
			t.Errorf("expected 1 model with null ignore list, got %d", len(models))
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

func (m *mockTransaction) UpdateSchema(_, _ bool) schemaUpdater { return m.schema }
func (m *mockTransaction) UpdateSpec(_ bool) partitionUpdater   { return m.partition }

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
		return PropertyModel{Name: types.StringValue(name), Value: types.StringValue(value), Type: types.StringValue("text")}
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
			err := checkPropChanges(tt.state, tt.plan, nil)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkPropChanges() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPropertyMismatchErr(t *testing.T) {
	pm := func(name, value string) PropertyModel {
		return PropertyModel{Name: types.StringValue(name), Value: types.StringValue(value), Type: types.StringValue("text")}
	}

	t.Run("no_state_props_tells_user_to_remove_blocks", func(t *testing.T) {
		err := propertyMismatchErr(nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "Remove all property blocks") {
			t.Errorf("error missing removal hint: %v", err)
		}
	})

	t.Run("state_props_shown_as_hcl", func(t *testing.T) {
		err := propertyMismatchErr([]PropertyModel{pm("write.metadata.compression-codec", "gzip")})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		msg := err.Error()
		if !strings.Contains(msg, `name  = "write.metadata.compression-codec"`) {
			t.Errorf("error missing property name: %v", msg)
		}
		if !strings.Contains(msg, `value = "gzip"`) {
			t.Errorf("error missing property value: %v", msg)
		}
		if !strings.Contains(msg, "property {") {
			t.Errorf("error missing HCL block: %v", msg)
		}
	})

	t.Run("checkPropChanges_state_has_extra_shows_hcl", func(t *testing.T) {
		state := []PropertyModel{pm("k", "v")}
		err := checkPropChanges(state, nil, nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), `name  = "k"`) {
			t.Errorf("error missing HCL: %v", err)
		}
	})

	t.Run("checkPropChanges_ignored_prop_in_state_no_error", func(t *testing.T) {
		// Transition case: ignored prop still in state from a previous Read where
		// ignore_properties was null. Should not error after user adds it to ignore list.
		state := []PropertyModel{pm("engine.prop", "x"), pm("user.prop", "v")}
		plan := []PropertyModel{pm("user.prop", "v")}
		ignore := map[string]struct{}{"engine.prop": {}}
		err := checkPropChanges(state, plan, ignore)
		if err != nil {
			t.Errorf("expected no error when ignored prop is in state but not plan, got: %v", err)
		}
	})

	t.Run("checkPropChanges_ignored_prop_in_both_no_error", func(t *testing.T) {
		// Ignored prop appears in both state and plan (e.g. user put it in a property
		// block AND ignore_properties). Both sides drop it; no mismatch.
		state := []PropertyModel{pm("engine.prop", "x")}
		plan := []PropertyModel{pm("engine.prop", "x")}
		ignore := map[string]struct{}{"engine.prop": {}}
		err := checkPropChanges(state, plan, ignore)
		if err != nil {
			t.Errorf("expected no error, got: %v", err)
		}
	})
}

func TestCheckPropValueEqual(t *testing.T) {
	tests := []struct {
		name     string
		stateVal string
		planVal  string
		planType string
		wantErr  bool
	}{
		{"text_equal", "foo", "foo", "text", false},
		{"text_different", "foo", "bar", "text", true},
		{"empty_type_equal", "foo", "foo", "", false},
		{"empty_type_different", "foo", "bar", "", true},
		{"json_key_order_differs", `{"a":1,"b":2}`, `{"b":2,"a":1}`, "json", false},
		{"json_whitespace_differs", `{"x":1}`, `{ "x" : 1 }`, "json", false},
		{"json_value_differs", `{"a":1}`, `{"a":2}`, "json", true},
		{"json_invalid_state", `not-json`, `{"a":1}`, "json", true},
		{"json_invalid_plan", `{"a":1}`, `not-json`, "json", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkPropValueEqual("prop", tt.stateVal, tt.planVal, tt.planType)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkPropValueEqual() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestUpdate_PropertyChanges tests the property-change detection logic exercised
// by the Update method. State properties always carry type="text" (as returned by
// Read); plan properties may carry type="json". The Update path calls
// checkPropChanges, which must not treat equivalent JSON as a drift error.
func TestUpdate_PropertyChanges(t *testing.T) {
	stateText := func(name, value string) PropertyModel {
		return PropertyModel{Name: types.StringValue(name), Value: types.StringValue(value), Type: types.StringValue("text")}
	}
	planJSON := func(name, value string) PropertyModel {
		return PropertyModel{Name: types.StringValue(name), Value: types.StringValue(value), Type: types.StringValue("json")}
	}

	t.Run("json_property_same_value_different_formatting_no_error", func(t *testing.T) {
		// Positive: plan JSON is semantically identical to state value but formatted
		// differently (key order, whitespace). Update must not report a change.
		state := []PropertyModel{stateText("cfg", `{"b":2,"a":1}`)}
		plan := []PropertyModel{planJSON("cfg", `{"a": 1, "b": 2}`)}
		if err := checkPropChanges(state, plan, nil); err != nil {
			t.Errorf("expected no error for semantically equal JSON, got: %v", err)
		}
	})

	t.Run("json_property_different_value_raises_not_supported_error", func(t *testing.T) {
		// Negative: plan JSON encodes different data from state. Update must return
		// an error (property changes are not supported).
		state := []PropertyModel{stateText("cfg", `{"a":1}`)}
		plan := []PropertyModel{planJSON("cfg", `{"a":2}`)}
		if err := checkPropChanges(state, plan, nil); err == nil {
			t.Error("expected error for semantically different JSON, got nil")
		}
	})
}

func TestPartitionsMatch(t *testing.T) {
	pm := func(src, transform, name string) PartitionModel {
		return PartitionModel{
			SourceName: types.StringValue(src),
			Transform:  types.StringValue(transform),
			Name:       types.StringValue(name),
		}
	}

	tests := []struct {
		name string
		want []PartitionModel
		got  []PartitionModel
		match bool
	}{
		{
			name:  "identical_same_order",
			want:  []PartitionModel{pm("ts", "hour", "ts_hour")},
			got:   []PartitionModel{pm("ts", "hour", "ts_hour")},
			match: true,
		},
		{
			name:  "identical_different_order",
			want:  []PartitionModel{pm("ts", "hour", "ts_hour"), pm("id", "identity", "id_part")},
			got:   []PartitionModel{pm("id", "identity", "id_part"), pm("ts", "hour", "ts_hour")},
			match: true,
		},
		{
			name:  "void_in_got_filtered_still_matches",
			want:  []PartitionModel{pm("ts", "hour", "ts_hour")},
			got:   []PartitionModel{pm("ts", "void", "ts_month"), pm("ts", "hour", "ts_hour")},
			match: true,
		},
		{
			name:  "all_void_in_got_matches_empty_want",
			want:  nil,
			got:   []PartitionModel{pm("ts", "void", "ts_month")},
			match: true,
		},
		{
			name:  "both_empty",
			want:  nil,
			got:   nil,
			match: true,
		},
		{
			name:  "different_transform",
			want:  []PartitionModel{pm("ts", "hour", "ts_hour")},
			got:   []PartitionModel{pm("ts", "month", "ts_hour")},
			match: false,
		},
		{
			name:  "different_source_name",
			want:  []PartitionModel{pm("ts", "hour", "ts_hour")},
			got:   []PartitionModel{pm("created_at", "hour", "ts_hour")},
			match: false,
		},
		{
			name:  "extra_active_field_in_got",
			want:  []PartitionModel{pm("ts", "hour", "ts_hour")},
			got:   []PartitionModel{pm("ts", "hour", "ts_hour"), pm("id", "identity", "id_part")},
			match: false,
		},
		{
			name:  "missing_field_in_got",
			want:  []PartitionModel{pm("ts", "hour", "ts_hour"), pm("id", "identity", "id_part")},
			got:   []PartitionModel{pm("ts", "hour", "ts_hour")},
			match: false,
		},
		{
			name:  "name_mismatch",
			want:  []PartitionModel{pm("ts", "hour", "ts_hour")},
			got:   []PartitionModel{pm("ts", "hour", "ts_hour_renamed")},
			match: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := partitionsMatch(tt.want, tt.got)
			if got != tt.match {
				t.Errorf("partitionsMatch() = %v, want %v", got, tt.match)
			}
		})
	}
}

func TestFieldNameValidator(t *testing.T) {
	fieldNameValidator := stringvalidator.All(
		stringvalidator.LengthBetween(1, 255),
		stringvalidator.RegexMatches(
			regexp.MustCompile(`^[a-z_][a-z0-9_]*$`),
			"must start with a lowercase letter or underscore and contain only lowercase letters, digits, and underscores",
		),
	)

	validate := func(value string) bool {
		req := validator.StringRequest{
			Path:           fwpath.Root("name"),
			PathExpression: fwpath.MatchRoot("name"),
			ConfigValue:    types.StringValue(value),
		}
		resp := validator.StringResponse{}
		fieldNameValidator.ValidateString(context.Background(), req, &resp)
		return !resp.Diagnostics.HasError()
	}

	valid := []string{
		"id",
		"user_name",
		"_internal",
		"col1",
		"a",
		"a1_b2_c3",
		strings.Repeat("a", 255),
	}
	for _, name := range valid {
		if !validate(name) {
			t.Errorf("expected %q to be valid, got error", name)
		}
	}

	invalid := []struct {
		name  string
		value string
	}{
		{"uppercase_letter", "UserName"},
		{"all_uppercase", "FIELD"},
		{"mixed_case", "HealthSafetyEmployeeSafetyIncidents"},
		{"trailing_uppercase", "my_fieldA"},
		{"starts_with_digit", "1col"},
		{"contains_hyphen", "col-name"},
		{"contains_space", "col name"},
		{"contains_dot", "col.name"},
		{"empty", ""},
		{"too_long", strings.Repeat("a", 256)},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			if validate(tt.value) {
				t.Errorf("expected %q to be invalid, got no error", tt.value)
			}
		})
	}
}

// ── mockCatalog ───────────────────────────────────────────────────────────────

// mockCatalog implements catalog.Catalog for testing refreshUntilConsistent.
// Only LoadTable is functional; all other methods panic.
type mockCatalog struct {
	loadTableFn func(ctx context.Context, id itable.Identifier) (*itable.Table, error)
}

func (m *mockCatalog) LoadTable(ctx context.Context, id itable.Identifier) (*itable.Table, error) {
	return m.loadTableFn(ctx, id)
}
func (m *mockCatalog) CatalogType() catalog.Type                { panic("not implemented") }
func (m *mockCatalog) CreateTable(_ context.Context, _ itable.Identifier, _ *iceberg.Schema, _ ...catalog.CreateTableOpt) (*itable.Table, error) {
	panic("not implemented")
}
func (m *mockCatalog) CommitTable(_ context.Context, _ itable.Identifier, _ []itable.Requirement, _ []itable.Update) (itable.Metadata, string, error) {
	panic("not implemented")
}
func (m *mockCatalog) ListTables(_ context.Context, _ itable.Identifier) iter.Seq2[itable.Identifier, error] {
	panic("not implemented")
}
func (m *mockCatalog) DropTable(_ context.Context, _ itable.Identifier) error {
	panic("not implemented")
}
func (m *mockCatalog) RenameTable(_ context.Context, _, _ itable.Identifier) (*itable.Table, error) {
	panic("not implemented")
}
func (m *mockCatalog) CheckTableExists(_ context.Context, _ itable.Identifier) (bool, error) {
	panic("not implemented")
}
func (m *mockCatalog) ListNamespaces(_ context.Context, _ itable.Identifier) ([]itable.Identifier, error) {
	panic("not implemented")
}
func (m *mockCatalog) CreateNamespace(_ context.Context, _ itable.Identifier, _ iceberg.Properties) error {
	panic("not implemented")
}
func (m *mockCatalog) DropNamespace(_ context.Context, _ itable.Identifier) error {
	panic("not implemented")
}
func (m *mockCatalog) CheckNamespaceExists(_ context.Context, _ itable.Identifier) (bool, error) {
	panic("not implemented")
}
func (m *mockCatalog) LoadNamespaceProperties(_ context.Context, _ itable.Identifier) (iceberg.Properties, error) {
	panic("not implemented")
}
func (m *mockCatalog) UpdateNamespaceProperties(_ context.Context, _ itable.Identifier, _ []string, _ iceberg.Properties) (catalog.PropertiesUpdateSummary, error) {
	panic("not implemented")
}

// buildTestTable constructs a minimal *itable.Table from FieldModels and PartitionModels.
func buildTestTable(t *testing.T, fields []FieldModel, partitions []PartitionModel) *itable.Table {
	t.Helper()
	schema, err := BuildSchema(fields)
	if err != nil {
		t.Fatalf("BuildSchema: %v", err)
	}
	spec, err := BuildPartitionSpec(partitions, schema)
	if err != nil {
		t.Fatalf("BuildPartitionSpec: %v", err)
	}
	builder, err := itable.NewMetadataBuilder(2)
	if err != nil {
		t.Fatalf("NewMetadataBuilder: %v", err)
	}
	if err := builder.AddSchema(schema); err != nil {
		t.Fatalf("AddSchema: %v", err)
	}
	if err := builder.SetCurrentSchemaID(0); err != nil {
		t.Fatalf("SetCurrentSchemaID: %v", err)
	}
	if err := builder.AddPartitionSpec(spec, true); err != nil {
		t.Fatalf("AddPartitionSpec: %v", err)
	}
	if err := builder.SetDefaultSpecID(spec.ID()); err != nil {
		t.Fatalf("SetDefaultSpecID: %v", err)
	}
	sortOrder := itable.UnsortedSortOrder
	if err := builder.AddSortOrder(&sortOrder); err != nil {
		t.Fatalf("AddSortOrder: %v", err)
	}
	if err := builder.SetDefaultSortOrderID(itable.UnsortedSortOrderID); err != nil {
		t.Fatalf("SetDefaultSortOrderID: %v", err)
	}
	meta, err := builder.Build()
	if err != nil {
		t.Fatalf("MetadataBuilder.Build: %v", err)
	}
	return itable.New(itable.Identifier{"ns", "tbl"}, meta, "", nil, nil)
}

func TestRefreshUntilConsistent(t *testing.T) {
	ctx := context.Background()
	identifier := itable.Identifier{"ns", "tbl"}

	fm := func(name string) FieldModel {
		return FieldModel{
			Name:          types.StringValue(name),
			Type:          types.StringValue("string"),
			Required:      types.BoolValue(false),
			DefaultString: types.StringNull(),
			DefaultNumber: types.NumberNull(),
			DefaultBool:   types.BoolNull(),
			Doc:           types.StringValue(""),
		}
	}
	pm := func(src, transform, name string) PartitionModel {
		return PartitionModel{
			SourceName: types.StringValue(src),
			Transform:  types.StringValue(transform),
			Name:       types.StringValue(name),
		}
	}

	wantFields := []FieldModel{fm("id"), fm("ts")}
	wantPartitions := []PartitionModel{pm("ts", "identity", "ts_part")}

	plan := S3TableResourceModel{Fields: wantFields, Partitions: wantPartitions}

	consistentTbl := buildTestTable(t, wantFields, wantPartitions)

	staleFields := []FieldModel{fm("id"), fm("ts")}
	stalePartitions := []PartitionModel{pm("ts", "identity", "ts_stale")}
	staleTbl := buildTestTable(t, staleFields, stalePartitions)

	t.Run("consistent_on_first_attempt_no_sleep", func(t *testing.T) {
		var sleepCalls []time.Duration
		calls := 0
		cat := &mockCatalog{loadTableFn: func(_ context.Context, _ itable.Identifier) (*itable.Table, error) {
			calls++
			return consistentTbl, nil
		}}
		result, err := refreshUntilConsistent(ctx, cat, identifier, plan, time.Second,
			func(d time.Duration) { sleepCalls = append(sleepCalls, d) })
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if calls != 1 {
			t.Errorf("LoadTable called %d times, want 1", calls)
		}
		if len(sleepCalls) != 0 {
			t.Errorf("sleep called %d times, want 0", len(sleepCalls))
		}
		if result == nil {
			t.Fatal("result is nil")
		}
	})

	t.Run("stale_then_consistent_sleeps_once", func(t *testing.T) {
		var sleepCalls []time.Duration
		calls := 0
		cat := &mockCatalog{loadTableFn: func(_ context.Context, _ itable.Identifier) (*itable.Table, error) {
			calls++
			if calls == 1 {
				return staleTbl, nil
			}
			return consistentTbl, nil
		}}
		result, err := refreshUntilConsistent(ctx, cat, identifier, plan, time.Second,
			func(d time.Duration) { sleepCalls = append(sleepCalls, d) })
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if calls != 2 {
			t.Errorf("LoadTable called %d times, want 2", calls)
		}
		if len(sleepCalls) != 1 || sleepCalls[0] != time.Second {
			t.Errorf("sleep calls = %v, want [1s]", sleepCalls)
		}
		if result == nil {
			t.Fatal("result is nil")
		}
	})

	t.Run("load_error_retried_then_succeeds", func(t *testing.T) {
		calls := 0
		cat := &mockCatalog{loadTableFn: func(_ context.Context, _ itable.Identifier) (*itable.Table, error) {
			calls++
			if calls < 3 {
				return nil, fmt.Errorf("transient catalog error")
			}
			return consistentTbl, nil
		}}
		result, err := refreshUntilConsistent(ctx, cat, identifier, plan, time.Millisecond,
			func(time.Duration) {})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if calls != 3 {
			t.Errorf("LoadTable called %d times, want 3", calls)
		}
		if result == nil {
			t.Fatal("result is nil")
		}
	})

	t.Run("always_stale_exhausts_retries_returns_error", func(t *testing.T) {
		calls := 0
		cat := &mockCatalog{loadTableFn: func(_ context.Context, _ itable.Identifier) (*itable.Table, error) {
			calls++
			return staleTbl, nil
		}}
		_, err := refreshUntilConsistent(ctx, cat, identifier, plan, time.Millisecond,
			func(time.Duration) {})
		if err == nil {
			t.Error("expected error when never consistent, got nil")
		}
		if calls != refreshMaxRetries+1 {
			t.Errorf("LoadTable called %d times, want %d (1 initial + %d retries)", calls, refreshMaxRetries+1, refreshMaxRetries)
		}
	})

	t.Run("exponential_backoff_doubles_each_retry", func(t *testing.T) {
		var sleepCalls []time.Duration
		// Return stale for first 3 attempts, then consistent.
		calls := 0
		cat := &mockCatalog{loadTableFn: func(_ context.Context, _ itable.Identifier) (*itable.Table, error) {
			calls++
			if calls <= 3 {
				return staleTbl, nil
			}
			return consistentTbl, nil
		}}
		_, err := refreshUntilConsistent(ctx, cat, identifier, plan, time.Second,
			func(d time.Duration) { sleepCalls = append(sleepCalls, d) })
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
		if !reflect.DeepEqual(sleepCalls, want) {
			t.Errorf("sleep sequence = %v, want %v", sleepCalls, want)
		}
	})

	t.Run("persistent_load_error_returns_last_error", func(t *testing.T) {
		cat := &mockCatalog{loadTableFn: func(_ context.Context, _ itable.Identifier) (*itable.Table, error) {
			return nil, fmt.Errorf("persistent error")
		}}
		_, err := refreshUntilConsistent(ctx, cat, identifier, plan, time.Millisecond,
			func(time.Duration) {})
		if err == nil || err.Error() != "persistent error" {
			t.Errorf("expected 'persistent error', got %v", err)
		}
	})
}

// TestBuildSchema_NestedTypes covers list, map, and struct type schema building.
func TestBuildSchema_NestedTypes(t *testing.T) {
	t.Run("list_type_auto_id", func(t *testing.T) {
		fields := []FieldModel{
			{Name: types.StringValue("tags"), Required: types.BoolValue(false), Doc: types.StringValue(""),
				Type: types.StringNull(), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(),
				ListType: &ListTypeModel{ID: types.Int64Null(), ElementType: types.StringValue("string"), Required: types.BoolValue(false)}},
		}
		s, err := BuildSchema(fields)
		if err != nil {
			t.Fatalf("BuildSchema error: %v", err)
		}
		f := s.Fields()[0]
		lt, ok := f.Type.(*iceberg.ListType)
		if !ok {
			t.Fatalf("expected ListType, got %T", f.Type)
		}
		if lt.ElementID != 2 { // 1 field, so nested starts at 2
			t.Errorf("ElementID = %d, want 2", lt.ElementID)
		}
		if lt.Element.String() != "string" {
			t.Errorf("Element = %q, want string", lt.Element.String())
		}
	})

	t.Run("map_type_auto_id", func(t *testing.T) {
		fields := []FieldModel{
			{Name: types.StringValue("counts"), Required: types.BoolValue(false), Doc: types.StringValue(""),
				Type: types.StringNull(), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(),
				MapType: &MapTypeModel{KeyID: types.Int64Null(), ValueID: types.Int64Null(), KeyType: types.StringValue("string"), ValueType: types.StringValue("long"), Required: types.BoolValue(false)}},
		}
		s, err := BuildSchema(fields)
		if err != nil {
			t.Fatalf("BuildSchema error: %v", err)
		}
		f := s.Fields()[0]
		mt, ok := f.Type.(*iceberg.MapType)
		if !ok {
			t.Fatalf("expected MapType, got %T", f.Type)
		}
		if mt.KeyID != 2 {
			t.Errorf("KeyID = %d, want 2", mt.KeyID)
		}
		if mt.ValueID != 3 {
			t.Errorf("ValueID = %d, want 3", mt.ValueID)
		}
	})

	t.Run("struct_type_sub_fields", func(t *testing.T) {
		fields := []FieldModel{
			{Name: types.StringValue("address"), Required: types.BoolValue(false), Doc: types.StringValue(""),
				Type: types.StringNull(), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(),
				StructType: &StructTypeModel{Fields: []StructSubFieldModel{
					{ID: types.Int64Null(), Name: types.StringValue("street"), Type: types.StringValue("string"), Required: types.BoolValue(false), Doc: types.StringValue("")},
					{ID: types.Int64Null(), Name: types.StringValue("zip"), Type: types.StringValue("string"), Required: types.BoolValue(true), Doc: types.StringValue("")},
				}}},
		}
		s, err := BuildSchema(fields)
		if err != nil {
			t.Fatalf("BuildSchema error: %v", err)
		}
		f := s.Fields()[0]
		st, ok := f.Type.(*iceberg.StructType)
		if !ok {
			t.Fatalf("expected StructType, got %T", f.Type)
		}
		if len(st.FieldList) != 2 {
			t.Fatalf("expected 2 sub-fields, got %d", len(st.FieldList))
		}
		if st.FieldList[0].ID != 2 {
			t.Errorf("sub-field 0 ID = %d, want 2", st.FieldList[0].ID)
		}
		if st.FieldList[1].ID != 3 {
			t.Errorf("sub-field 1 ID = %d, want 3", st.FieldList[1].ID)
		}
		if st.FieldList[0].Name != "street" {
			t.Errorf("sub-field 0 name = %q, want street", st.FieldList[0].Name)
		}
		if !st.FieldList[1].Required {
			t.Error("zip should be required")
		}
	})

	t.Run("explicit_ids_validated", func(t *testing.T) {
		fields := []FieldModel{
			{Name: types.StringValue("items"), Required: types.BoolValue(false), Doc: types.StringValue(""),
				Type: types.StringNull(), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(),
				ListType: &ListTypeModel{ID: types.Int64Value(10), ElementType: types.StringValue("int"), Required: types.BoolValue(false)}},
		}
		s, err := BuildSchema(fields)
		if err != nil {
			t.Fatalf("BuildSchema error: %v", err)
		}
		lt := s.Fields()[0].Type.(*iceberg.ListType)
		if lt.ElementID != 10 {
			t.Errorf("ElementID = %d, want 10", lt.ElementID)
		}
	})

	t.Run("duplicate_ids_rejected", func(t *testing.T) {
		fields := []FieldModel{
			{Name: types.StringValue("items"), Required: types.BoolValue(false), Doc: types.StringValue(""),
				Type: types.StringNull(), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(),
				ListType: &ListTypeModel{ID: types.Int64Value(1), ElementType: types.StringValue("int"), Required: types.BoolValue(false)}},
		}
		_, err := BuildSchema(fields)
		if err == nil {
			t.Fatal("expected error for duplicate ID 1 (same as top-level field), got nil")
		}
	})

	t.Run("partial_ids_rejected", func(t *testing.T) {
		fields := []FieldModel{
			{Name: types.StringValue("a"), Required: types.BoolValue(false), Doc: types.StringValue(""),
				Type: types.StringNull(), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(),
				ListType: &ListTypeModel{ID: types.Int64Value(10), ElementType: types.StringValue("int"), Required: types.BoolValue(false)}},
			{Name: types.StringValue("b"), Required: types.BoolValue(false), Doc: types.StringValue(""),
				Type: types.StringNull(), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(),
				ListType: &ListTypeModel{ID: types.Int64Null(), ElementType: types.StringValue("string"), Required: types.BoolValue(false)}},
		}
		_, err := BuildSchema(fields)
		if err == nil {
			t.Fatal("expected error for partial ID specification, got nil")
		}
	})

	t.Run("mixed_primitive_and_nested", func(t *testing.T) {
		fields := []FieldModel{
			{Name: types.StringValue("id"), Required: types.BoolValue(true), Doc: types.StringValue(""),
				Type: types.StringValue("long"), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull()},
			{Name: types.StringValue("tags"), Required: types.BoolValue(false), Doc: types.StringValue(""),
				Type: types.StringNull(), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(),
				ListType: &ListTypeModel{ID: types.Int64Null(), ElementType: types.StringValue("string"), Required: types.BoolValue(false)}},
		}
		s, err := BuildSchema(fields)
		if err != nil {
			t.Fatalf("BuildSchema error: %v", err)
		}
		if len(s.Fields()) != 2 {
			t.Fatalf("expected 2 fields, got %d", len(s.Fields()))
		}
		if s.Fields()[0].Type.String() != "long" {
			t.Errorf("field 0 type = %q, want long", s.Fields()[0].Type.String())
		}
		lt, ok := s.Fields()[1].Type.(*iceberg.ListType)
		if !ok {
			t.Fatalf("field 1 should be list type")
		}
		if lt.ElementID != 3 {
			t.Errorf("ElementID = %d, want 3 (after 2 top-level fields)", lt.ElementID)
		}
	})
}

func TestIcebergToFieldModel_NestedTypes(t *testing.T) {
	t.Run("list_type", func(t *testing.T) {
		nf := &iceberg.NestedField{
			ID:   1,
			Name: "tags",
			Type: &iceberg.ListType{ElementID: 5, Element: iceberg.PrimitiveTypes.String, ElementRequired: false},
		}
		m, err := icebergToFieldModel(nf)
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if !m.Type.IsNull() {
			t.Error("Type should be null for list field")
		}
		if m.ListType == nil {
			t.Fatal("ListType should be set")
		}
		if m.ListType.ID.ValueInt64() != 5 {
			t.Errorf("ElementID = %d, want 5", m.ListType.ID.ValueInt64())
		}
		if m.ListType.ElementType.ValueString() != "string" {
			t.Errorf("ElementType = %q, want string", m.ListType.ElementType.ValueString())
		}
	})

	t.Run("map_type", func(t *testing.T) {
		nf := &iceberg.NestedField{
			ID:   1,
			Name: "counts",
			Type: &iceberg.MapType{KeyID: 2, KeyType: iceberg.PrimitiveTypes.String, ValueID: 3, ValueType: iceberg.PrimitiveTypes.Int64, ValueRequired: false},
		}
		m, err := icebergToFieldModel(nf)
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if m.MapType == nil {
			t.Fatal("MapType should be set")
		}
		if m.MapType.KeyID.ValueInt64() != 2 {
			t.Errorf("KeyID = %d, want 2", m.MapType.KeyID.ValueInt64())
		}
		if m.MapType.ValueID.ValueInt64() != 3 {
			t.Errorf("ValueID = %d, want 3", m.MapType.ValueID.ValueInt64())
		}
		if m.MapType.KeyType.ValueString() != "string" {
			t.Errorf("KeyType = %q, want string", m.MapType.KeyType.ValueString())
		}
		if m.MapType.ValueType.ValueString() != "long" {
			t.Errorf("ValueType = %q, want long", m.MapType.ValueType.ValueString())
		}
	})

	t.Run("struct_type", func(t *testing.T) {
		nf := &iceberg.NestedField{
			ID:   1,
			Name: "address",
			Type: &iceberg.StructType{FieldList: []iceberg.NestedField{
				{ID: 2, Name: "street", Type: iceberg.PrimitiveTypes.String, Required: false},
				{ID: 3, Name: "zip", Type: iceberg.PrimitiveTypes.String, Required: true},
			}},
		}
		m, err := icebergToFieldModel(nf)
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if m.StructType == nil {
			t.Fatal("StructType should be set")
		}
		if len(m.StructType.Fields) != 2 {
			t.Fatalf("expected 2 sub-fields, got %d", len(m.StructType.Fields))
		}
		if m.StructType.Fields[0].Name.ValueString() != "street" {
			t.Error("expected street")
		}
		if !m.StructType.Fields[1].Required.ValueBool() {
			t.Error("zip should be required")
		}
		if m.StructType.Fields[0].ID.ValueInt64() != 2 {
			t.Errorf("sub-field 0 ID = %d, want 2", m.StructType.Fields[0].ID.ValueInt64())
		}
		if m.StructType.Fields[1].ID.ValueInt64() != 3 {
			t.Errorf("sub-field 1 ID = %d, want 3", m.StructType.Fields[1].ID.ValueInt64())
		}
	})
}

func TestResolveNestedIDs(t *testing.T) {
	makeList := func(id types.Int64) *ListTypeModel {
		return &ListTypeModel{ID: id, ElementType: types.StringValue("string"), Required: types.BoolValue(false)}
	}
	makeMap := func(kid, vid types.Int64) *MapTypeModel {
		return &MapTypeModel{KeyID: kid, ValueID: vid, KeyType: types.StringValue("string"), ValueType: types.StringValue("long"), Required: types.BoolValue(false)}
	}

	t.Run("no_nested_types_unchanged", func(t *testing.T) {
		fields := []FieldModel{{Name: types.StringValue("x"), Type: types.StringValue("int"), Required: types.BoolValue(false), Doc: types.StringValue(""), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull()}}
		got, err := resolveNestedIDs(fields)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 field")
		}
	})

	t.Run("list_auto_assign", func(t *testing.T) {
		fields := []FieldModel{
			{Name: types.StringValue("x"), Type: types.StringValue("int"), Required: types.BoolValue(false), Doc: types.StringValue(""), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull()},
			{Name: types.StringValue("tags"), Type: types.StringNull(), Required: types.BoolValue(false), Doc: types.StringValue(""), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(), ListType: makeList(types.Int64Null())},
		}
		got, err := resolveNestedIDs(fields)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got[1].ListType.ID.ValueInt64() != 3 {
			t.Errorf("ElementID = %d, want 3 (after 2 top-level fields)", got[1].ListType.ID.ValueInt64())
		}
	})

	t.Run("map_auto_assign", func(t *testing.T) {
		fields := []FieldModel{
			{Name: types.StringValue("counts"), Type: types.StringNull(), Required: types.BoolValue(false), Doc: types.StringValue(""), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(), MapType: makeMap(types.Int64Null(), types.Int64Null())},
		}
		got, err := resolveNestedIDs(fields)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got[0].MapType.KeyID.ValueInt64() != 2 {
			t.Errorf("KeyID = %d, want 2", got[0].MapType.KeyID.ValueInt64())
		}
		if got[0].MapType.ValueID.ValueInt64() != 3 {
			t.Errorf("ValueID = %d, want 3", got[0].MapType.ValueID.ValueInt64())
		}
	})

	t.Run("explicit_ids_pass_through", func(t *testing.T) {
		fields := []FieldModel{
			{Name: types.StringValue("tags"), Type: types.StringNull(), Required: types.BoolValue(false), Doc: types.StringValue(""), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(), ListType: makeList(types.Int64Value(10))},
		}
		got, err := resolveNestedIDs(fields)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got[0].ListType.ID.ValueInt64() != 10 {
			t.Errorf("ID = %d, want 10", got[0].ListType.ID.ValueInt64())
		}
	})

	t.Run("partial_ids_error", func(t *testing.T) {
		fields := []FieldModel{
			{Name: types.StringValue("a"), Type: types.StringNull(), Required: types.BoolValue(false), Doc: types.StringValue(""), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(), ListType: makeList(types.Int64Value(10))},
			{Name: types.StringValue("b"), Type: types.StringNull(), Required: types.BoolValue(false), Doc: types.StringValue(""), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(), ListType: makeList(types.Int64Null())},
		}
		_, err := resolveNestedIDs(fields)
		if err == nil {
			t.Fatal("expected error for partial ID specification")
		}
	})

	t.Run("duplicate_ids_error", func(t *testing.T) {
		fields := []FieldModel{
			{Name: types.StringValue("items"), Type: types.StringNull(), Required: types.BoolValue(false), Doc: types.StringValue(""), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(),
				ListType: makeList(types.Int64Value(1))}, // ID 1 collides with top-level field 1
		}
		_, err := resolveNestedIDs(fields)
		if err == nil {
			t.Fatal("expected error for duplicate ID")
		}
	})

	t.Run("map_key_value_same_id_error", func(t *testing.T) {
		fields := []FieldModel{
			{Name: types.StringValue("counts"), Type: types.StringNull(), Required: types.BoolValue(false), Doc: types.StringValue(""), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(),
				MapType: makeMap(types.Int64Value(5), types.Int64Value(5))},
		}
		_, err := resolveNestedIDs(fields)
		if err == nil {
			t.Fatal("expected error for key_id == value_id")
		}
	})

	t.Run("map_key_without_value_error", func(t *testing.T) {
		fields := []FieldModel{
			{Name: types.StringValue("counts"), Type: types.StringNull(), Required: types.BoolValue(false), Doc: types.StringValue(""), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(),
				MapType: makeMap(types.Int64Value(5), types.Int64Null())},
		}
		_, err := resolveNestedIDs(fields)
		if err == nil {
			t.Fatal("expected error for key_id set without value_id")
		}
	})

	makeStruct := func(subFieldIDs ...types.Int64) *StructTypeModel {
		sfs := make([]StructSubFieldModel, len(subFieldIDs))
		names := []string{"a", "b", "c", "d"}
		for i, id := range subFieldIDs {
			sfs[i] = StructSubFieldModel{ID: id, Name: types.StringValue(names[i]), Type: types.StringValue("string"), Required: types.BoolValue(false), Doc: types.StringValue("")}
		}
		return &StructTypeModel{Fields: sfs}
	}

	t.Run("struct_auto_assign", func(t *testing.T) {
		fields := []FieldModel{
			{Name: types.StringValue("addr"), Type: types.StringNull(), Required: types.BoolValue(false), Doc: types.StringValue(""), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(),
				StructType: makeStruct(types.Int64Null(), types.Int64Null())},
		}
		got, err := resolveNestedIDs(fields)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got[0].StructType.Fields[0].ID.ValueInt64() != 2 {
			t.Errorf("sub-field 0 ID = %d, want 2", got[0].StructType.Fields[0].ID.ValueInt64())
		}
		if got[0].StructType.Fields[1].ID.ValueInt64() != 3 {
			t.Errorf("sub-field 1 ID = %d, want 3", got[0].StructType.Fields[1].ID.ValueInt64())
		}
	})

	t.Run("struct_explicit_ids_pass_through", func(t *testing.T) {
		fields := []FieldModel{
			{Name: types.StringValue("addr"), Type: types.StringNull(), Required: types.BoolValue(false), Doc: types.StringValue(""), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(),
				StructType: makeStruct(types.Int64Value(10), types.Int64Value(11))},
		}
		got, err := resolveNestedIDs(fields)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got[0].StructType.Fields[0].ID.ValueInt64() != 10 {
			t.Errorf("sub-field 0 ID = %d, want 10", got[0].StructType.Fields[0].ID.ValueInt64())
		}
		if got[0].StructType.Fields[1].ID.ValueInt64() != 11 {
			t.Errorf("sub-field 1 ID = %d, want 11", got[0].StructType.Fields[1].ID.ValueInt64())
		}
	})

	t.Run("struct_partial_ids_rejected", func(t *testing.T) {
		fields := []FieldModel{
			{Name: types.StringValue("addr"), Type: types.StringNull(), Required: types.BoolValue(false), Doc: types.StringValue(""), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(),
				StructType: makeStruct(types.Int64Value(10), types.Int64Null())},
		}
		_, err := resolveNestedIDs(fields)
		if err == nil {
			t.Fatal("expected error for partial struct sub-field IDs")
		}
	})

	t.Run("struct_duplicate_id_rejected", func(t *testing.T) {
		fields := []FieldModel{
			{Name: types.StringValue("addr"), Type: types.StringNull(), Required: types.BoolValue(false), Doc: types.StringValue(""), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(),
				StructType: makeStruct(types.Int64Value(1), types.Int64Value(2))}, // 1 collides with top-level field ID
		}
		_, err := resolveNestedIDs(fields)
		if err == nil {
			t.Fatal("expected error for struct sub-field ID colliding with top-level field ID")
		}
	})
}

func TestFieldModelsEqual(t *testing.T) {
	makeField := func(name, typ string) FieldModel {
		return FieldModel{
			Name: types.StringValue(name), Type: types.StringValue(typ),
			Required: types.BoolValue(false), Doc: types.StringValue(""),
			DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(),
		}
	}

	t.Run("identical_primitive_fields_equal", func(t *testing.T) {
		a := makeField("x", "int")
		b := makeField("x", "int")
		if !fieldModelsEqual(a, b) {
			t.Error("expected equal")
		}
	})

	t.Run("different_type_not_equal", func(t *testing.T) {
		a := makeField("x", "int")
		b := makeField("x", "long")
		if fieldModelsEqual(a, b) {
			t.Error("expected not equal")
		}
	})

	t.Run("nil_vs_non_nil_list_type_not_equal", func(t *testing.T) {
		a := FieldModel{Name: types.StringValue("x"), Type: types.StringNull(), Required: types.BoolValue(false), Doc: types.StringValue(""), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull()}
		b := a
		b.ListType = &ListTypeModel{ID: types.Int64Value(2), ElementType: types.StringValue("string"), Required: types.BoolValue(false)}
		if fieldModelsEqual(a, b) {
			t.Error("expected not equal")
		}
	})

	t.Run("same_list_type_equal", func(t *testing.T) {
		lt := &ListTypeModel{ID: types.Int64Value(2), ElementType: types.StringValue("string"), Required: types.BoolValue(false)}
		a := FieldModel{Name: types.StringValue("tags"), Type: types.StringNull(), Required: types.BoolValue(false), Doc: types.StringValue(""), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(), ListType: lt}
		b := FieldModel{Name: types.StringValue("tags"), Type: types.StringNull(), Required: types.BoolValue(false), Doc: types.StringValue(""), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(),
			ListType: &ListTypeModel{ID: types.Int64Value(2), ElementType: types.StringValue("string"), Required: types.BoolValue(false)}}
		if !fieldModelsEqual(a, b) {
			t.Error("expected equal")
		}
	})
}

func TestNestedTypeUpdateErrMsg(t *testing.T) {
	stateFields := []FieldModel{
		{Name: types.StringValue("asset_id"), Type: types.StringValue("string"), Required: types.BoolValue(false), Doc: types.StringValue(""), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull()},
		{Name: types.StringValue("tags"), Type: types.StringNull(), Required: types.BoolValue(false), Doc: types.StringValue(""), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(),
			ListType: &ListTypeModel{ID: types.Int64Value(11), ElementType: types.StringValue("string"), Required: types.BoolValue(false)}},
		{Name: types.StringValue("counts"), Type: types.StringNull(), Required: types.BoolValue(false), Doc: types.StringValue(""), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(),
			MapType: &MapTypeModel{KeyID: types.Int64Value(12), ValueID: types.Int64Value(13), KeyType: types.StringValue("string"), ValueType: types.StringValue("long"), Required: types.BoolValue(false)}},
		{Name: types.StringValue("addr"), Type: types.StringNull(), Required: types.BoolValue(false), Doc: types.StringValue(""), DefaultString: types.StringNull(), DefaultNumber: types.NumberNull(), DefaultBool: types.BoolNull(),
			StructType: &StructTypeModel{Fields: []StructSubFieldModel{
				{ID: types.Int64Value(14), Name: types.StringValue("street"), Type: types.StringValue("string"), Required: types.BoolValue(false), Doc: types.StringValue("")},
			}}},
	}

	origErr := fmt.Errorf("cannot update field type for non-primitive type: tags")
	msg := nestedTypeUpdateErrMsg(origErr, stateFields)

	t.Run("contains_original_error", func(t *testing.T) {
		if !strings.Contains(msg, "cannot update field type for non-primitive type") {
			t.Error("message should contain original error")
		}
	})
	t.Run("list_id_shown", func(t *testing.T) {
		if !strings.Contains(msg, "id       = 11") {
			t.Errorf("expected list element ID 11 in message, got:\n%s", msg)
		}
	})
	t.Run("map_ids_shown", func(t *testing.T) {
		if !strings.Contains(msg, "key_id         = 12") || !strings.Contains(msg, "value_id       = 13") {
			t.Errorf("expected map key/value IDs in message, got:\n%s", msg)
		}
	})
	t.Run("struct_sub_field_id_shown", func(t *testing.T) {
		if !strings.Contains(msg, "id       = 14") {
			t.Errorf("expected struct sub-field ID 14 in message, got:\n%s", msg)
		}
	})
	t.Run("primitive_field_in_comment", func(t *testing.T) {
		if !strings.Contains(msg, "asset_id") {
			t.Errorf("expected primitive field name in message, got:\n%s", msg)
		}
	})
}
