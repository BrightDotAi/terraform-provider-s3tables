// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"testing"

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
			Default:  types.DynamicNull(),
			Doc:      types.StringValue("primary key"),
		},
		{
			Name:     types.StringValue("value"),
			Type:     types.StringValue("string"),
			Required: types.BoolValue(false),
			Default:  types.DynamicNull(),
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
			Default:  types.DynamicNull(),
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
			Default:  types.DynamicValue(types.Float64Value(0.0)),
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
		{Name: types.StringValue("ts"), Type: types.StringValue("timestamp"), Required: types.BoolValue(false), Default: types.DynamicNull(), Doc: types.StringValue("")},
	})
	spec, err := BuildPartitionSpec(nil, s)
	if err != nil {
		t.Fatalf("BuildPartitionSpec() error: %v", err)
	}
	if spec.NumFields() != 0 {
		t.Errorf("expected unpartitioned spec, got %d fields", spec.NumFields())
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
    default = 0.0
  }

  property {
    name  = "write.metadata.compression-codec"
    value = "gzip"
  }
}
`, warehouse, region, namespace, name)
}
