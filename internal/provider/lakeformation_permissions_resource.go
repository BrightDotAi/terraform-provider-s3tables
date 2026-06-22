// Copyright BrightAI 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	lakeformation "github.com/aws/aws-sdk-go-v2/service/lakeformation"
	lftypes "github.com/aws/aws-sdk-go-v2/service/lakeformation/types"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

var _ resource.Resource = &LakeFormationPermissionsResource{}
var _ resource.ResourceWithConfigValidators = &LakeFormationPermissionsResource{}
var _ resource.ResourceWithImportState = &LakeFormationPermissionsResource{}
var _ resource.ResourceWithModifyPlan = &LakeFormationPermissionsResource{}

// lfClientIface is the subset of the LF client API used by this resource.
// *lakeformation.Client satisfies it; tests substitute a mock.
type lfClientIface interface {
	GrantPermissions(ctx context.Context, params *lakeformation.GrantPermissionsInput, optFns ...func(*lakeformation.Options)) (*lakeformation.GrantPermissionsOutput, error)
	RevokePermissions(ctx context.Context, params *lakeformation.RevokePermissionsInput, optFns ...func(*lakeformation.Options)) (*lakeformation.RevokePermissionsOutput, error)
	ListPermissions(ctx context.Context, params *lakeformation.ListPermissionsInput, optFns ...func(*lakeformation.Options)) (*lakeformation.ListPermissionsOutput, error)
}

// NewLakeFormationPermissionsResource returns a new LakeFormationPermissionsResource for registration with the Terraform provider.
func NewLakeFormationPermissionsResource() resource.Resource {
	return &LakeFormationPermissionsResource{}
}

// --- Permission level structs ---

type CatalogPermissions struct {
	All            types.Bool `tfsdk:"all"`
	Alter          types.Bool `tfsdk:"alter"`
	CreateCatalog  types.Bool `tfsdk:"create_catalog"`
	CreateDatabase types.Bool `tfsdk:"create_database"`
	Describe       types.Bool `tfsdk:"describe"`
	Drop           types.Bool `tfsdk:"drop"`
}

type DatabasePermissions struct {
	All         types.Bool `tfsdk:"all"`
	Alter       types.Bool `tfsdk:"alter"`
	CreateTable types.Bool `tfsdk:"create_table"`
	Describe    types.Bool `tfsdk:"describe"`
	Drop        types.Bool `tfsdk:"drop"`
}

type TablePermissions struct {
	All      types.Bool `tfsdk:"all"`
	Alter    types.Bool `tfsdk:"alter"`
	Delete   types.Bool `tfsdk:"delete"`
	Describe types.Bool `tfsdk:"describe"`
	Drop     types.Bool `tfsdk:"drop"`
	Insert   types.Bool `tfsdk:"insert"`
	Select   types.Bool `tfsdk:"select"`
}

// --- Resource and model types ---

type LakeFormationPermissionsResource struct {
	awsCfg aws.Config
}

type LakeFormationPermissionsResourceModel struct {
	Principal types.String      `tfsdk:"principal"`
	Region    types.String      `tfsdk:"region"`
	Catalog   *CatalogPermModel `tfsdk:"catalog"`
}

type CatalogPermModel struct {
	ID                   types.String         `tfsdk:"id"`
	Permissions          *CatalogPermissions  `tfsdk:"permissions"`
	GrantablePermissions *CatalogPermissions  `tfsdk:"grantable_permissions"`
	Database             []DatabasePermModel  `tfsdk:"database"`
}

type DatabasePermModel struct {
	Name                 types.String          `tfsdk:"name"`
	Permissions          *DatabasePermissions  `tfsdk:"permissions"`
	GrantablePermissions *DatabasePermissions  `tfsdk:"grantable_permissions"`
	Table                []TablePermModel     `tfsdk:"table"`
	Wildcard             *TablePermModel      `tfsdk:"wildcard"`
}

type TablePermModel struct {
	Name                 types.String      `tfsdk:"name"`
	IsWildcard           bool              `tfsdk:"-"`
	Permissions          *TablePermissions `tfsdk:"permissions"`
	GrantablePermissions *TablePermissions `tfsdk:"grantable_permissions"`
}

// --- Schema helpers ---

// catalogPermAttr returns the schema definition for a catalog-level permissions block.
// When computed is true (used for the permissions attribute) the block and its inner bools
// are Optional+Computed so the provider can fill in the value when omitted.
func catalogPermAttr(desc string, computed bool) schema.SingleNestedAttribute {
	b := func(d string) schema.BoolAttribute {
		return schema.BoolAttribute{Optional: true, Computed: computed, MarkdownDescription: d}
	}
	return schema.SingleNestedAttribute{
		Optional:            true,
		Computed:            computed,
		MarkdownDescription: desc,
		Attributes: map[string]schema.Attribute{
			"all":             b("Grants all catalog permissions. Mutually exclusive with individual permission attributes."),
			"alter":           b("Grants ALTER on the catalog."),
			"create_catalog":  b("Grants CREATE_CATALOG."),
			"create_database": b("Grants CREATE_DATABASE."),
			"describe":        b("Grants DESCRIBE on the catalog."),
			"drop":            b("Grants DROP on the catalog."),
		},
	}
}

// databasePermAttr returns the schema definition for a database-level permissions block.
// See catalogPermAttr for the computed parameter semantics.
func databasePermAttr(desc string, computed bool) schema.SingleNestedAttribute {
	b := func(d string) schema.BoolAttribute {
		return schema.BoolAttribute{Optional: true, Computed: computed, MarkdownDescription: d}
	}
	return schema.SingleNestedAttribute{
		Optional:            true,
		Computed:            computed,
		MarkdownDescription: desc,
		Attributes: map[string]schema.Attribute{
			"all":          b("Grants all database permissions. Mutually exclusive with individual permission attributes."),
			"alter":        b("Grants ALTER on the database."),
			"create_table": b("Grants CREATE_TABLE."),
			"describe":     b("Grants DESCRIBE on the database."),
			"drop":         b("Grants DROP on the database."),
		},
	}
}

// tablePermAttr returns the schema definition for a table-level permissions block.
// See catalogPermAttr for the computed parameter semantics.
func tablePermAttr(desc string, computed bool) schema.SingleNestedAttribute {
	b := func(d string) schema.BoolAttribute {
		return schema.BoolAttribute{Optional: true, Computed: computed, MarkdownDescription: d}
	}
	return schema.SingleNestedAttribute{
		Optional:            true,
		Computed:            computed,
		MarkdownDescription: desc,
		Attributes: map[string]schema.Attribute{
			"all":      b("Grants all table permissions. Mutually exclusive with individual permission attributes."),
			"alter":    b("Grants ALTER on the table."),
			"delete":   b("Grants DELETE on the table."),
			"describe": b("Grants DESCRIBE on the table."),
			"drop":     b("Grants DROP on the table."),
			"insert":   b("Grants INSERT on the table."),
			"select":   b("Grants SELECT on the table."),
		},
	}
}

// Metadata sets the Terraform type name for this resource to `{provider}_lakeformation_permissions`.
func (r *LakeFormationPermissionsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_lakeformation_permissions"
}

// Schema declares the Terraform schema for the resource, covering the principal and region
// attributes and the catalog block with nested database, table, and wildcard sub-blocks.
func (r *LakeFormationPermissionsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages AWS Lake Formation permissions for a catalog, databases, and tables.",
		Attributes: map[string]schema.Attribute{
			"principal": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "IAM principal ARN to grant permissions to.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"region": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "AWS region where the Lake Formation permissions reside. If omitted, falls back to the provider region or AWS_REGION / AWS_DEFAULT_REGION.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
		},
		Blocks: map[string]schema.Block{
			"catalog": schema.SingleNestedBlock{
				MarkdownDescription: "Catalog-level permissions and nested database/table permissions.",
				Attributes: map[string]schema.Attribute{
					"id": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "AWS account ID (catalog ID) that owns the resources.",
						PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
					},
					"permissions":           catalogPermAttr("Catalog-level permissions to grant.", true),
					"grantable_permissions": catalogPermAttr("Catalog-level permissions the principal can grant to others.", false),
				},
				Blocks: map[string]schema.Block{
					"database": schema.ListNestedBlock{
						MarkdownDescription: "Database-level permissions.",
						NestedObject: schema.NestedBlockObject{
							Attributes: map[string]schema.Attribute{
								"name": schema.StringAttribute{
									Required:            true,
									MarkdownDescription: "Database name.",
								},
								"permissions":           databasePermAttr("Database-level permissions to grant.", true),
								"grantable_permissions": databasePermAttr("Database-level permissions the principal can grant to others.", false),
							},
							Blocks: map[string]schema.Block{
								"table": schema.ListNestedBlock{
									MarkdownDescription: "Named table permissions. Mutually exclusive with `wildcard`.",
									NestedObject: schema.NestedBlockObject{
										Attributes: map[string]schema.Attribute{
											"name": schema.StringAttribute{
												Required:            true,
												MarkdownDescription: "Table name.",
											},
											"permissions":           tablePermAttr("Table-level permissions to grant.", true),
											"grantable_permissions": tablePermAttr("Table-level permissions the principal can grant to others.", false),
										},
									},
								},
								"wildcard": schema.SingleNestedBlock{
									MarkdownDescription: "Permissions on all tables in this database. Mutually exclusive with `table`.",
									Attributes: map[string]schema.Attribute{
										"name":                  schema.StringAttribute{Optional: true, MarkdownDescription: "Must be omitted or empty; present only for struct compatibility with named table entries."},
										"permissions":           tablePermAttr("Table-level permissions to grant on all tables.", true),
										"grantable_permissions": tablePermAttr("Table-level permissions the principal can grant to others on all tables.", false),
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// Configure extracts the aws.Config from the provider-supplied data and stores it on the
// resource so that CRUD operations can call the Lake Formation API.
func (r *LakeFormationPermissionsResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	cfg, ok := req.ProviderData.(aws.Config)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data type",
			fmt.Sprintf("Expected aws.Config, got %T.", req.ProviderData))
		return
	}
	r.awsCfg = cfg
}

// ConfigValidators enforces catalog-required and the tables/wildcard mutex.
func (r *LakeFormationPermissionsResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{&lfPermissionsValidator{}}
}

type lfPermissionsValidator struct{}

// Description returns a plain-text description of the validator for use in diagnostic messages.
func (v *lfPermissionsValidator) Description(_ context.Context) string {
	return "Validates Lake Formation mutual exclusivity constraints."
}

// MarkdownDescription returns the Markdown form of the validator description by delegating to Description.
func (v *lfPermissionsValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

// ValidateResource enforces that a catalog block is present, that all=true is not combined
// with individual permission flags, and that table and wildcard blocks are not used together
// in the same database block.
func (v *lfPermissionsValidator) ValidateResource(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var data LakeFormationPermissionsResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if data.Catalog == nil {
		resp.Diagnostics.AddError("Missing required block", "A catalog block is required.")
		return
	}

	catPath := path.Root("catalog")
	checkPerms(data.Catalog.Permissions, catPath.AtName("permissions"), &resp.Diagnostics)
	checkPerms(data.Catalog.GrantablePermissions, catPath.AtName("grantable_permissions"), &resp.Diagnostics)
	checkSupersetPerms(data.Catalog.Permissions, data.Catalog.GrantablePermissions, catPath, &resp.Diagnostics)

	for i, db := range data.Catalog.Database {
		dbPath := catPath.AtName("database").AtListIndex(i)

		if len(db.Table) > 0 && db.Wildcard != nil {
			resp.Diagnostics.AddAttributeError(dbPath, "Conflicting configuration",
				"A database block cannot specify both table and wildcard.")
		}

		checkPerms(db.Permissions, dbPath.AtName("permissions"), &resp.Diagnostics)
		checkPerms(db.GrantablePermissions, dbPath.AtName("grantable_permissions"), &resp.Diagnostics)
		checkSupersetPerms(db.Permissions, db.GrantablePermissions, dbPath, &resp.Diagnostics)

		for j, tbl := range db.Table {
			tblPath := dbPath.AtName("table").AtListIndex(j)
			checkPerms(tbl.Permissions, tblPath.AtName("permissions"), &resp.Diagnostics)
			checkPerms(tbl.GrantablePermissions, tblPath.AtName("grantable_permissions"), &resp.Diagnostics)
			checkSupersetPerms(tbl.Permissions, tbl.GrantablePermissions, tblPath, &resp.Diagnostics)
			if tbl.Permissions == nil && tbl.GrantablePermissions == nil {
				resp.Diagnostics.AddAttributeError(tblPath, "Missing required attribute",
					"A table block must specify at least one of 'permissions' or 'grantable_permissions'.")
			}
		}

		if db.Wildcard != nil {
			wcPath := dbPath.AtName("wildcard")
			checkPerms(db.Wildcard.Permissions, wcPath.AtName("permissions"), &resp.Diagnostics)
			checkPerms(db.Wildcard.GrantablePermissions, wcPath.AtName("grantable_permissions"), &resp.Diagnostics)
			checkSupersetPerms(db.Wildcard.Permissions, db.Wildcard.GrantablePermissions, wcPath, &resp.Diagnostics)
			if db.Wildcard.Permissions == nil && db.Wildcard.GrantablePermissions == nil {
				resp.Diagnostics.AddAttributeError(wcPath, "Missing required attribute",
					"A wildcard block must specify at least one of 'permissions' or 'grantable_permissions'.")
			}
			if db.Wildcard.Name.ValueString() != "" {
				resp.Diagnostics.AddAttributeError(wcPath.AtName("name"), "Unexpected attribute",
					"The 'name' attribute must not be set in a wildcard block.")
			}
		}
	}
}

// checkSupersetPerms validates that perms is a superset of grantPerms. Skipped when perms is
// nil (it will be computed from grantPerms by ModifyPlan) or when perms contains ALL (which
// is a superset of every possible grantable_permissions value).
func checkSupersetPerms(perms, grantPerms any, parentPath path.Path, diags *diag.Diagnostics) {
	// nil permissions → will be computed; nothing to validate yet.
	if perms == nil {
		return
	}
	if rv := reflect.ValueOf(perms); rv.Kind() == reflect.Pointer && rv.IsNil() {
		return
	}
	p := permsToAPI(perms)
	g := permsToAPI(grantPerms)
	if len(g) == 0 || containsPermission(p, lftypes.PermissionAll) {
		return
	}
	ps := permSet(p)
	for _, gp := range g {
		if !ps[gp] {
			fieldName := strings.ToLower(string(gp))
			diags.AddAttributeError(
				parentPath.AtName("grantable_permissions").AtName(fieldName),
				"Permission not in 'permissions'",
				fmt.Sprintf("'%s' is in 'grantable_permissions' but not in 'permissions': "+
					"when both are specified, 'permissions' must contain every permission listed in 'grantable_permissions'.", fieldName),
			)
			return
		}
	}
}

// checkPerms validates a permission struct pointed to by p: reports an error if all=true
// is combined with any individual flag, and reports an error if every individual flag is
// true without all=true (use all=true instead). p must be a pointer to a struct; nil
// pointers are silently ignored.
func checkPerms(p any, attrPath path.Path, diags *diag.Diagnostics) {
	if p == nil {
		return
	}
	rv := reflect.ValueOf(p)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return
	}
	rv = rv.Elem()
	t := rv.Type()

	allField := rv.FieldByName("All")
	if !allField.IsValid() {
		return
	}
	allBool, ok := allField.Interface().(types.Bool)
	if !ok {
		return
	}
	allSet := allBool.ValueBool()

	total, trueCount := 0, 0
	for i := 0; i < rv.NumField(); i++ {
		if t.Field(i).Name == "All" {
			continue
		}
		b, ok := rv.Field(i).Interface().(types.Bool)
		if !ok {
			continue
		}
		total++
		if b.ValueBool() {
			trueCount++
			if allSet {
				diags.AddAttributeError(attrPath.AtName("all"), "Conflicting attributes",
					"Cannot set 'all' alongside individual permission attributes.")
				return
			}
		}
	}
	if !allSet && total > 0 && trueCount == total {
		diags.AddAttributeError(attrPath, "Implicit ALL not permitted",
			"To grant every permission use all = true. Setting every individual flag is not allowed.")
	}
}

// ModifyPlan is the single place where computed plan values are filled in:
//  1. Region — resolved from the provider config or environment when not set explicitly.
//  2. Permissions — set equal to grantable_permissions on any resource where permissions
//     is omitted but grantable_permissions is present.
func (r *LakeFormationPermissionsResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroy plan has no values to compute
	}

	var plan LakeFormationPermissionsResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	changed := false

	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		region, err := r.resolveRegion(plan.Region)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("region"), "Region not configured", err.Error())
			return
		}
		plan.Region = types.StringValue(region)
		changed = true
	}

	changed = defaultPermissions(&plan) || changed

	if changed {
		resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
	}
}

// defaultPermissions sets permissions equal to grantable_permissions on every resource
// where permissions is nil and grantable_permissions is non-nil. Returns true if any field was set.
func defaultPermissions(plan *LakeFormationPermissionsResourceModel) bool {
	if plan.Catalog == nil {
		return false
	}
	changed := false
	changed = defaultPermsFromGrantable(&plan.Catalog.Permissions, plan.Catalog.GrantablePermissions) || changed
	for i := range plan.Catalog.Database {
		db := &plan.Catalog.Database[i]
		changed = defaultPermsFromGrantable(&db.Permissions, db.GrantablePermissions) || changed
		for j := range db.Table {
			tbl := &db.Table[j]
			changed = defaultPermsFromGrantable(&tbl.Permissions, tbl.GrantablePermissions) || changed
		}
		if db.Wildcard != nil {
			changed = defaultPermsFromGrantable(&db.Wildcard.Permissions, db.Wildcard.GrantablePermissions) || changed
		}
	}
	return changed
}

// defaultPermsFromGrantable sets *perms to a copy of grantPerms when *perms is nil and grantPerms
// is non-nil. Returns true if the assignment was made.
func defaultPermsFromGrantable[T any](perms **T, grantPerms *T) bool {
	if *perms == nil && grantPerms != nil {
		cp := *grantPerms
		*perms = &cp
		return true
	}
	return false
}

// Create grants all permissions declared in the plan and writes the resulting state to Terraform.
func (r *LakeFormationPermissionsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data LakeFormationPermissionsResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client := r.lfClient(data.Region.ValueString())
	if err := grantAll(ctx, client, &data); err != nil {
		resp.Diagnostics.AddError("Failed to grant Lake Formation permissions", err.Error())
		return
	}

	tflog.Trace(ctx, "created lakeformation_permissions resource")
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// Read fetches the current Lake Formation permissions for each declared resource from AWS
// and refreshes Terraform state to reflect what is actually active.
func (r *LakeFormationPermissionsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data LakeFormationPermissionsResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if data.Catalog == nil {
		return
	}

	client := r.lfClient(data.Region.ValueString())
	principal := data.Principal.ValueString()
	catalogID := data.Catalog.ID.ValueString()

	if data.Catalog.Permissions != nil || data.Catalog.GrantablePermissions != nil {
		p, g, err := listLFPerms(ctx, client, principal, &lftypes.Resource{Catalog: &lftypes.CatalogResource{}})
		if err != nil {
			resp.Diagnostics.AddError("Failed to read catalog permissions",
				fmt.Sprintf("principal=%s catalog=%s: %s", principal, catalogID, err))
			return
		}
		data.Catalog.Permissions = refreshPerms(data.Catalog.Permissions, p)
		data.Catalog.GrantablePermissions = refreshPerms(data.Catalog.GrantablePermissions, g)
	}

	for i, db := range data.Catalog.Database {
		dbName := db.Name.ValueString()
		if db.Permissions != nil || db.GrantablePermissions != nil {
			p, g, err := listLFPerms(ctx, client, principal, &lftypes.Resource{
				Database: &lftypes.DatabaseResource{
					CatalogId: aws.String(catalogID),
					Name:      aws.String(dbName),
				},
			})
			if err != nil {
				resp.Diagnostics.AddError("Failed to read database permissions",
					fmt.Sprintf("principal=%s catalog=%s database=%s: %s", principal, catalogID, dbName, err))
				return
			}
			data.Catalog.Database[i].Permissions = refreshPerms(db.Permissions, p)
			data.Catalog.Database[i].GrantablePermissions = refreshPerms(db.GrantablePermissions, g)
		}

		for j, tbl := range db.Table {
			if tbl.Permissions != nil || tbl.GrantablePermissions != nil {
				tblName := tbl.Name.ValueString()
				tp, tg, err := listLFPerms(ctx, client, principal, &lftypes.Resource{
					Table: &lftypes.TableResource{
						CatalogId:    aws.String(catalogID),
						DatabaseName: aws.String(dbName),
						Name:         aws.String(tblName),
					},
				})
				if err != nil {
					resp.Diagnostics.AddError("Failed to read table permissions",
						fmt.Sprintf("principal=%s catalog=%s database=%s table=%s: %s", principal, catalogID, dbName, tblName, err))
					return
				}
				data.Catalog.Database[i].Table[j].Permissions = refreshPerms(tbl.Permissions, tp)
				data.Catalog.Database[i].Table[j].GrantablePermissions = refreshPerms(tbl.GrantablePermissions, tg)
			}
		}

		if db.Wildcard != nil && (db.Wildcard.Permissions != nil || db.Wildcard.GrantablePermissions != nil) {
			wp, wg, err := listLFPerms(ctx, client, principal, &lftypes.Resource{
				Table: &lftypes.TableResource{
					CatalogId:     aws.String(catalogID),
					DatabaseName:  aws.String(dbName),
					TableWildcard: &lftypes.TableWildcard{},
				},
			})
			if err != nil {
				resp.Diagnostics.AddError("Failed to read wildcard permissions",
					fmt.Sprintf("principal=%s catalog=%s database=%s wildcard: %s", principal, catalogID, dbName, err))
				return
			}
			data.Catalog.Database[i].Wildcard.IsWildcard = true
			data.Catalog.Database[i].Wildcard.Permissions = refreshPerms(db.Wildcard.Permissions, wp)
			data.Catalog.Database[i].Wildcard.GrantablePermissions = refreshPerms(db.Wildcard.GrantablePermissions, wg)
		}
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// Update applies a diff-based change: only permissions removed from the plan are revoked, and
// only new permissions added to the plan are granted. Permissions unchanged between state and
// plan are left untouched. ALL is an exception — when ALL appears on either side the resource
// always undergoes a full revoke-then-grant cycle.
func (r *LakeFormationPermissionsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var state, plan LakeFormationPermissionsResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client := r.lfClient(plan.Region.ValueString())
	if err := updatePermissions(ctx, client, &state, &plan); err != nil {
		resp.Diagnostics.AddError("Failed to update Lake Formation permissions", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete revokes all Lake Formation permissions recorded in state.
func (r *LakeFormationPermissionsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data LakeFormationPermissionsResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client := r.lfClient(data.Region.ValueString())
	if err := deletePermissions(ctx, client, &data); err != nil {
		resp.Diagnostics.AddError("Failed to revoke Lake Formation permissions", err.Error())
	}
}

// ImportState accepts: <principal_arn>,<region>,<catalog_id>
// where catalog_id has the form <account_id>:s3tablescatalog/<bucket_name>.
// Region must be specified explicitly because ModifyPlan (which resolves a missing region)
// does not run during import; a null region in state would break the subsequent Read.
func (r *LakeFormationPermissionsResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, ",", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		resp.Diagnostics.AddError(
			"Invalid import ID",
			fmt.Sprintf("Expected format principal_arn,region,catalog_id, got: %q", req.ID),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("principal"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("region"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("catalog").AtName("id"), parts[2])...)
}

// --- Helpers ---

// resolveRegion returns the effective region: the resource attribute if set, then the
// provider-level region from awsCfg, then an error if neither is available.
func (r *LakeFormationPermissionsResource) resolveRegion(resourceRegion types.String) (string, error) {
	if !resourceRegion.IsNull() && !resourceRegion.IsUnknown() {
		if s := resourceRegion.ValueString(); s != "" {
			return s, nil
		}
	}
	if r.awsCfg.Region != "" {
		return r.awsCfg.Region, nil
	}
	return "", fmt.Errorf("no AWS region configured: set the region attribute or configure AWS_REGION / AWS_DEFAULT_REGION")
}

// lfClient returns a Lake Formation client overriding the config region.
func (r *LakeFormationPermissionsResource) lfClient(region string) *lakeformation.Client {
	cfg := r.awsCfg
	cfg.Region = region
	return lakeformation.NewFromConfig(cfg)
}

// grantAll grants every permission declared in data at the catalog, database, table, and wildcard levels.
func grantAll(ctx context.Context, client lfClientIface, data *LakeFormationPermissionsResourceModel) error {
	if data.Catalog == nil {
		return nil
	}
	principal := data.Principal.ValueString()
	catalogID := data.Catalog.ID.ValueString()

	if err := grantLFPerms(ctx, client, principal,
		&lftypes.Resource{Catalog: &lftypes.CatalogResource{}},
		permsToAPI(data.Catalog.Permissions),
		permsToAPI(data.Catalog.GrantablePermissions)); err != nil {
		return fmt.Errorf("principal=%s catalog=%s: %w", principal, catalogID, err)
	}

	for _, db := range data.Catalog.Database {
		dbName := db.Name.ValueString()
		dbRes := &lftypes.Resource{
			Database: &lftypes.DatabaseResource{
				CatalogId: aws.String(catalogID),
				Name:      aws.String(dbName),
			},
		}
		if err := grantLFPerms(ctx, client, principal, dbRes,
			permsToAPI(db.Permissions),
			permsToAPI(db.GrantablePermissions)); err != nil {
			return fmt.Errorf("principal=%s catalog=%s database=%s: %w", principal, catalogID, dbName, err)
		}

		for _, tbl := range db.Table {
			tblName := tbl.Name.ValueString()
			tblRes := &lftypes.Resource{
				Table: &lftypes.TableResource{
					CatalogId:    aws.String(catalogID),
					DatabaseName: aws.String(dbName),
					Name:         aws.String(tblName),
				},
			}
			if err := grantLFPerms(ctx, client, principal, tblRes,
				permsToAPI(tbl.Permissions),
				permsToAPI(tbl.GrantablePermissions)); err != nil {
				return fmt.Errorf("principal=%s catalog=%s database=%s table=%s: %w", principal, catalogID, dbName, tblName, err)
			}
		}

		if db.Wildcard != nil {
			wcRes := &lftypes.Resource{
				Table: &lftypes.TableResource{
					CatalogId:     aws.String(catalogID),
					DatabaseName:  aws.String(dbName),
					TableWildcard: &lftypes.TableWildcard{},
				},
			}
			if err := grantLFPerms(ctx, client, principal, wcRes,
				permsToAPI(db.Wildcard.Permissions),
				permsToAPI(db.Wildcard.GrantablePermissions)); err != nil {
				return fmt.Errorf("principal=%s catalog=%s database=%s wildcard: %w", principal, catalogID, dbName, err)
			}
		}
	}
	return nil
}

// deletePermissions revokes every permission that was explicitly declared in data (i.e. whose
// Permissions or GrantablePermissions field is non-nil). Resources for which no permissions
// field was ever specified in the plan are left completely untouched.
func deletePermissions(ctx context.Context, client lfClientIface, data *LakeFormationPermissionsResourceModel) error {
	if data.Catalog == nil {
		return nil
	}
	principal := data.Principal.ValueString()
	catalogID := data.Catalog.ID.ValueString()

	// Catalog-level: only if the plan declared permissions here.
	if data.Catalog.Permissions != nil || data.Catalog.GrantablePermissions != nil {
		if err := applyResourceDiff(ctx, client, principal,
			&lftypes.Resource{Catalog: &lftypes.CatalogResource{}},
			permsToAPI(data.Catalog.Permissions), nil,
			permsToAPI(data.Catalog.GrantablePermissions), nil); err != nil {
			return fmt.Errorf("principal=%s catalog=%s: %w", principal, catalogID, err)
		}
	}

	for _, db := range data.Catalog.Database {
		dbName := db.Name.ValueString()
		dbRes := &lftypes.Resource{Database: &lftypes.DatabaseResource{
			CatalogId: aws.String(catalogID),
			Name:      aws.String(dbName),
		}}

		// Database-level: only if the plan declared permissions here.
		if db.Permissions != nil || db.GrantablePermissions != nil {
			if err := applyResourceDiff(ctx, client, principal, dbRes,
				permsToAPI(db.Permissions), nil,
				permsToAPI(db.GrantablePermissions), nil); err != nil {
				return fmt.Errorf("principal=%s catalog=%s database=%s: %w", principal, catalogID, dbName, err)
			}
		}

		for _, tbl := range db.Table {
			if tbl.Permissions == nil && tbl.GrantablePermissions == nil {
				continue
			}
			tblName := tbl.Name.ValueString()
			tblRes := &lftypes.Resource{Table: &lftypes.TableResource{
				CatalogId:    aws.String(catalogID),
				DatabaseName: aws.String(dbName),
				Name:         aws.String(tblName),
			}}
			if err := applyResourceDiff(ctx, client, principal, tblRes,
				permsToAPI(tbl.Permissions), nil,
				permsToAPI(tbl.GrantablePermissions), nil); err != nil {
				return fmt.Errorf("principal=%s catalog=%s database=%s table=%s: %w", principal, catalogID, dbName, tblName, err)
			}
		}

		if db.Wildcard != nil && (db.Wildcard.Permissions != nil || db.Wildcard.GrantablePermissions != nil) {
			wcRes := &lftypes.Resource{Table: &lftypes.TableResource{
				CatalogId:     aws.String(catalogID),
				DatabaseName:  aws.String(dbName),
				TableWildcard: &lftypes.TableWildcard{},
			}}
			if err := applyResourceDiff(ctx, client, principal, wcRes,
				permsToAPI(db.Wildcard.Permissions), nil,
				permsToAPI(db.Wildcard.GrantablePermissions), nil); err != nil {
				return fmt.Errorf("principal=%s catalog=%s database=%s wildcard: %w", principal, catalogID, dbName, err)
			}
		}
	}
	return nil
}

// revokeIfPermitted calls ListPermissions first and skips the revoke when the principal
// holds no active permissions on res, avoiding AWS errors for non-existent grants.
func revokeIfPermitted(ctx context.Context, client lfClientIface, principal string, res *lftypes.Resource, perms, grantPerms []lftypes.Permission) error {
	if len(perms) == 0 && len(grantPerms) == 0 {
		return nil
	}
	curP, curG, err := listLFPerms(ctx, client, principal, res)
	if err != nil {
		return err
	}
	if len(curP) == 0 && len(curG) == 0 {
		return nil
	}
	return revokeLFPerms(ctx, client, principal, res, perms, grantPerms)
}


// setSubtract returns elements of a that are not in b.
func setSubtract(a, b []lftypes.Permission) []lftypes.Permission {
	if len(a) == 0 {
		return nil
	}
	if len(b) == 0 {
		return a
	}
	bSet := permSet(b)
	var out []lftypes.Permission
	for _, p := range a {
		if !bSet[p] {
			out = append(out, p)
		}
	}
	return out
}

// containsPermission reports whether p appears in perms.
func containsPermission(perms []lftypes.Permission, p lftypes.Permission) bool {
	for _, q := range perms {
		if q == p {
			return true
		}
	}
	return false
}

// revokeIfPermittedDirect is like revokeIfPermitted but passes Permissions and
// PermissionsWithGrantOption to AWS exactly as given, without merging. Used in
// diff-based updates where a grant option may be revoked independently of its
// corresponding regular permission.
func revokeIfPermittedDirect(ctx context.Context, client lfClientIface, principal string, res *lftypes.Resource, perms, grantPerms []lftypes.Permission) error {
	if len(perms) == 0 && len(grantPerms) == 0 {
		return nil
	}
	curP, curG, err := listLFPerms(ctx, client, principal, res)
	if err != nil {
		return err
	}
	if len(curP) == 0 && len(curG) == 0 {
		return nil
	}
	_, err = client.RevokePermissions(ctx, &lakeformation.RevokePermissionsInput{
		Principal:                  &lftypes.DataLakePrincipal{DataLakePrincipalIdentifier: aws.String(principal)},
		Resource:                   res,
		Permissions:                perms,
		PermissionsWithGrantOption: grantPerms,
	})
	return err
}

// applyResourceDiff computes the minimal revoke and grant operations for a single resource
// by diffing its current state against the desired plan. stateP/stateG are the currently
// active regular and grantable permissions; planP/planG are the desired state (nil/empty
// means the field should be fully revoked). When ALL appears on either side a full
// revoke-then-grant cycle is performed instead of a granular diff.
func applyResourceDiff(ctx context.Context, client lfClientIface, principal string, res *lftypes.Resource,
	stateP, planP, stateG, planG []lftypes.Permission) error {

	if len(stateP) == 0 && len(planP) == 0 {
		return nil
	}

	// ALL exception: full revoke+grant cycle so the permission set is always clean.
	if containsPermission(planP, lftypes.PermissionAll) || containsPermission(stateP, lftypes.PermissionAll) {
		if err := revokeIfPermitted(ctx, client, principal, res, stateP, stateG); err != nil {
			return err
		}
		return grantLFPerms(ctx, client, principal, res, planP, planG)
	}

	// Diff-based path: only touch what changed.
	revokeP := setSubtract(stateP, planP)
	revokeG := setSubtract(stateG, planG)
	grantP := setSubtract(planP, stateP)
	grantG := setSubtract(planG, stateG)

	if len(revokeP) > 0 || len(revokeG) > 0 {
		if err := revokeIfPermittedDirect(ctx, client, principal, res, revokeP, revokeG); err != nil {
			return err
		}
	}
	if len(grantP) > 0 || len(grantG) > 0 {
		if err := grantLFPerms(ctx, client, principal, res, grantP, grantG); err != nil {
			return err
		}
	}
	return nil
}

// updatePermissions applies diff-based permission changes for every resource declared in state or plan.
// For each resource it calls applyResourceDiff, which revokes only removed permissions and grants
// only new ones. Resources absent from plan are fully revoked; resources new in plan are fully granted.
func updatePermissions(ctx context.Context, client lfClientIface, state, plan *LakeFormationPermissionsResourceModel) error {
	if state.Catalog == nil || plan.Catalog == nil {
		return nil
	}
	principal := plan.Principal.ValueString()
	catalogID := plan.Catalog.ID.ValueString()

	// Catalog-level permissions — only when plan actively manages them (non-nil block).
	if plan.Catalog.Permissions != nil || plan.Catalog.GrantablePermissions != nil {
		stP := permsToAPI(state.Catalog.Permissions)
		stG := permsToAPI(state.Catalog.GrantablePermissions)
		plP := stP // nil plan field ⇒ no change for that dimension
		if plan.Catalog.Permissions != nil {
			plP = permsToAPI(plan.Catalog.Permissions)
		}
		plG := stG
		if plan.Catalog.GrantablePermissions != nil {
			plG = permsToAPI(plan.Catalog.GrantablePermissions)
		}
		if err := applyResourceDiff(ctx, client, principal,
			&lftypes.Resource{Catalog: &lftypes.CatalogResource{}},
			stP, plP, stG, plG); err != nil {
			return fmt.Errorf("principal=%s catalog=%s: %w", principal, catalogID, err)
		}
	}

	// Build index maps for databases.
	stateDBIdx := make(map[string]DatabasePermModel, len(state.Catalog.Database))
	for _, db := range state.Catalog.Database {
		stateDBIdx[db.Name.ValueString()] = db
	}
	planDBIdx := make(map[string]DatabasePermModel, len(plan.Catalog.Database))
	for _, db := range plan.Catalog.Database {
		planDBIdx[db.Name.ValueString()] = db
	}

	// Union of database names from both sides.
	allDBNames := make(map[string]struct{}, len(stateDBIdx)+len(planDBIdx))
	for k := range stateDBIdx {
		allDBNames[k] = struct{}{}
	}
	for k := range planDBIdx {
		allDBNames[k] = struct{}{}
	}

	for name := range allDBNames {
		stDB, inState := stateDBIdx[name]
		plDB, inPlan := planDBIdx[name]

		dbRes := &lftypes.Resource{Database: &lftypes.DatabaseResource{
			CatalogId: aws.String(catalogID),
			Name:      aws.String(name),
		}}

		// Database-level permissions.
		stP := permsToAPI(stDB.Permissions)
		stG := permsToAPI(stDB.GrantablePermissions)
		var plP, plG []lftypes.Permission
		switch {
		case !inPlan:
			// Database removed: revoke everything.
		case !inState:
			// New database: grant plan permissions from scratch.
			plP = permsToAPI(plDB.Permissions)
			plG = permsToAPI(plDB.GrantablePermissions)
		default:
			plP = stP
			if plDB.Permissions != nil {
				plP = permsToAPI(plDB.Permissions)
			}
			plG = stG
			if plDB.GrantablePermissions != nil {
				plG = permsToAPI(plDB.GrantablePermissions)
			}
		}
		if err := applyResourceDiff(ctx, client, principal, dbRes, stP, plP, stG, plG); err != nil {
			return fmt.Errorf("principal=%s catalog=%s database=%s: %w", principal, catalogID, name, err)
		}

		// Tables — union of state and plan tables for this database.
		stTblIdx := make(map[string]TablePermModel)
		if inState {
			for _, tbl := range stDB.Table {
				stTblIdx[tbl.Name.ValueString()] = tbl
			}
		}
		plTblIdx := make(map[string]TablePermModel)
		if inPlan {
			for _, tbl := range plDB.Table {
				plTblIdx[tbl.Name.ValueString()] = tbl
			}
		}
		allTblNames := make(map[string]struct{}, len(stTblIdx)+len(plTblIdx))
		for k := range stTblIdx {
			allTblNames[k] = struct{}{}
		}
		for k := range plTblIdx {
			allTblNames[k] = struct{}{}
		}

		for tblName := range allTblNames {
			stTbl, tblInState := stTblIdx[tblName]
			plTbl, tblInPlan := plTblIdx[tblName]

			tblRes := &lftypes.Resource{Table: &lftypes.TableResource{
				CatalogId:    aws.String(catalogID),
				DatabaseName: aws.String(name),
				Name:         aws.String(tblName),
			}}

			stP := permsToAPI(stTbl.Permissions)
			stG := permsToAPI(stTbl.GrantablePermissions)
			var plP, plG []lftypes.Permission
			switch {
			case !tblInPlan || !inPlan:
				// Table or database removed.
			case !tblInState:
				plP = permsToAPI(plTbl.Permissions)
				plG = permsToAPI(plTbl.GrantablePermissions)
			default:
				plP = stP
				if plTbl.Permissions != nil {
					plP = permsToAPI(plTbl.Permissions)
				}
				plG = stG
				if plTbl.GrantablePermissions != nil {
					plG = permsToAPI(plTbl.GrantablePermissions)
				}
			}
			if err := applyResourceDiff(ctx, client, principal, tblRes, stP, plP, stG, plG); err != nil {
				return fmt.Errorf("principal=%s catalog=%s database=%s table=%s: %w", principal, catalogID, name, tblName, err)
			}
		}

		// Wildcard.
		var stWC, plWC *TablePermModel
		if inState {
			stWC = stDB.Wildcard
		}
		if inPlan {
			plWC = plDB.Wildcard
		}
		if stWC != nil || plWC != nil {
			wcRes := &lftypes.Resource{Table: &lftypes.TableResource{
				CatalogId:     aws.String(catalogID),
				DatabaseName:  aws.String(name),
				TableWildcard: &lftypes.TableWildcard{},
			}}
			var stP, stG []lftypes.Permission
			if stWC != nil {
				stP = permsToAPI(stWC.Permissions)
				stG = permsToAPI(stWC.GrantablePermissions)
			}
			var plP, plG []lftypes.Permission
			switch {
			case plWC == nil || !inPlan:
				// Wildcard or database removed.
			case stWC == nil:
				plP = permsToAPI(plWC.Permissions)
				plG = permsToAPI(plWC.GrantablePermissions)
			default:
				plP = stP
				if plWC.Permissions != nil {
					plP = permsToAPI(plWC.Permissions)
				}
				plG = stG
				if plWC.GrantablePermissions != nil {
					plG = permsToAPI(plWC.GrantablePermissions)
				}
			}
			if err := applyResourceDiff(ctx, client, principal, wcRes, stP, plP, stG, plG); err != nil {
				return fmt.Errorf("principal=%s catalog=%s database=%s wildcard: %w", principal, catalogID, name, err)
			}
		}
	}
	return nil
}

// grantLFPerms calls GrantPermissions for a single principal/resource pair; no-ops when both lists are empty.
// The caller must ensure grantPerms ⊆ perms (enforced by the schema superset constraint).
func grantLFPerms(ctx context.Context, client lfClientIface, principal string, res *lftypes.Resource, perms, grantPerms []lftypes.Permission) error {
	if len(perms) == 0 && len(grantPerms) == 0 {
		return nil
	}
	_, err := client.GrantPermissions(ctx, &lakeformation.GrantPermissionsInput{
		Principal:                  &lftypes.DataLakePrincipal{DataLakePrincipalIdentifier: aws.String(principal)},
		Resource:                   res,
		Permissions:                perms,
		PermissionsWithGrantOption: grantPerms,
	})
	return err
}

// revokeLFPerms calls RevokePermissions for a single principal/resource pair; no-ops when both lists are empty.
// The caller must ensure grantPerms ⊆ perms (enforced by the schema superset constraint).
func revokeLFPerms(ctx context.Context, client lfClientIface, principal string, res *lftypes.Resource, perms, grantPerms []lftypes.Permission) error {
	if len(perms) == 0 && len(grantPerms) == 0 {
		return nil
	}
	_, err := client.RevokePermissions(ctx, &lakeformation.RevokePermissionsInput{
		Principal:                  &lftypes.DataLakePrincipal{DataLakePrincipalIdentifier: aws.String(principal)},
		Resource:                   res,
		Permissions:                perms,
		PermissionsWithGrantOption: grantPerms,
	})
	return err
}

// listLFPerms pages through ListPermissions for a principal/resource pair and returns the active permissions and grant-option permissions.
func listLFPerms(ctx context.Context, client lfClientIface, principal string, res *lftypes.Resource) ([]lftypes.Permission, []lftypes.Permission, error) {
	paginator := lakeformation.NewListPermissionsPaginator(client, &lakeformation.ListPermissionsInput{
		Principal: &lftypes.DataLakePrincipal{DataLakePrincipalIdentifier: aws.String(principal)},
		Resource:  res,
	})
	var perms, grantPerms []lftypes.Permission
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, prp := range page.PrincipalResourcePermissions {
			perms = append(perms, prp.Permissions...)
			grantPerms = append(grantPerms, prp.PermissionsWithGrantOption...)
		}
	}
	return perms, grantPerms, nil
}

// --- Permission struct ↔ API conversions ---

// lfPermMap maps tfsdk field tag names to their Lake Formation permission constants.
var lfPermMap = map[string]lftypes.Permission{
	"all":             lftypes.PermissionAll,
	"alter":           lftypes.PermissionAlter,
	"create_catalog":  lftypes.PermissionCreateCatalog,
	"create_database": lftypes.PermissionCreateDatabase,
	"create_table":    lftypes.PermissionCreateTable,
	"delete":          lftypes.PermissionDelete,
	"describe":        lftypes.PermissionDescribe,
	"drop":            lftypes.PermissionDrop,
	"insert":          lftypes.PermissionInsert,
	"select":          lftypes.PermissionSelect,
}

// permsToAPI converts any permission struct to an API permission list.
// If the All field is true it returns [ALL] immediately. If every individual
// (non-All) field is true it also collapses to [ALL].
// p must be a pointer to a struct whose fields are types.Bool with tfsdk tags.
func permsToAPI(p any) []lftypes.Permission {
	if p == nil {
		return nil
	}
	rv := reflect.ValueOf(p)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return nil
	}
	rv = rv.Elem()
	t := rv.Type()

	allField := rv.FieldByName("All")
	if allField.IsValid() {
		if b, ok := allField.Interface().(types.Bool); ok && b.ValueBool() {
			return []lftypes.Permission{lftypes.PermissionAll}
		}
	}

	var out []lftypes.Permission
	for i := 0; i < rv.NumField(); i++ {
		tag := t.Field(i).Tag.Get("tfsdk")
		if tag == "-" || tag == "all" {
			continue
		}
		perm, ok := lfPermMap[tag]
		if !ok {
			continue
		}
		if b, ok := rv.Field(i).Interface().(types.Bool); ok && b.ValueBool() {
			out = append(out, perm)
		}
	}
	return out
}

// refreshPerms returns a new permissions struct reflecting which declared permissions are
// currently active. Each field's tfsdk tag encodes the AWS permission name
// (e.g. "create_database" → CREATE_DATABASE, "all" → ALL); ALL in current sets every
// declared individual flag true. The "all" field itself is refreshed via the same
// mechanism: if the user declared all=true and ALL is still active in AWS, it stays true.
func refreshPerms[T any](declared *T, current []lftypes.Permission) *T {
	if declared == nil {
		return nil
	}
	s := permSet(current)
	hasAll := s[lftypes.PermissionAll]
	dv := reflect.ValueOf(declared).Elem()
	t := dv.Type()
	nv := reflect.New(t).Elem()
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("tfsdk")
		if tag == "-" {
			continue
		}
		perm := lftypes.Permission(strings.ToUpper(tag))
		nv.Field(i).Set(reflect.ValueOf(
			refreshBool(dv.Field(i).Interface().(types.Bool), perm, s, hasAll),
		))
	}
	return nv.Addr().Interface().(*T)
}

// permSet converts a permission slice to a set for O(1) membership checks.
func permSet(perms []lftypes.Permission) map[lftypes.Permission]bool {
	s := make(map[lftypes.Permission]bool, len(perms))
	for _, p := range perms {
		s[p] = true
	}
	return s
}

// refreshBool returns the current state of a declared permission, or null if the
// field was not declared. Null (not false) is used for undeclared fields so that
// state and plan agree — config also produces null for unconfigured Optional fields,
// and cty treats false ≠ null, which would cause phantom plan diffs.
func refreshBool(declared types.Bool, perm lftypes.Permission, current map[lftypes.Permission]bool, hasAll bool) types.Bool {
	if !declared.ValueBool() {
		return types.BoolNull()
	}
	return types.BoolValue(current[perm] || hasAll)
}
