// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sort"

	iceberg "github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/catalog"
	"github.com/apache/iceberg-go/catalog/rest"
	itable "github.com/apache/iceberg-go/table"
	"github.com/aws/aws-sdk-go-v2/aws"
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
	Properties		  []PropertyModel  `tfsdk:"property"`
}

// FieldModel represents one column in the Iceberg schema.
type FieldModel struct {
	Name     types.String `tfsdk:"name"`
	Type     types.String `tfsdk:"type"`
	Required types.Bool   `tfsdk:"required"`
	Default  types.String `tfsdk:"default"`
	Doc      types.String `tfsdk:"doc"`
}

// PartitionModel represents one field in the Iceberg partition spec.
type PartitionModel struct {
	SourceName types.String `tfsdk:"source_name"`
	Transform  types.String `tfsdk:"transform"`
	Name       types.String `tfsdk:"name"`
}

// PropertyModel represents one field in the Iceberg property spec.
type PropertyModel struct {
	Name types.String `tfsdk:"name"`
	Value types.String `tfsdk:"value"`
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
						"default": schema.StringAttribute{
							MarkdownDescription: "Default value for column. Int or float values will be parsed from string",
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

	properties, err := BuildProperties(data.Properties)
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
		resp.Diagnostics.AddError("Error updating schema", err.Error())
		return
	}

	err = ApplyPartitionChanges(&txnAdapter{txn}, state.Partitions, plan.Partitions)
	if err != nil {
		resp.Diagnostics.AddError("Error updating partition spec", err.Error())
		return
	}
	
	err = checkPropChanges(state.Properties, plan.Properties)
	if err != nil {
		resp.Diagnostics.AddError("Error - Table property changes not supported", err.Error())
		return
	}

	updated, err := txn.Commit(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Error committing table changes", err.Error())
		return
	}

	err = setModelFromTable(&plan, updated)
	if err != nil {
		resp.Diagnostics.AddError("Error reading iceberg fields", err.Error())
		return
	}

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

	err = cat.PurgeTable(ctx, identifier)
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
	dv, err := f.getFieldDefault()
	if err != nil {
		return nil, err
	}
	nestedField := iceberg.NestedField{
		ID:       id + 1,
		Name:     f.Name.ValueString(),
		Type:     typ,
		Required: f.Required.ValueBool(),
		InitialDefault: dv,
		WriteDefault: dv,
		Doc:      f.Doc.ValueString(),
	}
	return &nestedField, nil
}

func (f *FieldModel) getFieldDefault() (any, error) {
	if f.Default.IsNull() || f.Default.IsUnknown() {
		return nil, nil
	}
	ds := f.Default.ValueString()

	switch typ := f.Type.ValueString(); typ {
	case "boolean":
		switch ds {
		case "true", "True":
			return true, nil
		case "false", "False":
			return false, nil
		default:
			return nil, fmt.Errorf("Could not parse default to boolean: %s", ds)
		}
	case "int", "long":
		i, err := strconv.ParseInt(ds, 10, 64)
		if err != nil {
			return nil, err
		}
		if typ == "long" {
			return i, nil
		} else {
			return int32(i), nil
		}
	case "float", "double":
		f, err := strconv.ParseFloat(ds, 64)
		if err != nil {
			return nil, err
		}
		if typ == "double" {
			return f, nil
		} else {
			return float32(f), nil
		}
	case "string":
		return ds, nil
	default :
		return nil, fmt.Errorf("Unsupported default type: %s", typ)
	}
}


// Dynamic Values - for default value fields

func anyToIcebergLit(typ string, d any) (iceberg.Literal, error) {
	if d == nil {
		// option not specified
		return nil, nil
	}
	switch typ {
	case "boolean":
		b, ok := d.(bool)
		if !ok {
			return nil, fmt.Errorf("Non-boolean value %v", d)
		} else {
			return iceberg.BoolLiteral(b), nil
		}
	case "int":
		i32, ok := d.(int32)
		if !ok {
			return nil, fmt.Errorf("Non-integer value %v", d)
		} else {
			return iceberg.Int32Literal(i32), nil
		}
	case "long":
		i64, ok := d.(int64)
		if !ok {
			return nil, fmt.Errorf("Non-integer value %v", d)
		} else {
			return iceberg.Int64Literal(i64), nil
		}
	case "float":
		f32, ok := d.(float32)
		if !ok {
			return nil, fmt.Errorf("Non-float value %v", d)
		} else {
			return iceberg.Float32Literal(f32), nil
		}
	case "double":
		f64, ok := d.(float64)
		if !ok {
			return nil, fmt.Errorf("Non-float value %v", d)
		} else {
			return iceberg.Float64Literal(f64), nil
		}
	case "string":
		s, ok := d.(string)
		if !ok {
			return nil, fmt.Errorf("Non-string value %v", d)
		} else {
			return iceberg.StringLiteral(s), nil
		}
	default:
		return nil, fmt.Errorf("Unsupported default value type: %v", d)
	}
}

// Retrieving state

// setModelFromTable - set model fields, partition spec, properties from iceberg table
func setModelFromTable(data *S3TableResourceModel, tbl *itable.Table) (error) {
	var err error
	data.Fields, err = schemaToFieldModels(tbl.Schema())
	if err != nil {
		return err
	}
	data.Partitions = specToPartitionModels(tbl.Spec(), tbl.Schema())

	data.Properties = propertiesToPropertyModels(tbl.Properties())
	return nil
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

// BuildProperties converts Terraform properties models to Iceberg properties
func BuildProperties(props []PropertyModel) (*iceberg.Properties, error) {
	iproperties := make(iceberg.Properties)
	for _, prop := range props {
		iproperties[prop.Name.ValueString()] = prop.Value.ValueString()
	}
	return &iproperties, nil
}

// schemaToFieldModels maps an Iceberg schema back to Terraform field models.
func schemaToFieldModels(schema *iceberg.Schema) ([]FieldModel, error) {
	fields := schema.Fields()
	models := make([]FieldModel, 0, len(fields))
	for _, f := range fields {
		dv, err := anyToStringValue(f.Type.String(), f.WriteDefault)
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

// Convert a default value to Terraform String Value
func anyToStringValue(typ string, val any) (types.String, error) {
	if val == nil {
		return types.StringNull(), nil
	}
	switch typ {
	case "boolean":
		b, ok := val.(bool)
		if !ok {
			return types.StringNull(), fmt.Errorf("Type missmatch: %v not of type boolean", val)
		} else if b {
			return types.StringValue("true"), nil
		} else {
			return types.StringValue("false"), nil
		}
	case "int":
		i32, ok := val.(int32)
		if !ok {
			return types.StringNull(), fmt.Errorf("Type missmatch: %v not of type int", val)
		} else {
			return types.StringValue(strconv.Itoa(int(i32))), nil
		}
	case "long":
		i64, ok := val.(int64)
		if !ok {
			return types.StringNull(), fmt.Errorf("Type missmatch: %v not of type long", val)
		} else {
			return types.StringValue(strconv.FormatInt(i64, 10)), nil
		}
	case "float":
		f32, ok := val.(float32)
		if !ok {
			return types.StringNull(), fmt.Errorf("Type missmatch: %v not of type float", val)
		} else {
			return types.StringValue(strconv.FormatFloat(float64(f32), 'f', -1, 32)), nil
		}
	case "double":
		f64, ok := val.(float64)
		if !ok {
			return types.StringNull(), fmt.Errorf("Type missmatch: %v not of type float", val)
		} else {
			return types.StringValue(strconv.FormatFloat(f64, 'f', -1, 64)), nil
		}
	case "string":
		s, ok := val.(string)
		if !ok {
			return types.StringNull(), fmt.Errorf("Type missmatch: %v not of type string", val)
		} else {
			return types.StringValue(s), nil
		}
	default:
		return types.StringNull(), fmt.Errorf("Unsupported default value %v", val)
	}
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

// propertiesToPropertyModels
func propertiesToPropertyModels(props iceberg.Properties) []PropertyModel {
	models := make([]PropertyModel, 0)

	prop_names := make([]string, 0)
	for name, _ := range props {
		prop_names = append(prop_names, name)
	}
	sort.Strings(prop_names)

	for _, name := range prop_names {
		models = append(models, PropertyModel{
			Name:  types.StringValue(name),
			Value: types.StringValue(props[name]),
		})
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

func (a *txnAdapter) UpdateSchema(caseSensitive, allowIncompatible bool) schemaUpdater {
	return a.t.UpdateSchema(caseSensitive, allowIncompatible)
}

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
			dv, err := pf.getFieldDefault()
			if err != nil {
				return fmt.Errorf("field %q: %w", name, err)
			}
			dvlit, err := anyToIcebergLit(pf.Type.ValueString(), dv)
			if err != nil {
				return fmt.Errorf("field %q: %w", name, err)
			}
			if !exists {
				updater.AddColumn([]string{name}, typ, pf.Doc.ValueString(), pf.Required.ValueBool(), dvlit)
			} else {
				updater.UpdateColumn([]string{name}, itable.ColumnUpdate{
					FieldType: iceberg.Optional[iceberg.Type]{Valid: true, Val: typ},
					Doc: iceberg.Optional[string]{Valid: true, Val: pf.Doc.ValueString()},
					Required: iceberg.Optional[bool]{Valid: true, Val: pf.Required.ValueBool()},
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

// checkPropChanges - returns error if properties are different
// Note: table property updates not supported in icebert-go package
func checkPropChanges(stateProps, planProps []PropertyModel) error {
	// Build a map of current props by name.
	current := make(map[string]PropertyModel)
	for _, p := range stateProps {
		current[p.Name.ValueString()] = p
	}

	// Build a set of plan partition field names.
	plan := make(map[string]PropertyModel)
	for _, p := range planProps {
		plan[p.Name.ValueString()] = p
	}

	// check for changes
	if len(current) != len(plan) {
		return fmt.Errorf("Differing properties count: %d vs %d", len(current), len(plan))
	}
	for name, pp := range plan {
		if sp, exists := current[name]; !exists || pp != sp {
			return fmt.Errorf("Differing property: %v", name)
		}
	}
	for name, _ := range current {
		if _, exists := plan[name]; !exists {
			return fmt.Errorf("Missing property: %v", name)
		}
	}
	return nil
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
