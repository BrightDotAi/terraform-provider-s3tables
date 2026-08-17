// Copyright BrightAI 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	iceberg "github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/catalog"
	"github.com/apache/iceberg-go/catalog/rest"
	_ "github.com/apache/iceberg-go/io"
	itable "github.com/apache/iceberg-go/table"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

var _ resource.Resource = &S3TableResource{}
var _ resource.ResourceWithImportState = &S3TableResource{}
var _ resource.ResourceWithValidateConfig = &S3TableResource{}
var _ resource.ResourceWithModifyPlan = &S3TableResource{}

// NewS3TableResource returns a new S3TableResource instance for registration with the Terraform provider.
func NewS3TableResource() resource.Resource {
	return &S3TableResource{}
}

// refreshMaxRetries is the maximum number of retries when waiting for table
// metadata to become consistent after a commit.
const refreshMaxRetries = 8

// Property defaults added to table automatically

var prop_defaults = map[string]string{
	"table_type":                      "iceberg",
	"write_compression":               "zstd",
	"write.parquet.compression-codec": "zstd",
}

// systemManagedProps is the set of Iceberg table property keys written automatically
// by query engines (Athena, Spark, etc.) as a side effect of DML operations.
// They are never stored in Terraform state so they never cause drift.
var systemManagedProps = map[string]struct{}{
	"schema.name-mapping.default": {},
}

// S3TableResource defines the resource implementation.
type S3TableResource struct {
	awsCfg aws.Config
}

// S3TableResourceModel describes the resource data model.
type S3TableResourceModel struct {
	Warehouse        types.String     `tfsdk:"warehouse"`
	Region           types.String     `tfsdk:"region"`
	Namespace        types.String     `tfsdk:"namespace"`
	Name             types.String     `tfsdk:"name"`
	FormatVersion    types.String     `tfsdk:"format_version"`
	Fields           []FieldModel     `tfsdk:"field"`
	Partitions       []PartitionModel `tfsdk:"partition"`
	Properties       []PropertyModel  `tfsdk:"property"`
	IgnoreProperties types.List       `tfsdk:"ignore_properties"`
}

// FieldModel represents one column in the Iceberg schema.
type FieldModel struct {
	Name          types.String     `tfsdk:"name"`
	Type          types.String     `tfsdk:"type"`
	Required      types.Bool       `tfsdk:"required"`
	DefaultString types.String     `tfsdk:"default_string"`
	DefaultNumber types.Number     `tfsdk:"default_number"`
	DefaultBool   types.Bool       `tfsdk:"default_bool"`
	Doc           types.String     `tfsdk:"doc"`
	ListType      *ListTypeModel   `tfsdk:"list_type"`
	MapType       *MapTypeModel    `tfsdk:"map_type"`
	StructType    *StructTypeModel `tfsdk:"struct_type"`
}

// StructSubFieldModel is a field inside a struct_type block.
// Only primitive types are supported at this depth (Terraform schemas cannot be recursive).
type StructSubFieldModel struct {
	ID       types.Int64  `tfsdk:"id"`
	Name     types.String `tfsdk:"name"`
	Type     types.String `tfsdk:"type"`
	Required types.Bool   `tfsdk:"required"`
	Doc      types.String `tfsdk:"doc"`
}

// ListTypeModel represents a list<T> Iceberg column type.
type ListTypeModel struct {
	ID              types.Int64  `tfsdk:"id"`
	ElementType     types.String `tfsdk:"type"`
	Required        types.Bool   `tfsdk:"required"`
}

// MapTypeModel represents a map<K,V> Iceberg column type.
type MapTypeModel struct {
	KeyID         types.Int64  `tfsdk:"key_id"`
	ValueID       types.Int64  `tfsdk:"value_id"`
	KeyType       types.String `tfsdk:"key_type"`
	ValueType     types.String `tfsdk:"value_type"`
	Required      types.Bool   `tfsdk:"required"`
}

// StructTypeModel represents a struct<...> Iceberg column type.
type StructTypeModel struct {
	Fields []StructSubFieldModel `tfsdk:"field"`
}

// PartitionModel represents one field in the Iceberg partition spec.
type PartitionModel struct {
	SourceName types.String `tfsdk:"source_name"`
	Transform  types.String `tfsdk:"transform"`
	Name       types.String `tfsdk:"name"`
}

// PropertyModel represents one field in the Iceberg property spec.
type PropertyModel struct {
	Name  types.String `tfsdk:"name"`
	Value types.String `tfsdk:"value"`
	Type  types.String `tfsdk:"type"`
}

// Metadata sets the Terraform type name for this resource to `{provider}_s3tables_table`.
func (r *S3TableResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_s3tables_table"
}

// Schema declares the Terraform schema for the resource, covering top-level attributes
// (warehouse, region, namespace, name, format_version) and the field, partition, and
// property nested blocks.
func (r *S3TableResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an S3 Tables Iceberg table via the AWS Glue catalog.",
		Attributes: map[string]schema.Attribute{
			"warehouse": schema.StringAttribute{
				MarkdownDescription: "Warehouse identifier the S3 table bucket (`{account}:s3tablescatalog/{name}`).",
				Required:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"region": schema.StringAttribute{
				MarkdownDescription: "AWS region for table",
				Required:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"namespace": schema.StringAttribute{
				MarkdownDescription: "Glue database name (namespace) that contains the table.",
				Required:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Name of the table.",
				Required:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"format_version": schema.StringAttribute{
				MarkdownDescription: "Iceberg format version. Accepted values: `2` (default) or `3`. Version 3 is required to use column default values.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("2"),
			},
			"ignore_properties": schema.ListAttribute{
				MarkdownDescription: "Additional table property names to ignore when checking for drift. Applied on top of built-in system-managed properties (e.g. `schema.name-mapping.default`). Useful for properties written by query engines that are not in the built-in ignore list.",
				Optional:            true,
				ElementType:         types.StringType,
			},
		},
		Blocks: map[string]schema.Block{
			"field": schema.ListNestedBlock{
				MarkdownDescription: "Iceberg schema column.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							MarkdownDescription: "Column name. Must contain only lowercase letters, digits, and underscores, and must not start with a digit. AWS S3 Tables normalizes column names to lowercase and does not support uppercase letters.",
							Required:            true,
							Validators: []validator.String{
								stringvalidator.LengthBetween(1, 255),
								stringvalidator.RegexMatches(
									regexp.MustCompile(`^[a-z_][a-z0-9_]*$`),
									"must start with a lowercase letter or underscore and contain only lowercase letters, digits, and underscores",
								),
							},
						},
						"type": schema.StringAttribute{
							MarkdownDescription: "Iceberg primitive type: `boolean`, `int`, `long`, `float`, `double`, `date`, `time`, `timestamp`, `timestamptz`, `string`, `binary`, `uuid`, `fixed[N]`, `decimal(P,S)`. Exactly one of `type`, `list_type`, `map_type`, or `struct_type` must be set.",
							Optional:            true,
							Validators: []validator.String{
								stringvalidator.ConflictsWith(
									path.MatchRelative().AtParent().AtName("list_type"),
									path.MatchRelative().AtParent().AtName("map_type"),
									path.MatchRelative().AtParent().AtName("struct_type"),
								),
							},
						},
						"required": schema.BoolAttribute{
							MarkdownDescription: "Whether the column is non-nullable. Defaults to `false`.",
							Optional:            true,
							Computed:            true,
							Default:             booldefault.StaticBool(false),
						},
						"default_string": schema.StringAttribute{
							MarkdownDescription: "Default value for string column. At most one of `default_string`, `default_bool` or `default_number` should be set.",
							Optional:            true,
							Computed:            false,
						},
						"default_number": schema.NumberAttribute{
							MarkdownDescription: "Default value for integer or float column. At most one of `default_string`, `default_bool` or `default_number` should be set.",
							Optional:            true,
							Computed:            false,
						},
						"default_bool": schema.BoolAttribute{
							MarkdownDescription: "Default value for bool column. At most one of `default_string`, `default_bool` or `default_number` should be set.",
							Optional:            true,
							Computed:            false,
						},
						"doc": schema.StringAttribute{
							MarkdownDescription: "Documentation string for the column.",
							Optional:            true,
							Computed:            true,
							Default:             stringdefault.StaticString(""),
						},
					},
					Blocks: map[string]schema.Block{
						"list_type": schema.SingleNestedBlock{
							MarkdownDescription: "Iceberg list<T> type. Exactly one of `type`, `list_type`, `map_type`, or `struct_type` must be set.",
							Attributes: map[string]schema.Attribute{
								"id": schema.Int64Attribute{
									MarkdownDescription: "Iceberg element field ID. Optional; if any nested type IDs are set across the table, all must be set and globally unique.",
									Optional:            true,
									Computed:            true,
								},
								"type": schema.StringAttribute{
									MarkdownDescription: "Primitive type of list elements. Required when `list_type` block is set.",
									Optional:            true,
								},
								"required": schema.BoolAttribute{
									MarkdownDescription: "Whether list elements are non-nullable. Defaults to `true`.",
									Optional:            true,
									Computed:            true,
									Default:             booldefault.StaticBool(true),
								},
							},
						},
						"map_type": schema.SingleNestedBlock{
							MarkdownDescription: "Iceberg map<K,V> type. Exactly one of `type`, `list_type`, `map_type`, or `struct_type` must be set.",
							Attributes: map[string]schema.Attribute{
								"key_id": schema.Int64Attribute{
									MarkdownDescription: "Iceberg key field ID. Optional; must be set together with `value_id` if any nested type IDs are set.",
									Optional:            true,
									Computed:            true,
								},
								"value_id": schema.Int64Attribute{
									MarkdownDescription: "Iceberg value field ID. Optional; must be set together with `key_id` if any nested type IDs are set.",
									Optional:            true,
									Computed:            true,
								},
								"key_type": schema.StringAttribute{
									MarkdownDescription: "Primitive type of map keys. Required when `map_type` block is set.",
									Optional:            true,
								},
								"value_type": schema.StringAttribute{
									MarkdownDescription: "Primitive type of map values. Required when `map_type` block is set.",
									Optional:            true,
								},
								"required": schema.BoolAttribute{
									MarkdownDescription: "Whether map values are non-nullable. Defaults to `true`.",
									Optional:            true,
									Computed:            true,
									Default:             booldefault.StaticBool(true),
								},
							},
						},
						"struct_type": schema.SingleNestedBlock{
							MarkdownDescription: "Iceberg struct<...> type. Exactly one of `type`, `list_type`, `map_type`, or `struct_type` must be set.",
							Blocks: map[string]schema.Block{
								"field": schema.ListNestedBlock{
									MarkdownDescription: "A field within the struct. Only primitive types are supported.",
									NestedObject: schema.NestedBlockObject{
										Attributes: map[string]schema.Attribute{
											"id": schema.Int64Attribute{
												MarkdownDescription: "Iceberg field ID for this struct sub-field. Optional; if any nested type IDs are set across the table, all must be set and globally unique.",
												Optional:            true,
												Computed:            true,
											},
											"name": schema.StringAttribute{
												MarkdownDescription: "Sub-field name.",
												Required:            true,
												Validators: []validator.String{
													stringvalidator.LengthBetween(1, 255),
													stringvalidator.RegexMatches(
														regexp.MustCompile(`^[a-z_][a-z0-9_]*$`),
														"must start with a lowercase letter or underscore and contain only lowercase letters, digits, and underscores",
													),
												},
											},
											"type": schema.StringAttribute{
												MarkdownDescription: "Primitive Iceberg type of the sub-field.",
												Required:            true,
											},
											"required": schema.BoolAttribute{
												MarkdownDescription: "Whether the sub-field is non-nullable. Defaults to `true`.",
												Optional:            true,
												Computed:            true,
												Default:             booldefault.StaticBool(true),
											},
											"doc": schema.StringAttribute{
												MarkdownDescription: "Documentation for the sub-field.",
												Optional:            true,
												Computed:            true,
												Default:             stringdefault.StaticString(""),
											},
										},
									},
								},
							},
						},
					},
				},
			},
			"partition": schema.ListNestedBlock{
				MarkdownDescription: "Iceberg partition field.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"source_name": schema.StringAttribute{
							MarkdownDescription: "Name of the source column to partition by.",
							Required:            true,
						},
						"transform": schema.StringAttribute{
							MarkdownDescription: "Partition transform: `identity`, `year`, `month`, `day`, `hour`, `bucket[N]`, `truncate[N]`.",
							Required:            true,
						},
						"name": schema.StringAttribute{
							MarkdownDescription: "Name for this partition field.",
							Required:            true,
						},
					},
				},
			},
			"property": schema.ListNestedBlock{
				MarkdownDescription: "Iceberg properties field.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							MarkdownDescription: "Property name.",
							Required:            true,
						},
						"value": schema.StringAttribute{
							MarkdownDescription: "Property value.",
							Required:            true,
						},
						"type": schema.StringAttribute{
							MarkdownDescription: "Property value type: `text` (default) or `json`.",
							Optional:            true,
							Computed:            true,
							Default:             stringdefault.StaticString("text"),
							Validators: []validator.String{
								stringvalidator.OneOf("text", "json"),
							},
						},
					},
				},
			},
		},
	}
}

// Configure extracts the aws.Config from the provider-supplied data and stores it
// on the resource so that CRUD operations can open a catalog connection.
func (r *S3TableResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	cfg, ok := req.ProviderData.(aws.Config)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected aws.Config, got: %T. Please report this issue to the provider developers.", req.ProviderData),
		)
		return
	}
	r.awsCfg = cfg
}

// Create builds the Iceberg schema, partition spec, and properties from the plan and
// creates the table in the S3 Tables catalog, then writes the resulting state back to Terraform.
func (r *S3TableResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data S3TableResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	icebergSchema, err := BuildSchema(data.Fields)
	if err != nil {
		resp.Diagnostics.AddError("Invalid field definition", err.Error())
		return
	}

	partSpec, err := BuildPartitionSpec(data.Partitions, icebergSchema)
	if err != nil {
		resp.Diagnostics.AddError("Invalid partition definition", err.Error())
		return
	}

	properties, err := BuildProperties(data.Properties, data.FormatVersion.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid property definition", err.Error())
		return
	}

	cat, err := data.GetCatalog(ctx, r.awsCfg)
	if err != nil {
		resp.Diagnostics.AddError("Error Connecting to Iceberg Catalog", err.Error())
		return
	}

	identifier := data.GetIdentifier()

	tbl, err := cat.CreateTable(ctx, identifier, icebergSchema,
		catalog.WithPartitionSpec(partSpec),
		catalog.WithProperties(*properties),
	)
	if err != nil {
		resp.Diagnostics.AddError("Error creating Iceberg table", err.Error())
		return
	}

	err = setModelFromTable(&data, tbl)
	if err != nil {
		resp.Diagnostics.AddError("Error converting iceberg fields", err.Error())
		return
	}

	tflog.Trace(ctx, "created Iceberg table", map[string]any{
		"warehouse": data.Warehouse.ValueString(),
		"namespace": data.Namespace.ValueString(),
		"name":      data.Name.ValueString(),
	})
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// Read fetches the current table state from the S3 Tables catalog and refreshes
// Terraform state. If the table no longer exists the resource is removed from state.
func (r *S3TableResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data S3TableResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	cat, err := data.GetCatalog(ctx, r.awsCfg)
	if err != nil {
		resp.Diagnostics.AddError("Error Connecting to Iceberg Catalog", err.Error())
		return
	}

	identifier := data.GetIdentifier()

	tbl, err := cat.LoadTable(ctx, identifier)
	if err != nil {
		if isNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Error reading Iceberg table", err.Error())
		return
	}

	err = setModelFromTable(&data, tbl)
	if err != nil {
		resp.Diagnostics.AddError("Error reading Iceberg fields", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// Update applies schema and partition-spec changes from the plan to the existing table.
// Property changes are not supported by the Iceberg catalog and will be returned as an error.
// State is refreshed from the catalog after the transaction commits.
func (r *S3TableResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var state, plan S3TableResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	cat, err := state.GetCatalog(ctx, r.awsCfg)
	if err != nil {
		resp.Diagnostics.AddError("Error Connecting to Iceberg Catalog", err.Error())
		return
	}

	identifier := state.GetIdentifier()

	tbl, err := cat.LoadTable(ctx, identifier)
	if err != nil {
		resp.Diagnostics.AddError("Error loading Iceberg table for update", err.Error())
		return
	}

	txn := tbl.NewTransaction()

	err = ApplySchemaChanges(&txnAdapter{txn}, state.Fields, plan.Fields)
	if err != nil {
		if isNonPrimitiveUpdateErr(err) {
			resp.Diagnostics.AddError("Error updating schema", nestedTypeUpdateErrMsg(err, state.Fields))
		} else {
			resp.Diagnostics.AddError("Error updating schema", err.Error())
		}
		return
	}

	err = ApplyPartitionChanges(&txnAdapter{txn}, state.Partitions, plan.Partitions)
	if err != nil {
		resp.Diagnostics.AddError("Error updating partition spec", err.Error())
		return
	}

	err = checkPropChanges(state.Properties, plan.Properties, ignorePropsSet(plan.IgnoreProperties))
	if err != nil {
		resp.Diagnostics.AddError("Error - Table property changes not supported", err.Error())
		return
	}

	_, _ = txn.Commit(ctx)
	// Ignoring errors from Commit because of bug loading reloading meta-data after
	// commit causes spurious errors.
	// Instead will refresh table and reload state to confirm updates have been
	// applied correctly.

	result, err := refreshUntilConsistent(ctx, cat, identifier, plan, 1*time.Second, time.Sleep)
	if err != nil {
		resp.Diagnostics.AddError("Error loading iceberg table after commit", err.Error())
		return
	}
	plan.Fields = result.Fields
	plan.Partitions = result.Partitions
	plan.Properties = result.Properties
	plan.FormatVersion = result.FormatVersion

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete purges the Iceberg table from the S3 Tables catalog. A not-found error is
// treated as a successful deletion so that partially destroyed resources can be cleaned up.
func (r *S3TableResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data S3TableResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	cat, err := data.GetCatalog(ctx, r.awsCfg)
	if err != nil {
		resp.Diagnostics.AddError("Error Connecting to Iceberg Catalog", err.Error())
		return
	}

	identifier := data.GetIdentifier()

	err = cat.PurgeTable(ctx, identifier)
	if err != nil {
		if !isNotFound(err) {
			resp.Diagnostics.AddError("Error deleting Iceberg table", err.Error())
		}
	}
}

// partitionsMatch returns true when got contains exactly the same set of
// partition fields as want, ignoring order. Void transforms in got are skipped
// (they are remnants of partition evolution, not active partition fields).
// Comparison is by Name, SourceName, and Transform.
func partitionsMatch(want, got []PartitionModel) bool {
	// Build a map of active (non-void) partitions from got, keyed by name.
	active := make(map[string]PartitionModel, len(got))
	for _, p := range got {
		if p.Transform.ValueString() == "void" {
			continue
		}
		active[p.Name.ValueString()] = p
	}
	if len(active) != len(want) {
		return false
	}
	for _, p := range want {
		g, ok := active[p.Name.ValueString()]
		if !ok {
			return false
		}
		if g.SourceName != p.SourceName || g.Transform != p.Transform {
			return false
		}
	}
	return true
}

// refreshUntilConsistent loads the table fresh from the catalog on each attempt
// with exponential backoff until metadata matches plan.Fields/plan.Partitions,
// or refreshMaxRetries are exhausted. sleepFn is injectable for testing.
func refreshUntilConsistent(
	ctx context.Context,
	cat catalog.Catalog,
	identifier itable.Identifier,
	plan S3TableResourceModel,
	initialBackoff time.Duration,
	sleepFn func(time.Duration),
) (*S3TableResourceModel, error) {
	backoff := initialBackoff
	var lastErr error
	for attempt := 0; attempt <= refreshMaxRetries; attempt++ {
		if attempt > 0 {
			tflog.Debug(ctx, "metadata not yet consistent after commit, retrying",
				map[string]any{"attempt": attempt, "backoff_ms": backoff.Milliseconds()})
			sleepFn(backoff)
			backoff *= 2
		}
		tbl, err := cat.LoadTable(ctx, identifier)
		if err != nil {
			lastErr = err
			continue
		}
		var model S3TableResourceModel
		model.IgnoreProperties = plan.IgnoreProperties
		if err := setModelFromTable(&model, tbl); err != nil {
			lastErr = err
			continue
		}
		if reflect.DeepEqual(model.Fields, plan.Fields) && partitionsMatch(plan.Partitions, model.Partitions) {
			return &model, nil
		}
		lastErr = fmt.Errorf("table metadata not consistent after %d attempt(s)", attempt+1)
	}
	return nil, lastErr
}

// ValidateConfig enforces that each field block has exactly one of type, list_type,
// map_type, or struct_type set, and validates nested type ID constraints.
func (r *S3TableResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	// Skip if any top-level list block is unknown (e.g. dynamic blocks with computed
	// for_each). Validation cannot run on unknown values and []FieldModel /
	// []PartitionModel cannot hold unknown lists. No error — validation re-runs once
	// unknowns resolve.
	for _, attrName := range []string{"field", "property", "partition"} {
		var list types.List
		if diags := req.Config.GetAttribute(ctx, path.Root(attrName), &list); diags.HasError() || list.IsUnknown() {
			return
		}
	}
	var data S3TableResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	for i, f := range data.Fields {
		fp := path.Root("field").AtListIndex(i)

		// Validate list_type child attrs when block is present.
		if f.ListType != nil && (f.ListType.ElementType.IsNull() || f.ListType.ElementType.IsUnknown()) {
			resp.Diagnostics.AddAttributeError(
				fp.AtName("list_type").AtName("type"),
				"Missing required attribute",
				fmt.Sprintf("field %q: list_type.type is required when list_type block is set", f.Name.ValueString()),
			)
		}
		// Validate map_type child attrs when block is present.
		if f.MapType != nil {
			if f.MapType.KeyType.IsNull() || f.MapType.KeyType.IsUnknown() {
				resp.Diagnostics.AddAttributeError(
					fp.AtName("map_type").AtName("key_type"),
					"Missing required attribute",
					fmt.Sprintf("field %q: map_type.key_type is required when map_type block is set", f.Name.ValueString()),
				)
			}
			if f.MapType.ValueType.IsNull() || f.MapType.ValueType.IsUnknown() {
				resp.Diagnostics.AddAttributeError(
					fp.AtName("map_type").AtName("value_type"),
					"Missing required attribute",
					fmt.Sprintf("field %q: map_type.value_type is required when map_type block is set", f.Name.ValueString()),
				)
			}
		}

		typeCount := 0
		if !f.Type.IsNull() && !f.Type.IsUnknown() {
			typeCount++
		}
		if f.ListType != nil && !f.ListType.ElementType.IsNull() {
			typeCount++
		}
		if f.MapType != nil && !f.MapType.KeyType.IsNull() {
			typeCount++
		}
		if f.StructType != nil {
			typeCount++
		}
		if typeCount != 1 {
			resp.Diagnostics.AddAttributeError(
				fp.AtName("type"),
				"Invalid field type",
				fmt.Sprintf("field %q: exactly one of type, list_type, map_type, or struct_type must be set (got %d)", f.Name.ValueString(), typeCount),
			)
		}
	}
	if _, err := resolveNestedIDs(data.Fields); err != nil {
		resp.Diagnostics.AddError("Invalid nested type IDs", err.Error())
	}
}

// ModifyPlan auto-assigns nested type IDs (list ElementID, map KeyID/ValueID) when
// none are set by the user, ensuring the plan stored to state has canonical IDs.
func (r *S3TableResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || !req.Plan.Raw.IsKnown() {
		return
	}
	// Skip if any top-level list block is unknown at the list level. This happens
	// when blocks reference computed values from other resources (dynamic blocks).
	// We must not use IsFullyKnown() here: individual attrs within a known list
	// (e.g. nested type IDs as types.Int64) may be unknown but req.Plan.Get still
	// succeeds because types.Int64 handles unknowns. Only an unknown list itself
	// cannot be decoded into []FieldModel / []PropertyModel / []PartitionModel.
	for _, attrName := range []string{"field", "property", "partition"} {
		var list types.List
		if diags := req.Plan.GetAttribute(ctx, path.Root(attrName), &list); diags.HasError() || list.IsUnknown() {
			return
		}
	}
	var plan S3TableResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resolved, err := resolveNestedIDs(plan.Fields)
	if err != nil {
		resp.Diagnostics.AddError("Invalid nested type IDs", err.Error())
		return
	}
	plan.Fields = resolved
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

// ImportState accepts: warehouse,region,namespace,name
func (r *S3TableResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, ",", 4)
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		resp.Diagnostics.AddError(
			"Invalid import ID",
			fmt.Sprintf("Expected format warehouse,region,namespace,name, got: %q", req.ID),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("warehouse"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("region"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("namespace"), parts[2])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), parts[3])...)
}

// ── helpers ──────────────────────────────────────────────────────────────────

// GetCatalog -  connect to catalog using glue RESTful endpoint
func (data *S3TableResourceModel) GetCatalog(ctx context.Context, awsCfg aws.Config) (*rest.Catalog, error) {
	cat, err := rest.NewCatalog(ctx, "s3tables_catalog",
		"https://glue."+data.Region.ValueString()+".amazonaws.com/iceberg",
		rest.WithAwsConfig(awsCfg),
		rest.WithWarehouseLocation(data.Warehouse.ValueString()),
		rest.WithSigV4(),
		rest.WithSigV4RegionSvc(data.Region.ValueString(), "glue"),
		rest.WithCustomTransport(&metadataPatchTransport{base: http.DefaultTransport}),
	)
	return cat, err
}

// metadataPatchTransport strips v3-only fields (specifically "next-row-id")
// from v1/v2 Iceberg table metadata returned by S3 Tables. S3 Tables includes
// this field even in format-version:2 responses; iceberg-go v0.6.0 rejects it.
type metadataPatchTransport struct {
	base http.RoundTripper
}

// RoundTrip executes the underlying HTTP request and, on success, patches the
// response body to remove v3-only metadata fields that would be rejected by iceberg-go.
func (t *metadataPatchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return resp, err
	}

	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return resp, err
	}

	patched := stripV3FieldsFromV2Metadata(body)
	resp.Body = io.NopCloser(bytes.NewReader(patched))
	resp.ContentLength = int64(len(patched))
	return resp, nil
}

// stripV3FieldsFromV2Metadata removes "next-row-id" from the nested
// "metadata" object when format-version < 3. Returns data unchanged if the
// body does not match the expected shape or no patch is needed.
func stripV3FieldsFromV2Metadata(data []byte) []byte {
	var outer struct {
		Metadata json.RawMessage `json:"metadata"`
	}
	if json.Unmarshal(data, &outer) != nil || len(outer.Metadata) == 0 {
		return data
	}

	var ver struct {
		FormatVersion int `json:"format-version"`
	}
	if json.Unmarshal(outer.Metadata, &ver) != nil || ver.FormatVersion >= 3 {
		return data
	}

	var metaMap map[string]json.RawMessage
	if json.Unmarshal(outer.Metadata, &metaMap) != nil {
		return data
	}
	if _, ok := metaMap["next-row-id"]; !ok {
		return data
	}

	delete(metaMap, "next-row-id")

	patchedMeta, err := json.Marshal(metaMap)
	if err != nil {
		return data
	}

	var outerMap map[string]json.RawMessage
	if json.Unmarshal(data, &outerMap) != nil {
		return data
	}
	outerMap["metadata"] = patchedMeta

	result, err := json.Marshal(outerMap)
	if err != nil {
		return data
	}
	return result
}

// GetIdentifier - get Identifier from table model
func (data *S3TableResourceModel) GetIdentifier() itable.Identifier {
	return catalog.ToIdentifier(data.Namespace.ValueString(), data.Name.ValueString())
}

// fieldModelsEqual compares two FieldModels for semantic equality.
// Uses reflect.DeepEqual for pointer fields (ListType, MapType, StructType).
func fieldModelsEqual(a, b FieldModel) bool {
	return a.Name == b.Name &&
		a.Type == b.Type &&
		a.Required == b.Required &&
		a.DefaultString == b.DefaultString &&
		a.DefaultNumber == b.DefaultNumber &&
		a.DefaultBool == b.DefaultBool &&
		a.Doc == b.Doc &&
		reflect.DeepEqual(a.ListType, b.ListType) &&
		reflect.DeepEqual(a.MapType, b.MapType) &&
		reflect.DeepEqual(a.StructType, b.StructType)
}

// fieldModelToIcebergType builds an iceberg.Type from a FieldModel.
// For nested types used in UpdateSchema.AddColumn, IDs are not set
// (the catalog assigns fresh IDs). For BuildSchema, call resolveNestedIDs first.
func fieldModelToIcebergType(f FieldModel) (iceberg.Type, error) {
	switch {
	case !f.Type.IsNull() && !f.Type.IsUnknown():
		return parseIcebergType(f.Type.ValueString())
	case f.ListType != nil && !f.ListType.ElementType.IsNull():
		elemType, err := parseIcebergType(f.ListType.ElementType.ValueString())
		if err != nil {
			return nil, fmt.Errorf("list_type element: %w", err)
		}
		lt := &iceberg.ListType{Element: elemType, ElementRequired: f.ListType.Required.ValueBool()}
		if idIsSet(f.ListType.ID) {
			lt.ElementID = int(f.ListType.ID.ValueInt64())
		}
		return lt, nil
	case f.MapType != nil && !f.MapType.KeyType.IsNull():
		keyType, err := parseIcebergType(f.MapType.KeyType.ValueString())
		if err != nil {
			return nil, fmt.Errorf("map_type key: %w", err)
		}
		valType, err := parseIcebergType(f.MapType.ValueType.ValueString())
		if err != nil {
			return nil, fmt.Errorf("map_type value: %w", err)
		}
		mt := &iceberg.MapType{KeyType: keyType, ValueType: valType, ValueRequired: f.MapType.Required.ValueBool()}
		if idIsSet(f.MapType.KeyID) {
			mt.KeyID = int(f.MapType.KeyID.ValueInt64())
		}
		if idIsSet(f.MapType.ValueID) {
			mt.ValueID = int(f.MapType.ValueID.ValueInt64())
		}
		return mt, nil
	case f.StructType != nil:
		subFields := make([]iceberg.NestedField, 0, len(f.StructType.Fields))
		for _, sf := range f.StructType.Fields {
			sfType, err := parseIcebergType(sf.Type.ValueString())
			if err != nil {
				return nil, fmt.Errorf("struct_type field %q: %w", sf.Name.ValueString(), err)
			}
			nf := iceberg.NestedField{
				Name:     sf.Name.ValueString(),
				Type:     sfType,
				Required: sf.Required.ValueBool(),
				Doc:      sf.Doc.ValueString(),
			}
			if idIsSet(sf.ID) {
				nf.ID = int(sf.ID.ValueInt64())
			}
			subFields = append(subFields, nf)
		}
		return &iceberg.StructType{FieldList: subFields}, nil
	default:
		return nil, fmt.Errorf("no type specified")
	}
}



// idIsSet returns true when an optional+computed Int64 attribute was explicitly
// provided by the user (i.e., neither null nor unknown).
func idIsSet(v types.Int64) bool {
	return !v.IsNull() && !v.IsUnknown()
}

// resolveNestedIDs either validates user-provided nested type IDs (uniqueness,
// completeness) or auto-assigns them sequentially when none are set.
// Top-level fields occupy IDs 1..len(fields). Nested IDs start from len(fields)+1.
func resolveNestedIDs(fields []FieldModel) ([]FieldModel, error) {
	// Count total nested ID slots and how many are user-set.
	totalSlots := 0
	setSlots := 0
	for _, f := range fields {
		if f.ListType != nil && !f.ListType.ElementType.IsNull() {
			totalSlots++
			if idIsSet(f.ListType.ID) {
				setSlots++
			}
		}
		if f.MapType != nil && !f.MapType.KeyType.IsNull() {
			totalSlots += 2
			if idIsSet(f.MapType.KeyID) {
				setSlots++
			}
			if idIsSet(f.MapType.ValueID) {
				setSlots++
			}
		}
		if f.StructType != nil {
			for _, sf := range f.StructType.Fields {
				totalSlots++
				if idIsSet(sf.ID) {
					setSlots++
				}
			}
		}
	}

	if setSlots != 0 && setSlots != totalSlots {
		return nil, fmt.Errorf("nested type IDs: either all must be specified or none; got %d of %d set", setSlots, totalSlots)
	}

	// Validate map key_id/value_id pairing.
	for _, f := range fields {
		if f.MapType != nil && !f.MapType.KeyType.IsNull() {
			keySet := idIsSet(f.MapType.KeyID)
			valSet := idIsSet(f.MapType.ValueID)
			if keySet != valSet {
				return nil, fmt.Errorf("field %q map_type: key_id and value_id must both be set or both omitted", f.Name.ValueString())
			}
		}
	}

	if setSlots == totalSlots && totalSlots > 0 {
		return fields, validateNestedIDUniqueness(fields)
	}

	if totalSlots == 0 {
		return fields, nil
	}

	// Auto-assign: counter starts after top-level field IDs (1..N).
	counter := len(fields) + 1
	result := make([]FieldModel, len(fields))
	copy(result, fields)
	for i := range result {
		f := &result[i]
		if f.ListType != nil && !f.ListType.ElementType.IsNull() {
			lt := *f.ListType
			lt.ID = types.Int64Value(int64(counter))
			counter++
			f.ListType = &lt
		}
		if f.MapType != nil && !f.MapType.KeyType.IsNull() {
			mt := *f.MapType
			mt.KeyID = types.Int64Value(int64(counter))
			counter++
			mt.ValueID = types.Int64Value(int64(counter))
			counter++
			f.MapType = &mt
		}
		if f.StructType != nil {
			st := *f.StructType
			newSFs := make([]StructSubFieldModel, len(st.Fields))
			copy(newSFs, st.Fields)
			for j := range newSFs {
				newSFs[j].ID = types.Int64Value(int64(counter))
				counter++
			}
			st.Fields = newSFs
			f.StructType = &st
		}
	}
	return result, nil
}

// validateNestedIDUniqueness checks that all explicitly set nested type IDs
// are unique across the entire schema (including top-level field IDs).
func validateNestedIDUniqueness(fields []FieldModel) error {
	seen := make(map[int64]string)
	for i, f := range fields {
		id := int64(i + 1)
		seen[id] = f.Name.ValueString()
	}
	for _, f := range fields {
		if f.ListType != nil && !f.ListType.ElementType.IsNull() {
			id := f.ListType.ID.ValueInt64()
			if prev, ok := seen[id]; ok {
				return fmt.Errorf("duplicate nested type ID %d (field %q list_type conflicts with %q)", id, f.Name.ValueString(), prev)
			}
			seen[id] = f.Name.ValueString() + ".list_type"
		}
		if f.MapType != nil && !f.MapType.KeyType.IsNull() {
			kid := f.MapType.KeyID.ValueInt64()
			vid := f.MapType.ValueID.ValueInt64()
			if kid == vid {
				return fmt.Errorf("field %q map_type: key_id and value_id must differ (both %d)", f.Name.ValueString(), kid)
			}
			for _, pair := range [][2]any{{kid, "key_id"}, {vid, "value_id"}} {
				id, label := pair[0].(int64), pair[1].(string)
				if prev, ok := seen[id]; ok {
					return fmt.Errorf("duplicate nested type ID %d (%s of field %q conflicts with %q)", id, label, f.Name.ValueString(), prev)
				}
				seen[id] = fmt.Sprintf("%s.map_type.%s", f.Name.ValueString(), label)
			}
		}
		if f.StructType != nil {
			for _, sf := range f.StructType.Fields {
				id := sf.ID.ValueInt64()
				if prev, ok := seen[id]; ok {
					return fmt.Errorf("duplicate nested type ID %d (struct sub-field %q of field %q conflicts with %q)", id, sf.Name.ValueString(), f.Name.ValueString(), prev)
				}
				seen[id] = fmt.Sprintf("%s.struct_type.%s", f.Name.ValueString(), sf.Name.ValueString())
			}
		}
	}
	return nil
}

// toNestedField converts a FieldModel to an iceberg NestedField.
// fieldID is the 1-based ID for the top-level field.
// resolveNestedIDs must be called before this to populate all nested IDs.
func (f *FieldModel) toNestedField(fieldID int) (*iceberg.NestedField, error) {
	var (
		typ iceberg.Type
		err error
	)

	switch {
	case !f.Type.IsNull() && !f.Type.IsUnknown():
		typ, err = parseIcebergType(f.Type.ValueString())
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", f.Name.ValueString(), err)
		}

	case f.ListType != nil && !f.ListType.ElementType.IsNull():
		elemType, err := parseIcebergType(f.ListType.ElementType.ValueString())
		if err != nil {
			return nil, fmt.Errorf("field %q list_type: %w", f.Name.ValueString(), err)
		}
		typ = &iceberg.ListType{
			ElementID:       int(f.ListType.ID.ValueInt64()),
			Element:         elemType,
			ElementRequired: f.ListType.Required.ValueBool(),
		}

	case f.MapType != nil && !f.MapType.KeyType.IsNull():
		keyType, err := parseIcebergType(f.MapType.KeyType.ValueString())
		if err != nil {
			return nil, fmt.Errorf("field %q map_type key: %w", f.Name.ValueString(), err)
		}
		valType, err := parseIcebergType(f.MapType.ValueType.ValueString())
		if err != nil {
			return nil, fmt.Errorf("field %q map_type value: %w", f.Name.ValueString(), err)
		}
		typ = &iceberg.MapType{
			KeyID:         int(f.MapType.KeyID.ValueInt64()),
			KeyType:       keyType,
			ValueID:       int(f.MapType.ValueID.ValueInt64()),
			ValueType:     valType,
			ValueRequired: f.MapType.Required.ValueBool(),
		}

	case f.StructType != nil:
		subFields := make([]iceberg.NestedField, 0, len(f.StructType.Fields))
		for _, sf := range f.StructType.Fields {
			sfType, err := parseIcebergType(sf.Type.ValueString())
			if err != nil {
				return nil, fmt.Errorf("field %q struct_type.field %q: %w", f.Name.ValueString(), sf.Name.ValueString(), err)
			}
			subFields = append(subFields, iceberg.NestedField{
				ID:       int(sf.ID.ValueInt64()),
				Name:     sf.Name.ValueString(),
				Type:     sfType,
				Required: sf.Required.ValueBool(),
				Doc:      sf.Doc.ValueString(),
			})
		}
		typ = &iceberg.StructType{FieldList: subFields}

	default:
		return nil, fmt.Errorf("field %q: exactly one of type, list_type, map_type, struct_type must be set", f.Name.ValueString())
	}

	var dv any
	if !f.Type.IsNull() && !f.Type.IsUnknown() {
		dv, err = f.getFieldDefault()
		if err != nil {
			return nil, err
		}
	}

	return &iceberg.NestedField{
		ID:             fieldID,
		Name:           f.Name.ValueString(),
		Type:           typ,
		Required:       f.Required.ValueBool(),
		InitialDefault: dv,
		WriteDefault:   dv,
		Doc:            f.Doc.ValueString(),
	}, nil
}

// getFieldDefault validates that at most one of default_string, default_number, or
// default_bool is set and returns the corresponding Go-native value for the field's
// Iceberg type. Returns nil when no default is configured.
func (f *FieldModel) getFieldDefault() (any, error) {
	// Nested type fields cannot have defaults.
	if f.Type.IsNull() || f.Type.IsUnknown() {
		if !f.DefaultString.IsNull() || !f.DefaultNumber.IsNull() || !f.DefaultBool.IsNull() {
			return nil, fmt.Errorf("field %q: defaults are not supported for nested types (list_type, map_type, struct_type)", f.Name.ValueString())
		}
		return nil, nil
	}

	default_count := 0
	if !f.DefaultString.IsNull() && !f.DefaultString.IsUnknown() {
		default_count++
	}
	if !f.DefaultNumber.IsNull() && !f.DefaultNumber.IsUnknown() {
		default_count++
	}
	if !f.DefaultBool.IsNull() && !f.DefaultBool.IsUnknown() {
		default_count++
	}

	if default_count == 0 {
		return nil, nil
	}
	if default_count > 1 {
		return nil, fmt.Errorf("multiple default values set for field %s", f.Name)
	}

	switch typ := f.Type.ValueString(); typ {
	case "boolean":
		if f.DefaultBool.IsNull() || f.DefaultBool.IsUnknown() {
			return nil, fmt.Errorf("non-boolean default set for boolean field %s", f.Name)
		}
		return f.DefaultBool.ValueBool(), nil
	case "int", "long":
		if f.DefaultNumber.IsNull() || f.DefaultNumber.IsUnknown() {
			return nil, fmt.Errorf("non-number default set for integer field %s", f.Name)
		}
		i64, acc := f.DefaultNumber.ValueBigFloat().Int64()
		if acc != 0 {
			return nil, fmt.Errorf("non-number default set for integer field %s", f.Name)
		}
		if typ == "long" {
			return i64, nil
		} else {
			return int32(i64), nil
		}
	case "float", "double":
		if f.DefaultNumber.IsNull() || f.DefaultNumber.IsUnknown() {
			return nil, fmt.Errorf("non-number default set for float field %s", f.Name)
		}
		f64, _ := f.DefaultNumber.ValueBigFloat().Float64()
		if typ == "double" {
			return f64, nil
		} else {
			return float32(f64), nil
		}
	case "string":
		if f.DefaultString.IsNull() || f.DefaultString.IsUnknown() {
			return nil, fmt.Errorf("non-string default set for string field %s", f.Name)
		}
		return f.DefaultString.ValueString(), nil
	default:
		return nil, fmt.Errorf("unsupported default type: %s", typ)
	}
}

// anyToIcebergLit converts a Go-native default value to the Iceberg Literal required
// by the schema API. Returns nil for a nil input (no default configured).
func anyToIcebergLit(typ string, d any) (iceberg.Literal, error) {
	if d == nil {
		// option not specified
		return nil, nil
	}
	switch typ {
	case "boolean":
		b, ok := d.(bool)
		if !ok {
			return nil, fmt.Errorf("non-boolean value %v", d)
		} else {
			return iceberg.BoolLiteral(b), nil
		}
	case "int":
		i32, ok := d.(int32)
		if !ok {
			return nil, fmt.Errorf("non-integer value %v", d)
		} else {
			return iceberg.Int32Literal(i32), nil
		}
	case "long":
		i64, ok := d.(int64)
		if !ok {
			return nil, fmt.Errorf("non-integer value %v", d)
		} else {
			return iceberg.Int64Literal(i64), nil
		}
	case "float":
		f32, ok := d.(float32)
		if !ok {
			return nil, fmt.Errorf("non-float value %v", d)
		} else {
			return iceberg.Float32Literal(f32), nil
		}
	case "double":
		f64, ok := d.(float64)
		if !ok {
			return nil, fmt.Errorf("non-float value %v", d)
		} else {
			return iceberg.Float64Literal(f64), nil
		}
	case "string":
		s, ok := d.(string)
		if !ok {
			return nil, fmt.Errorf("non-string value %v", d)
		} else {
			return iceberg.StringLiteral(s), nil
		}
	default:
		return nil, fmt.Errorf("unsupported default value type: %v", d)
	}
}

// Retrieving state

// setModelFromTable - set model fields, partition spec, properties from iceberg table
func setModelFromTable(data *S3TableResourceModel, tbl *itable.Table) error {
	var err error
	version := strconv.Itoa(tbl.Metadata().Version())
	data.FormatVersion = types.StringValue(version)

	data.Fields, err = schemaToFieldModels(tbl.Schema())
	if err != nil {
		return err
	}
	data.Partitions = specToPartitionModels(tbl.Spec(), tbl.Schema())

	data.Properties = propertiesToPropertyModels(tbl.Properties(), ignorePropsSet(data.IgnoreProperties))
	return nil
}

// BuildSchema converts Terraform field models to an Iceberg schema.
func BuildSchema(fields []FieldModel) (*iceberg.Schema, error) {
	resolved, err := resolveNestedIDs(fields)
	if err != nil {
		return nil, err
	}
	nestedFields := make([]iceberg.NestedField, 0, len(resolved))
	for i, f := range resolved {
		nf, err := f.toNestedField(i + 1)
		if err != nil {
			return nil, err
		}
		nestedFields = append(nestedFields, *nf)
	}
	return iceberg.NewSchema(0, nestedFields...), nil
}

// BuildPartitionSpec converts Terraform partition models to an Iceberg PartitionSpec.
func BuildPartitionSpec(partitions []PartitionModel, schema *iceberg.Schema) (*iceberg.PartitionSpec, error) {
	if len(partitions) == 0 {
		return iceberg.UnpartitionedSpec, nil
	}

	opts := []iceberg.PartitionOption{iceberg.WithSpecID(0)}
	for _, p := range partitions {
		transform, err := iceberg.ParseTransform(p.Transform.ValueString())
		if err != nil {
			return nil, fmt.Errorf("partition %q: %w", p.Name.ValueString(), err)
		}
		opts = append(opts, iceberg.AddPartitionFieldByName(
			p.SourceName.ValueString(),
			p.Name.ValueString(),
			transform,
			schema,
			nil,
		))
	}
	spec, err := iceberg.NewPartitionSpecOpts(opts...)
	if err != nil {
		return nil, err
	}
	return &spec, nil
}

// BuildProperties converts Terraform properties models to Iceberg properties
func BuildProperties(props []PropertyModel, version string) (*iceberg.Properties, error) {
	if version != "2" && version != "3" {
		return nil, fmt.Errorf("unsupported Iceberg Format Version: %s", version)
	}
	iproperties := make(iceberg.Properties)
	for _, prop := range props {
		iproperties[prop.Name.ValueString()] = prop.Value.ValueString()
	}
	// defaults added by s3tables:
	for name, val := range prop_defaults {
		if _, exists := iproperties[name]; !exists {
			iproperties[name] = val
		}
	}
	if version == "3" {
		iproperties["format-version"] = version
	}
	return &iproperties, nil
}

// icebergToFieldModel converts an Iceberg NestedField to a Terraform FieldModel,
// mapping the write-default value to the appropriate default_string, default_number,
// or default_bool attribute.
func icebergToFieldModel(f *iceberg.NestedField) (FieldModel, error) {
	model := FieldModel{
		Name:          types.StringValue(f.Name),
		Type:          types.StringNull(),
		Required:      types.BoolValue(f.Required),
		DefaultString: types.StringNull(),
		DefaultNumber: types.NumberNull(),
		DefaultBool:   types.BoolNull(),
		Doc:           types.StringValue(f.Doc),
		ListType:      nil,
		MapType:       nil,
		StructType:    nil,
	}

	switch t := f.Type.(type) {
	case *iceberg.ListType:
		model.ListType = &ListTypeModel{
			ID:          types.Int64Value(int64(t.ElementID)),
			ElementType: types.StringValue(t.Element.String()),
			Required:    types.BoolValue(t.ElementRequired),
		}
	case *iceberg.MapType:
		model.MapType = &MapTypeModel{
			KeyID:    types.Int64Value(int64(t.KeyID)),
			ValueID:  types.Int64Value(int64(t.ValueID)),
			KeyType:  types.StringValue(t.KeyType.String()),
			ValueType: types.StringValue(t.ValueType.String()),
			Required: types.BoolValue(t.ValueRequired),
		}
	case *iceberg.StructType:
		subFields := make([]StructSubFieldModel, 0, len(t.FieldList))
		for _, sf := range t.FieldList {
			subFields = append(subFields, StructSubFieldModel{
				ID:       types.Int64Value(int64(sf.ID)),
				Name:     types.StringValue(sf.Name),
				Type:     types.StringValue(sf.Type.String()),
				Required: types.BoolValue(sf.Required),
				Doc:      types.StringValue(sf.Doc),
			})
		}
		model.StructType = &StructTypeModel{Fields: subFields}
	default:
		// Primitive type
		model.Type = types.StringValue(f.Type.String())
		val := f.WriteDefault
		if val != nil {
			switch f.Type.String() {
			case "boolean":
				var b bool
				switch v := val.(type) {
				case bool:
					b = v
				default:
					return FieldModel{}, fmt.Errorf("type mismatch: %v not of type boolean, (type %s)", val, reflect.TypeOf(val))
				}
				model.DefaultBool = types.BoolValue(b)
			case "int", "long", "float", "double":
				var f64 float64
				switch v := val.(type) {
				case float64:
					f64 = v
				default:
					return FieldModel{}, fmt.Errorf("type mismatch: %v not of numeric type (type %s)", val, reflect.TypeOf(val))
				}
				model.DefaultNumber = types.NumberValue(big.NewFloat(f64))
			case "string":
				s, ok := val.(string)
				if !ok {
					return FieldModel{}, fmt.Errorf("type mismatch: %v not of type string, (type %s)", val, reflect.TypeOf(val))
				}
				model.DefaultString = types.StringValue(s)
			default:
				return FieldModel{}, fmt.Errorf("unsupported default value %v", val)
			}
		}
	}
	return model, nil
}

// schemaToFieldModels maps an Iceberg schema back to Terraform field models.
func schemaToFieldModels(schema *iceberg.Schema) ([]FieldModel, error) {
	fields := schema.Fields()
	models := make([]FieldModel, 0, len(fields))
	for _, f := range fields {
		m, err := icebergToFieldModel(&f)
		if err != nil {
			return models, err
		}
		models = append(models, m)
	}
	return models, nil
}

// specToPartitionModels maps an Iceberg PartitionSpec back to Terraform partition models.
func specToPartitionModels(spec iceberg.PartitionSpec, schema *iceberg.Schema) []PartitionModel {
	var models []PartitionModel
	for _, pf := range spec.Fields() {
		sourceField, ok := schema.FindFieldByID(pf.SourceID())
		sourceName := ""
		if ok {
			sourceName = sourceField.Name
		}
		models = append(models, PartitionModel{
			SourceName: types.StringValue(sourceName),
			Transform:  types.StringValue(pf.Transform.String()),
			Name:       types.StringValue(pf.Name),
		})
	}
	return models
}

// ignorePropsSet converts a types.List of property names into a lookup set.
// Returns nil (safe for lookup) when the list is null or unknown.
func ignorePropsSet(ignore types.List) map[string]struct{} {
	if ignore.IsNull() || ignore.IsUnknown() {
		return nil
	}
	result := make(map[string]struct{}, len(ignore.Elements()))
	for _, v := range ignore.Elements() {
		if s, ok := v.(types.String); ok && !s.IsNull() && !s.IsUnknown() {
			result[s.ValueString()] = struct{}{}
		}
	}
	return result
}

// propertiesToPropertyModels converts Iceberg table properties to Terraform models,
// filtering out format-version, built-in prop_defaults, systemManagedProps, and any
// keys in extraIgnore (from the resource's ignore_properties field).
func propertiesToPropertyModels(props iceberg.Properties, extraIgnore map[string]struct{}) []PropertyModel {
	models := make([]PropertyModel, 0)

	prop_names := make([]string, 0)
	for name := range props {
		prop_names = append(prop_names, name)
	}
	sort.Strings(prop_names)

	for _, name := range prop_names {
		if name == "format-version" {
			continue
		}
		if _, ignored := systemManagedProps[name]; ignored {
			continue
		}
		if _, ignored := extraIgnore[name]; ignored {
			continue
		}
		if dv, exists := prop_defaults[name]; !exists || props[name] != dv {
			models = append(models, PropertyModel{
				Name:  types.StringValue(name),
				Value: types.StringValue(props[name]),
				Type:  types.StringValue("text"),
			})
		}
	}
	return models
}

// Applying changes

// schemaUpdater, partitionUpdater, tableTransaction are thin interfaces over the
// iceberg-go concrete types so that Apply* functions can be tested without a
// real catalog connection.
type schemaUpdater interface {
	AddColumn(path []string, fieldType iceberg.Type, doc string, required bool, defaultValue iceberg.Literal) *itable.UpdateSchema
	DeleteColumn(path []string) *itable.UpdateSchema
	UpdateColumn(path []string, update itable.ColumnUpdate) *itable.UpdateSchema
	Commit() error
}

type partitionUpdater interface {
	AddField(sourceColName string, transform iceberg.Transform, partitionFieldName string) *itable.UpdateSpec
	RemoveField(name string) *itable.UpdateSpec
	Commit() error
}

type tableTransaction interface {
	UpdateSchema(caseSensitive, allowIncompatibleChanges bool) schemaUpdater
	UpdateSpec(caseSensitive bool) partitionUpdater
}

// txnAdapter wraps *itable.Transaction to satisfy tableTransaction.
type txnAdapter struct{ t *itable.Transaction }

// UpdateSchema delegates to the underlying transaction, satisfying the tableTransaction interface.
func (a *txnAdapter) UpdateSchema(caseSensitive, allowIncompatible bool) schemaUpdater {
	return a.t.UpdateSchema(caseSensitive, allowIncompatible)
}

// UpdateSpec delegates to the underlying transaction, satisfying the tableTransaction interface.
func (a *txnAdapter) UpdateSpec(caseSensitive bool) partitionUpdater {
	return a.t.UpdateSpec(caseSensitive)
}

// ApplySchemaChanges computes the diff between state and plan fields and applies
// add/delete/update operations to the transaction.
func ApplySchemaChanges(txn tableTransaction, stateFields, planFields []FieldModel) error {

	// Build a map of current Iceberg fields by name.
	current := make(map[string]FieldModel)
	for _, f := range stateFields {
		current[f.Name.ValueString()] = f
	}

	// Build a map of plan fields by name.
	plan := make(map[string]FieldModel)
	for _, f := range planFields {
		plan[f.Name.ValueString()] = f
	}

	// Detect any changes that require an UpdateSchema call.
	hasChanges := false
	for name, pf := range plan {
		if cf, exists := current[name]; !exists || !fieldModelsEqual(cf, pf) {
			hasChanges = true
			break
		}
	}
	if !hasChanges {
		for name := range current {
			if _, exists := plan[name]; !exists {
				hasChanges = true
				break
			}
		}
	}
	if !hasChanges {
		return nil
	}

	updater := txn.UpdateSchema(true, false)

	// Delete columns that are in current but not in plan.
	for name := range current {
		if _, exists := plan[name]; !exists {
			updater.DeleteColumn([]string{name})
		}
	}

	// Add columns that are in plan but not in current.
	// Update columns for existing columns which have changed
	for name, pf := range plan {
		if cf, exists := current[name]; !exists || !fieldModelsEqual(cf, pf) {
			typ, err := fieldModelToIcebergType(pf)
			if err != nil {
				return fmt.Errorf("field %q: %w", name, err)
			}
			dv, err := pf.getFieldDefault()
			if err != nil {
				return fmt.Errorf("field %q: %w", name, err)
			}
			var dvlit iceberg.Literal
			if !pf.Type.IsNull() {
				dvlit, err = anyToIcebergLit(pf.Type.ValueString(), dv)
				if err != nil {
					return fmt.Errorf("field %q: %w", name, err)
				}
			}
			if !exists {
				updater.AddColumn([]string{name}, typ, pf.Doc.ValueString(), pf.Required.ValueBool(), dvlit)
			} else {
				updater.UpdateColumn([]string{name}, itable.ColumnUpdate{
					FieldType:    iceberg.Optional[iceberg.Type]{Valid: true, Val: typ},
					Doc:          iceberg.Optional[string]{Valid: true, Val: pf.Doc.ValueString()},
					Required:     iceberg.Optional[bool]{Valid: true, Val: pf.Required.ValueBool()},
					WriteDefault: iceberg.Optional[iceberg.Literal]{Valid: true, Val: dvlit},
				})
			}
		}
	}

	return updater.Commit()
}

// applyPartitionChanges computes the diff between the current spec and the plan
// and applies add/remove operations to the transaction.
func ApplyPartitionChanges(txn tableTransaction, statePartitions, planPartitions []PartitionModel) error {
	// Build a map of current partition fields by name.
	current := make(map[string]PartitionModel)
	for _, p := range statePartitions {
		current[p.Name.ValueString()] = p
	}

	// Build a set of plan partition field names.
	plan := make(map[string]PartitionModel)
	for _, p := range planPartitions {
		plan[p.Name.ValueString()] = p
	}

	// check for changes
	hasChanges := len(current) != len(plan)
	if !hasChanges {
		for name, pp := range plan {
			if sp, exists := current[name]; !exists || pp != sp {
				hasChanges = true
				break
			}
		}
	}
	if !hasChanges {
		for name := range current {
			if _, exists := plan[name]; !exists {
				hasChanges = true
				break
			}
		}
	}
	if !hasChanges {
		return nil
	}

	updater := txn.UpdateSpec(true)

	// Remove partition fields that are in current but not in plan or that have changed.
	for name, cp := range current {
		if pp, exists := plan[name]; !exists || cp != pp {
			updater.RemoveField(name)
		}
	}

	// Add partition fields that are in plan but not in current, or that have changed.
	for name, pp := range plan {
		if cp, exists := current[name]; !exists || cp != pp {
			transform, err := iceberg.ParseTransform(pp.Transform.ValueString())
			if err != nil {
				return fmt.Errorf("partition %q: %w", name, err)
			}
			updater.AddField(pp.SourceName.ValueString(), transform, name)
		}
	}

	return updater.Commit()
}

// filterIgnoredProps removes entries whose name is in systemManagedProps or extraIgnore
// from a property slice. Filtering systemManagedProps here (in addition to
// propertiesToPropertyModels) ensures that if a user has explicitly declared a
// system-managed key in a property block, the plan-side copy is also dropped so
// checkPropChanges does not treat the state/plan asymmetry as a real change.
func filterIgnoredProps(props []PropertyModel, extraIgnore map[string]struct{}) []PropertyModel {
	result := make([]PropertyModel, 0, len(props))
	for _, p := range props {
		name := p.Name.ValueString()
		if _, ignored := systemManagedProps[name]; ignored {
			continue
		}
		if _, ignored := extraIgnore[name]; ignored {
			continue
		}
		result = append(result, p)
	}
	return result
}

// checkPropChanges returns an error when the plan properties differ from state after
// filtering out entries listed in extraIgnore (the resource's ignore_properties field).
// Filtering both sides handles the transition case where an ignored property is still
// present in state from a previous Read that ran before ignore_properties was set.
// Table property updates are not supported by the Iceberg catalog; on mismatch the
// error message includes the filtered state properties as copy-pasteable HCL blocks.
func checkPropChanges(stateProps, planProps []PropertyModel, extraIgnore map[string]struct{}) error {
	stateProps = filterIgnoredProps(stateProps, extraIgnore)
	planProps = filterIgnoredProps(planProps, extraIgnore)

	current := make(map[string]PropertyModel)
	for _, p := range stateProps {
		current[p.Name.ValueString()] = p
	}
	plan := make(map[string]PropertyModel)
	for _, p := range planProps {
		plan[p.Name.ValueString()] = p
	}

	mismatch := len(current) != len(plan)
	if !mismatch {
		for name, pp := range plan {
			sp, exists := current[name]
			if !exists {
				mismatch = true
				break
			}
			if err := checkPropValueEqual(name, sp.Value.ValueString(), pp.Value.ValueString(), pp.Type.ValueString()); err != nil {
				mismatch = true
				break
			}
		}
	}
	if !mismatch {
		for name := range current {
			if _, exists := plan[name]; !exists {
				mismatch = true
				break
			}
		}
	}
	if !mismatch {
		return nil
	}
	return propertyMismatchErr(stateProps)
}

// propertyMismatchErr builds a human-friendly error for property mismatches that
// includes the current table properties as copy-pasteable HCL so the user can
// reconcile their configuration.
func propertyMismatchErr(stateProps []PropertyModel) error {
	var sb strings.Builder
	sb.WriteString("Table property changes are not supported.\n\n")
	if len(stateProps) == 0 {
		sb.WriteString("The table has no user-defined properties. Remove all property blocks from your configuration.")
		return fmt.Errorf("%s", sb.String())
	}
	sb.WriteString("Update your configuration to match the table's actual properties:\n\n")
	for _, p := range stateProps {
		fmt.Fprintf(&sb, "  property {\n    name  = %q\n    value = %q\n  }\n", p.Name.ValueString(), p.Value.ValueString())
	}
	return fmt.Errorf("%s", sb.String())
}

// checkPropValueEqual compares a state and plan property value.
// When planType is "json", strict JSON mode applies: both values must be valid
// JSON and are compared structurally (key order and whitespace ignored).
// For all other types, structural JSON comparison is attempted automatically;
// if either value is not valid JSON the comparison falls back to string equality.
func checkPropValueEqual(name, stateVal, planVal, planType string) error {
	if planType == "json" {
		var stateDecoded, planDecoded any
		if err := json.Unmarshal([]byte(stateVal), &stateDecoded); err != nil {
			return fmt.Errorf("property %q: state value is not valid JSON: %w", name, err)
		}
		if err := json.Unmarshal([]byte(planVal), &planDecoded); err != nil {
			return fmt.Errorf("property %q: plan value is not valid JSON: %w", name, err)
		}
		if !reflect.DeepEqual(stateDecoded, planDecoded) {
			return fmt.Errorf("differing property: %v", name)
		}
		return nil
	}
	// Auto-detect JSON: if both values parse as JSON, compare structurally so that
	// whitespace/key-order differences (e.g. schema.name-mapping.default written by
	// Athena) are not treated as changes.
	var stateDecoded, planDecoded any
	if json.Unmarshal([]byte(stateVal), &stateDecoded) == nil &&
		json.Unmarshal([]byte(planVal), &planDecoded) == nil {
		if !reflect.DeepEqual(stateDecoded, planDecoded) {
			return fmt.Errorf("differing property: %v", name)
		}
		return nil
	}
	if stateVal != planVal {
		return fmt.Errorf("differing property: %v", name)
	}
	return nil
}

// isNonPrimitiveUpdateErr returns true when the catalog has rejected an UpdateColumn
// call because the column has a non-primitive (list, map, struct) type.
func isNonPrimitiveUpdateErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "cannot update field type for non-primitive type")
}

// nestedTypeUpdateErrMsg builds a human-friendly error for the non-primitive update
// case. It includes the current table schema with all Iceberg-assigned IDs so the
// user can update their configuration with explicit id attributes and avoid the diff.
func nestedTypeUpdateErrMsg(origErr error, stateFields []FieldModel) string {
	var sb strings.Builder
	sb.WriteString(origErr.Error())
	sb.WriteString("\n\n")
	sb.WriteString("The Iceberg catalog cannot change the type of an existing list, map, or struct column.\n")
	sb.WriteString("This usually means the provider assigned different nested-type IDs than the catalog originally chose.\n\n")
	sb.WriteString("Add explicit id attributes to your configuration matching the values below, then re-run terraform apply:\n\n")

	for i, f := range stateFields {
		topID := i + 1
		switch {
		case f.ListType != nil:
			fmt.Fprintf(&sb, "  field {\n    name = %q\n    list_type {\n      id       = %d\n      type     = %q\n      required = %v\n    }\n  }\n",
				f.Name.ValueString(),
				f.ListType.ID.ValueInt64(),
				f.ListType.ElementType.ValueString(),
				f.ListType.Required.ValueBool(),
			)
		case f.MapType != nil:
			fmt.Fprintf(&sb, "  field {\n    name = %q\n    map_type {\n      key_id         = %d\n      value_id       = %d\n      key_type       = %q\n      value_type     = %q\n      required       = %v\n    }\n  }\n",
				f.Name.ValueString(),
				f.MapType.KeyID.ValueInt64(),
				f.MapType.ValueID.ValueInt64(),
				f.MapType.KeyType.ValueString(),
				f.MapType.ValueType.ValueString(),
				f.MapType.Required.ValueBool(),
			)
		case f.StructType != nil:
			fmt.Fprintf(&sb, "  field {\n    name = %q\n    struct_type {\n", f.Name.ValueString())
			for _, sf := range f.StructType.Fields {
				fmt.Fprintf(&sb, "      field {\n        id       = %d\n        name     = %q\n        type     = %q\n        required = %v\n      }\n",
					sf.ID.ValueInt64(),
					sf.Name.ValueString(),
					sf.Type.ValueString(),
					sf.Required.ValueBool(),
				)
			}
			sb.WriteString("    }\n  }\n")
		default:
			fmt.Fprintf(&sb, "  # field %d: %s (%s)\n", topID, f.Name.ValueString(), f.Type.ValueString())
		}
	}
	return sb.String()
}

// parseIcebergType converts a type string to an iceberg.Type.
func parseIcebergType(s string) (iceberg.Type, error) {
	switch s {
	case "boolean":
		return iceberg.PrimitiveTypes.Bool, nil
	case "int":
		return iceberg.PrimitiveTypes.Int32, nil
	case "long":
		return iceberg.PrimitiveTypes.Int64, nil
	case "float":
		return iceberg.PrimitiveTypes.Float32, nil
	case "double":
		return iceberg.PrimitiveTypes.Float64, nil
	case "date":
		return iceberg.PrimitiveTypes.Date, nil
	case "time":
		return iceberg.PrimitiveTypes.Time, nil
	case "timestamp":
		return iceberg.PrimitiveTypes.Timestamp, nil
	case "timestamptz":
		return iceberg.PrimitiveTypes.TimestampTz, nil
	case "string":
		return iceberg.PrimitiveTypes.String, nil
	case "binary":
		return iceberg.PrimitiveTypes.Binary, nil
	case "uuid":
		return iceberg.PrimitiveTypes.UUID, nil
	}

	if strings.HasPrefix(s, "fixed[") && strings.HasSuffix(s, "]") {
		inner := s[len("fixed[") : len(s)-1]
		n, err := strconv.Atoi(inner)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid fixed type %q: length must be a positive integer", s)
		}
		return iceberg.FixedTypeOf(n), nil
	}

	if strings.HasPrefix(s, "decimal(") && strings.HasSuffix(s, ")") {
		inner := s[len("decimal(") : len(s)-1]
		inner = strings.ReplaceAll(inner, " ", "")
		parts := strings.SplitN(inner, ",", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid decimal type %q: expected decimal(P,S)", s)
		}
		precision, err1 := strconv.Atoi(parts[0])
		scale, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || precision <= 0 || scale < 0 {
			return nil, fmt.Errorf("invalid decimal type %q: precision and scale must be non-negative integers", s)
		}
		return iceberg.DecimalTypeOf(precision, scale), nil
	}

	return nil, fmt.Errorf("unsupported type %q: use boolean, int, long, float, double, date, time, timestamp, timestamptz, string, binary, uuid, fixed[N], or decimal(P,S)", s)
}

// isNotFound returns true when the Glue catalog error indicates the resource does not exist.
func isNotFound(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "EntityNotFoundException") ||
		strings.Contains(msg, "NoSuchObjectException") ||
		strings.Contains(msg, "does not exist")
}
