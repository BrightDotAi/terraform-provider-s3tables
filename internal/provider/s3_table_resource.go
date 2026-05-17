// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	iceberg "github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/catalog"
	"github.com/apache/iceberg-go/catalog/rest"
	itable "github.com/apache/iceberg-go/table"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

var _ resource.Resource = &S3TableResource{}
var _ resource.ResourceWithImportState = &S3TableResource{}

func NewS3TableResource() resource.Resource {
	return &S3TableResource{}
}

// S3TableResource defines the resource implementation.
type S3TableResource struct {
	awsCfg aws.Config
}

// S3TableResourceModel describes the resource data model.
type S3TableResourceModel struct {
	Warehouse         types.String     `tfsdk:"warehouse"`
	Region			  types.String	   `tfsdk:"region"`
	Namespace         types.String     `tfsdk:"namespace"`
	Name              types.String     `tfsdk:"name"`
	Fields            []FieldModel     `tfsdk:"field"`
	Partitions        []PartitionModel `tfsdk:"partition"`
}

// FieldModel represents one column in the Iceberg schema.
type FieldModel struct {
	Name     types.String `tfsdk:"name"`
	Type     types.String `tfsdk:"type"`
	Required types.Bool   `tfsdk:"required"`
	Default  types.Dynamic `tfsdk:"default"`
	Doc      types.String `tfsdk:"doc"`
}

// PartitionModel represents one field in the Iceberg partition spec.
type PartitionModel struct {
	SourceName types.String `tfsdk:"source_name"`
	Transform  types.String `tfsdk:"transform"`
	Name       types.String `tfsdk:"name"`
}

func (r *S3TableResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_s3_table"
}

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
		},
		Blocks: map[string]schema.Block{
			"field": schema.ListNestedBlock{
				MarkdownDescription: "Iceberg schema column.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							MarkdownDescription: "Column name.",
							Required:            true,
						},
						"type": schema.StringAttribute{
							MarkdownDescription: "Iceberg type: `boolean`, `int`, `long`, `float`, `double`, `date`, `time`, `timestamp`, `timestamptz`, `string`, `binary`, `uuid`, `fixed[N]`, `decimal(P,S)`.",
							Required:            true,
						},
						"required": schema.BoolAttribute{
							MarkdownDescription: "Whether the column is non-nullable. Defaults to `false`.",
							Optional:            true,
							Computed:            true,
							Default:             booldefault.StaticBool(false),
						},
						"default": schema.DynamicAttribute{
							MarkdownDescription: "Default value for column",
							Optional:            true,
							Computed:            true,
						},
						"doc": schema.StringAttribute{
							MarkdownDescription: "Documentation string for the column.",
							Optional:            true,
							Computed:            true,
							Default:             stringdefault.StaticString(""),
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
		},
	}
}

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

	cat, err := data.GetCatalog(ctx, r.awsCfg)
	if err != nil {
		resp.Diagnostics.AddError("Error Connecting to Iceberg Catalog", err.Error())
		return
	}

	identifier := data.GetIdentifier()

	tbl, err := cat.CreateTable(ctx, identifier, icebergSchema,
		catalog.WithPartitionSpec(partSpec),
	)
	if err != nil {
		resp.Diagnostics.AddError("Error creating Iceberg table", err.Error())
		return
	}

	data.Fields, err = schemaToFieldModels(tbl.Schema())
	if err != nil {
		resp.Diagnostics.AddError("Error converting iceberg fields", err.Error())
		return
	}
	data.Partitions = specToPartitionModels(tbl.Spec(), tbl.Schema())

	tflog.Trace(ctx, "created Iceberg table", map[string]any{
		"warehouse": data.Warehouse.ValueString(),
		"namespace": data.Namespace.ValueString(),
		"name":      data.Name.ValueString(),
	})
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}


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

	data.Fields, err = schemaToFieldModels(tbl.Schema())
	if err != nil {
		resp.Diagnostics.AddError("Error reading Iceberg fields", err.Error())
		return
	}
	data.Partitions = specToPartitionModels(tbl.Spec(), tbl.Schema())

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

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

	err = ApplySchemaChanges(txn, state.Fields, plan.Fields)
	if err != nil {
		resp.Diagnostics.AddError("Error updating schema", err.Error())
		return
	}

	err = ApplyPartitionChanges(txn, state.Partitions, plan.Partitions)
	if err != nil {
		resp.Diagnostics.AddError("Error updating partition spec", err.Error())
		return
	}

	updated, err := txn.Commit(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Error committing table changes", err.Error())
		return
	}

	plan.Fields, err  = schemaToFieldModels(updated.Schema())
	if err != nil {
		resp.Diagnostics.AddError("Error reading iceberg fields", err.Error())
		return
	}

	plan.Partitions = specToPartitionModels(updated.Spec(), updated.Schema())

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}


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

	err = cat.DropTable(ctx, identifier)
	if err != nil {
		if !isNotFound(err) {
			resp.Diagnostics.AddError("Error deleting Iceberg table", err.Error())
		}
	}
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
func (data *S3TableResourceModel) GetCatalog(ctx context.Context, awsCfg aws.Config) (*rest.Catalog, error){
	cat, err := rest.NewCatalog(ctx, "s3tables_catalog",
						   ("https://glue." + data.Region.ValueString() + ".amazonaws.com/iceberg"),
						   rest.WithAwsConfig(awsCfg),
						   rest.WithWarehouseLocation(data.Warehouse.ValueString()),
						   rest.WithSigV4(),
						   rest.WithSigV4RegionSvc(data.Region.ValueString(), "glue"))
	return cat, err
}

// GetIdentifier - get Identifier from table model
func (data *S3TableResourceModel) GetIdentifier() (itable.Identifier) {
	return catalog.ToIdentifier(data.Namespace.ValueString(), data.Name.ValueString())
}

// toNestedField - Convert a FieldModel record to an iceberg nested field
func (f *FieldModel) toNestedField(id int) (*iceberg.NestedField, error) {
	typ, err := parseIcebergType(f.Type.ValueString())
	if err != nil {
		return nil, fmt.Errorf("field %q: %w", f.Name.ValueString(), err)
	}
	dv, err := dynamicValueToDefault(f.Default)
	if err != nil {
		return nil, err
	}
	nestedField := iceberg.NestedField{
		ID:       id + 1,
		Name:     f.Name.ValueString(),
		Type:     typ,
		Required: f.Required.ValueBool(),
		InitialDefault: dv.Any(),
		WriteDefault: dv.Any(),
		Doc:      f.Doc.ValueString(),
	}
	return &nestedField, nil
}

func dynamicValueToDefault(d types.Dynamic) (iceberg.Literal, error) {
	if d.IsNull() {
		// option not specified
		return nil, nil
	}
	switch value := d.UnderlyingValue().(type) {
    case types.Bool:
		return iceberg.BoolLiteral(value.ValueBool()), nil
	case types.Float64:
		return iceberg.Float64Literal(value.ValueFloat64()), nil
	case types.Float32:
		return iceberg.Float32Literal(value.ValueFloat32()), nil
	case types.Int64:
		return iceberg.Int64Literal(value.ValueInt64()), nil
	case types.Int32:
		return iceberg.Int32Literal(value.ValueInt32()), nil
    case types.String:
		return iceberg.StringLiteral(value.ValueString()), nil
	default:
		return nil, fmt.Errorf("Unsupported default value type: %v", value)
	}
}

// BuildSchema converts Terraform field models to an Iceberg schema.
func BuildSchema(fields []FieldModel) (*iceberg.Schema, error) {
	nestedFields := make([]iceberg.NestedField, 0, len(fields))
	for i, f := range fields {
		nf, err := f.toNestedField(i)
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

// schemaToFieldModels maps an Iceberg schema back to Terraform field models.
func schemaToFieldModels(schema *iceberg.Schema) ([]FieldModel, error) {
	fields := schema.Fields()
	models := make([]FieldModel, 0, len(fields))
	for _, f := range fields {
		dv, err := anyToDynamicValue(f.Type.String(), f.WriteDefault)
		if 	err != nil {
			return nil, err
		}
		models = append(models, FieldModel{
			Name:     types.StringValue(f.Name),
			Type:     types.StringValue(f.Type.String()),
			Required: types.BoolValue(f.Required),
			Default:  dv,
			Doc:      types.StringValue(f.Doc),
		})
	}
	return models, nil
}

func anyToDynamicValue(typ string, val any) (types.Dynamic, error) {
	if val == nil {
		return types.DynamicNull(), nil
	}
	var tv attr.Value
	switch typ {
	case "boolean":
		tv = types.BoolValue(val.(bool))
	case "int":
		tv = types.Int32Value(val.(int32))
	case "long":
		tv = types.Int64Value(val.(int64))
	case "float":
		tv = types.Float32Value(val.(float32))
	case "double":
		tv = types.Float64Value(val.(float64))
	case "string":
		tv = types.StringValue(val.(string))
	default:
		return types.DynamicNull(), fmt.Errorf("Unsupported default value %v", val)
	}
	return types.DynamicValue(tv), nil
}

// specToPartitionModels maps an Iceberg PartitionSpec back to Terraform partition models.
func specToPartitionModels(spec iceberg.PartitionSpec, schema *iceberg.Schema) []PartitionModel {
	var models []PartitionModel
	for pf := range spec.Fields() {
		sourceField, ok := schema.FindFieldByID(pf.SourceID)
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

// ApplySchemaChanges computes the diff between state and plan fields and applies
// add/delete/update operations to the transaction.
func ApplySchemaChanges(txn *itable.Transaction, stateFields, planFields []FieldModel) error {

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
		if cf, exists := current[name]; !exists || cf != pf {
			hasChanges = true
			break
		}
	}
	if !hasChanges {
		for name, _ := range current {
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
		if cf, exists := current[name]; !exists  || pf != cf {
			typ, err := parseIcebergType(pf.Type.ValueString())
			if err != nil {
				return fmt.Errorf("field %q: %w", name, err)
			}
			dv, err := dynamicValueToDefault(pf.Default)
			if err != nil {
				return fmt.Errorf("field %q: %w", name, err)
			}
			if !exists {
				updater.AddColumn([]string{name}, typ, pf.Doc.ValueString(), pf.Required.ValueBool(), dv)
			} else {
				updater.UpdateColumn([]string{name}, itable.ColumnUpdate{
					FieldType: iceberg.Optional[iceberg.Type]{Valid: true, Val: typ},
					Doc: iceberg.Optional[string]{Valid: true, Val: pf.Doc.ValueString()},
					Required: iceberg.Optional[bool]{Valid: true, Val: pf.Required.ValueBool()},
					WriteDefault: iceberg.Optional[iceberg.Literal]{Valid: true, Val: dv},
			})
			}
		}
	}

	return updater.Commit()
}

// applyPartitionChanges computes the diff between the current spec and the plan
// and applies add/remove operations to the transaction.
func ApplyPartitionChanges(txn *itable.Transaction, statePartitions, planPartitions []PartitionModel) error {
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
			if sp, exists := current[name]; !exists || pp != sp{
				hasChanges = true
				break
			}
		}
	}
	if !hasChanges {
		for name, _ := range current {
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
