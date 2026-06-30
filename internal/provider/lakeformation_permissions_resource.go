// Copyright BrightAI 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	lakeformation "github.com/aws/aws-sdk-go-v2/service/lakeformation"
	lftypes "github.com/aws/aws-sdk-go-v2/service/lakeformation/types"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

var _ resource.Resource = &LakeFormationPermissionsResource{}
var _ resource.ResourceWithConfigValidators = &LakeFormationPermissionsResource{}
var _ resource.ResourceWithImportState = &LakeFormationPermissionsResource{}
var _ resource.ResourceWithModifyPlan = &LakeFormationPermissionsResource{}

const (
	lfPermsMaxRetries  = 4
	lfPermsRetryBaseMs = 1000
	lfPermsRetryMaxMs  = 8000
)

// lfPermsSleepFn is the pause used between concurrency-error retries.
// Replaced with a no-op in unit tests to avoid real sleeps.
var lfPermsSleepFn = func(ctx context.Context, attempt int) error {
	baseMs := lfPermsRetryBaseMs << attempt
	if baseMs > lfPermsRetryMaxMs {
		baseMs = lfPermsRetryMaxMs
	}
	jitterMs := rand.Intn(baseMs/2 + 1)
	select {
	case <-time.After(time.Duration(baseMs+jitterMs) * time.Millisecond):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// isConcurrencyErr reports whether err is a LakeFormation ConcurrentModificationException.
func isConcurrencyErr(err error) bool {
	var e *lftypes.ConcurrentModificationException
	return errors.As(err, &e)
}

// isSuperUserGrantErr reports whether err is the LakeFormation rejection for attempting to
// modify grant options on a SUPER_USER (data-lake-admin) grant. Those grants are managed
// outside Terraform and cannot be individually revoked via RevokePermissions.
func isSuperUserGrantErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Grant options not allowed for SUPER_USER grant")
}

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

// --- Permission struct ---

// Permissions is a unified struct for all Lake Formation permission types.
// All fields default to false when unmarshalled by the framework.
type Permissions struct {
	All            bool `tfsdk:"all"`
	Alter          bool `tfsdk:"alter"`
	CreateCatalog  bool `tfsdk:"create_catalog"`
	CreateDatabase bool `tfsdk:"create_database"`
	CreateTable    bool `tfsdk:"create_table"`
	Delete         bool `tfsdk:"delete"`
	Describe       bool `tfsdk:"describe"`
	Drop           bool `tfsdk:"drop"`
	Insert         bool `tfsdk:"insert"`
	Select         bool `tfsdk:"select"`
}

// --- Resource-type enum and valid-permissions map ---

type LFResourceType string

const (
	LFResourceTypeCatalog  LFResourceType = "catalog"
	LFResourceTypeDatabase LFResourceType = "database"
	LFResourceTypeTable    LFResourceType = "table"
)

// validPermsForType is the set of Lake Formation permissions valid for each resource type.
var validPermsForType = map[LFResourceType]map[lftypes.Permission]bool{
	LFResourceTypeCatalog: {
		lftypes.PermissionAll: true, lftypes.PermissionAlter: true,
		lftypes.PermissionCreateCatalog: true, lftypes.PermissionCreateDatabase: true,
		lftypes.PermissionDescribe: true, lftypes.PermissionDrop: true,
	},
	LFResourceTypeDatabase: {
		lftypes.PermissionAll: true, lftypes.PermissionAlter: true,
		lftypes.PermissionCreateTable: true, lftypes.PermissionDescribe: true,
		lftypes.PermissionDrop: true,
	},
	LFResourceTypeTable: {
		lftypes.PermissionAll: true, lftypes.PermissionAlter: true,
		lftypes.PermissionDelete: true, lftypes.PermissionDescribe: true,
		lftypes.PermissionDrop: true, lftypes.PermissionInsert: true,
		lftypes.PermissionSelect: true,
	},
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
	ID                   types.String      `tfsdk:"id"`
	Permissions          *Permissions      `tfsdk:"permissions"`
	GrantablePermissions *Permissions      `tfsdk:"grantable_permissions"`
	Database             []DatabasePermModel `tfsdk:"database"`
}

type DatabasePermModel struct {
	Name                 types.String    `tfsdk:"name"`
	Permissions          *Permissions    `tfsdk:"permissions"`
	GrantablePermissions *Permissions    `tfsdk:"grantable_permissions"`
	Table                []TablePermModel `tfsdk:"table"`
	Wildcard             *TablePermModel  `tfsdk:"wildcard"`
}

type TablePermModel struct {
	Name                 types.String `tfsdk:"name"`
	IsWildcard           bool         `tfsdk:"-"`
	Permissions          *Permissions `tfsdk:"permissions"`
	GrantablePermissions *Permissions `tfsdk:"grantable_permissions"`
}

// --- Plan modifiers ---

// resolveUnknownToNull collapses an unknown Object plan value to null so that
// ModifyPlan can safely decode the plan and apply its own fill logic without
// hitting decode errors on unknown values.
type resolveUnknownToNull struct{}

var _ planmodifier.Object = resolveUnknownToNull{}

func (resolveUnknownToNull) Description(_ context.Context) string {
	return "Resolves unknown object plan values to null."
}

func (m resolveUnknownToNull) MarkdownDescription(ctx context.Context) string { return m.Description(ctx) }

func (resolveUnknownToNull) PlanModifyObject(ctx context.Context, req planmodifier.ObjectRequest, resp *planmodifier.ObjectResponse) {
	if req.PlanValue.IsUnknown() {
		resp.PlanValue = types.ObjectNull(req.PlanValue.AttributeTypes(ctx))
	}
}

// --- Schema helpers ---

// permAttr returns the schema definition for a permissions block. All ten permission
// fields are present regardless of resource type; validation enforces which are valid
// for each resource type. The outer block is Optional+Computed with nullOrStateForUnknown.
func permAttr(desc string) schema.SingleNestedAttribute {
	b := func(d string) schema.BoolAttribute {
		return schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), MarkdownDescription: d}
	}
	return schema.SingleNestedAttribute{
		Optional:            true,
		Computed:            true,
		MarkdownDescription: desc,
		PlanModifiers:       []planmodifier.Object{resolveUnknownToNull{}},
		Attributes: map[string]schema.Attribute{
			"all":             b("Grants all permissions. Mutually exclusive with individual permission attributes."),
			"alter":           b("Grants ALTER."),
			"create_catalog":  b("Grants CREATE_CATALOG."),
			"create_database": b("Grants CREATE_DATABASE."),
			"create_table":    b("Grants CREATE_TABLE."),
			"delete":          b("Grants DELETE."),
			"describe":        b("Grants DESCRIBE."),
			"drop":            b("Grants DROP."),
			"insert":          b("Grants INSERT."),
			"select":          b("Grants SELECT."),
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
					"permissions":           permAttr("Catalog-level permissions to grant."),
					"grantable_permissions": permAttr("Catalog-level permissions the principal can grant to others."),
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
								"permissions":           permAttr("Database-level permissions to grant."),
								"grantable_permissions": permAttr("Database-level permissions the principal can grant to others."),
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
											"permissions":           permAttr("Table-level permissions to grant."),
											"grantable_permissions": permAttr("Table-level permissions the principal can grant to others."),
										},
									},
								},
								"wildcard": schema.SingleNestedBlock{
									MarkdownDescription: "Permissions on all tables in this database. Mutually exclusive with `table`.",
									Attributes: map[string]schema.Attribute{
										"name":                  schema.StringAttribute{Optional: true, MarkdownDescription: "Must be omitted or empty; present only for struct compatibility with named table entries."},
										"permissions":           permAttr("Table-level permissions to grant on all tables."),
										"grantable_permissions": permAttr("Table-level permissions the principal can grant to others on all tables."),
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
	// Config.Get uses reflect.Options{UnhandledNullAsEmpty: false}, which errors when
	// optional bool fields within a permissions block are not set (null in config).
	// Instead we get the catalog as a types.Object and decode it with
	// UnhandledNullAsEmpty: true so that unset bools are treated as false.
	var catalogObj types.Object
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("catalog"), &catalogObj)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if catalogObj.IsNull() || catalogObj.IsUnknown() {
		resp.Diagnostics.AddError("Missing required block", "A catalog block is required.")
		return
	}

	var catalog CatalogPermModel
	resp.Diagnostics.Append(catalogObj.As(ctx, &catalog, basetypes.ObjectAsOptions{UnhandledNullAsEmpty: true})...)
	if resp.Diagnostics.HasError() {
		return
	}

	catPath := path.Root("catalog")
	checkPerms(catalog.Permissions, LFResourceTypeCatalog, catPath.AtName("permissions"), &resp.Diagnostics)
	checkPerms(catalog.GrantablePermissions, LFResourceTypeCatalog, catPath.AtName("grantable_permissions"), &resp.Diagnostics)
	checkSupersetPerms(catalog.Permissions, catalog.GrantablePermissions, catPath, &resp.Diagnostics)

	for i, db := range catalog.Database {
		dbPath := catPath.AtName("database").AtListIndex(i)

		if len(db.Table) > 0 && db.Wildcard != nil {
			resp.Diagnostics.AddAttributeError(dbPath, "Conflicting configuration",
				"A database block cannot specify both table and wildcard.")
		}

		checkPerms(db.Permissions, LFResourceTypeDatabase, dbPath.AtName("permissions"), &resp.Diagnostics)
		checkPerms(db.GrantablePermissions, LFResourceTypeDatabase, dbPath.AtName("grantable_permissions"), &resp.Diagnostics)
		checkSupersetPerms(db.Permissions, db.GrantablePermissions, dbPath, &resp.Diagnostics)

		for j, tbl := range db.Table {
			tblPath := dbPath.AtName("table").AtListIndex(j)
			checkPerms(tbl.Permissions, LFResourceTypeTable, tblPath.AtName("permissions"), &resp.Diagnostics)
			checkPerms(tbl.GrantablePermissions, LFResourceTypeTable, tblPath.AtName("grantable_permissions"), &resp.Diagnostics)
			checkSupersetPerms(tbl.Permissions, tbl.GrantablePermissions, tblPath, &resp.Diagnostics)
			if tbl.Permissions == nil && tbl.GrantablePermissions == nil {
				resp.Diagnostics.AddAttributeError(tblPath, "Missing required attribute",
					"A table block must specify at least one of 'permissions' or 'grantable_permissions'.")
			}
		}

		if db.Wildcard != nil {
			wcPath := dbPath.AtName("wildcard")
			checkPerms(db.Wildcard.Permissions, LFResourceTypeTable, wcPath.AtName("permissions"), &resp.Diagnostics)
			checkPerms(db.Wildcard.GrantablePermissions, LFResourceTypeTable, wcPath.AtName("grantable_permissions"), &resp.Diagnostics)
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

// checkPerms validates:
//  1. Each selected permission is valid for the given resource type.
//  2. all=true is mutually exclusive with every other flag.
//  3. Selecting every valid individual permission is forbidden — use all=true.
func checkPerms(p *Permissions, rt LFResourceType, attrPath path.Path, diags *diag.Diagnostics) {
	if p == nil {
		return
	}
	valid := validPermsForType[rt]
	perms := permsToAPI(p)

	if p.All {
		if p.Alter || p.CreateCatalog || p.CreateDatabase || p.CreateTable || p.Delete || p.Describe || p.Drop || p.Insert || p.Select {
			diags.AddAttributeError(attrPath.AtName("all"), "Conflicting attributes",
				"Cannot set 'all' alongside individual permission attributes.")
		}
		return
	}

	for _, perm := range perms {
		if !valid[perm] {
			name := strings.ToLower(string(perm))
			diags.AddAttributeError(attrPath.AtName(name), "Invalid permission",
				fmt.Sprintf("%s is not valid for %s resources.", perm, rt))
		}
	}

	// Count valid individual permissions (excluding ALL itself).
	validIndividual := len(valid) - 1
	if len(perms) > 0 && len(perms) == validIndividual {
		diags.AddAttributeError(attrPath, "Implicit ALL not permitted",
			"To grant every permission use all = true instead of setting every individual flag.")
	}
}

// checkSupersetPerms validates that perms is a superset of grantPerms. Skipped when perms is
// nil (it will be computed from grantPerms by ModifyPlan) or when perms contains ALL (which
// is a superset of every possible grantable_permissions value).
func checkSupersetPerms(perms, grantPerms *Permissions, parentPath path.Path, diags *diag.Diagnostics) {
	if perms == nil {
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
			name := strings.ToLower(string(gp))
			diags.AddAttributeError(
				parentPath.AtName("grantable_permissions").AtName(name),
				"Permission not in 'permissions'",
				fmt.Sprintf("'%s' is in 'grantable_permissions' but not in 'permissions': "+
					"when both are specified, 'permissions' must contain every permission listed in 'grantable_permissions'.", name),
			)
			return
		}
	}
}

// ModifyPlan resolves computed plan values so that no unknowns reach the apply phase.
// The resolveUnknownToNull plan modifier on each permissions attribute has already
// collapsed unknown objects to null before this runs, so nil means "not set in config".
//
//  1. Region — resolved from the provider config or environment when not set explicitly.
//  2. permissions / grantable_permissions pairs — filled according to these rules:
//     - both nil → leave nil (apply will skip this resource entirely)
//     - permissions nil, grantable set → permissions = copy of grantable (superset rule)
//     - permissions set, grantable nil → grantable = empty &Permissions{} (no grant options)
//     - both set → no change
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

// defaultPermissions fills each permissions/grantable_permissions pair in the plan.
// resolveUnknownToNull has already turned unknown objects into null (nil pointers),
// so nil means "not set in config". Rules:
//   - both nil → leave nil; apply will skip this resource.
//   - permissions nil, grantable set → permissions = copy of grantable (superset rule).
//   - permissions set, grantable nil → grantable = empty &Permissions{} (no grant options).
//   - both set → no change.
//
// Returns true if any plan field was changed.
func defaultPermissions(plan *LakeFormationPermissionsResourceModel) bool {
	if plan.Catalog == nil {
		return false
	}
	changed := fillPermPair(&plan.Catalog.Permissions, &plan.Catalog.GrantablePermissions)
	for i := range plan.Catalog.Database {
		db := &plan.Catalog.Database[i]
		changed = fillPermPair(&db.Permissions, &db.GrantablePermissions) || changed
		for j := range db.Table {
			changed = fillPermPair(&db.Table[j].Permissions, &db.Table[j].GrantablePermissions) || changed
		}
		if db.Wildcard != nil {
			changed = fillPermPair(&db.Wildcard.Permissions, &db.Wildcard.GrantablePermissions) || changed
		}
	}
	return changed
}

// fillPermPair applies the nil-based fill rules for one permissions/grantable pair.
func fillPermPair(perms, grantPerms **Permissions) bool {
	switch {
	case *perms == nil && *grantPerms == nil:
		return false
	case *perms != nil && *grantPerms == nil:
		*grantPerms = &Permissions{}
		return true
	case *perms == nil: // grantPerms non-nil
		cp := **grantPerms
		*perms = &cp
		return true
	default:
		return false
	}
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

	client := r.lfClient(data.Region.ValueString())
	if err := readPermissions(ctx, client, &data); err != nil {
		resp.Diagnostics.AddError("Failed to read Lake Formation permissions", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// readPermissions refreshes data in-place with the current AWS state for every declared
// resource. Resources whose Permissions pointer is nil are skipped (nil = unmanaged).
func readPermissions(ctx context.Context, client lfClientIface, data *LakeFormationPermissionsResourceModel) error {
	if data.Catalog == nil {
		return nil
	}

	principal := data.Principal.ValueString()
	catalogID := data.Catalog.ID.ValueString()

	if data.Catalog.Permissions != nil {
		p, g, err := listLFPerms(ctx, client, principal, &lftypes.Resource{Catalog: &lftypes.CatalogResource{Id: aws.String(catalogID)}})
		if err != nil {
			return fmt.Errorf("principal=%s catalog=%s: %w", principal, catalogID, err)
		}
		data.Catalog.Permissions = refreshPerms(data.Catalog.Permissions, p)
		data.Catalog.GrantablePermissions = refreshPerms(data.Catalog.GrantablePermissions, g)
	}

	for i, db := range data.Catalog.Database {
		dbName := db.Name.ValueString()
		if db.Permissions != nil {
			p, g, err := listLFPerms(ctx, client, principal, &lftypes.Resource{
				Database: &lftypes.DatabaseResource{
					CatalogId: aws.String(catalogID),
					Name:      aws.String(dbName),
				},
			})
			if err != nil {
				return fmt.Errorf("principal=%s catalog=%s database=%s: %w", principal, catalogID, dbName, err)
			}
			data.Catalog.Database[i].Permissions = refreshPerms(db.Permissions, p)
			data.Catalog.Database[i].GrantablePermissions = refreshPerms(db.GrantablePermissions, g)
		}

		for j, tbl := range db.Table {
			tblName := tbl.Name.ValueString()
			tp, tg, err := listLFPerms(ctx, client, principal, &lftypes.Resource{
				Table: &lftypes.TableResource{
					CatalogId:    aws.String(catalogID),
					DatabaseName: aws.String(dbName),
					Name:         aws.String(tblName),
				},
			})
			if err != nil {
				return fmt.Errorf("principal=%s catalog=%s database=%s table=%s: %w", principal, catalogID, dbName, tblName, err)
			}
			data.Catalog.Database[i].Table[j].Permissions = refreshPerms(tbl.Permissions, tp)
			data.Catalog.Database[i].Table[j].GrantablePermissions = refreshPerms(tbl.GrantablePermissions, tg)
		}

		if db.Wildcard != nil {
			wp, wg, err := listLFPerms(ctx, client, principal, &lftypes.Resource{
				Table: &lftypes.TableResource{
					CatalogId:     aws.String(catalogID),
					DatabaseName:  aws.String(dbName),
					TableWildcard: &lftypes.TableWildcard{},
				},
			})
			if err != nil {
				return fmt.Errorf("principal=%s catalog=%s database=%s wildcard: %w", principal, catalogID, dbName, err)
			}
			data.Catalog.Database[i].Wildcard.IsWildcard = true
			data.Catalog.Database[i].Wildcard.Permissions = refreshPerms(db.Wildcard.Permissions, wp)
			data.Catalog.Database[i].Wildcard.GrantablePermissions = refreshPerms(db.Wildcard.GrantablePermissions, wg)
		}
	}

	return nil
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

	if data.Catalog.Permissions != nil {
		if err := applyResource(ctx, client, principal,
			&lftypes.Resource{Catalog: &lftypes.CatalogResource{Id: aws.String(catalogID)}},
			permsToAPI(data.Catalog.Permissions), permsToAPI(data.Catalog.GrantablePermissions)); err != nil {
			return fmt.Errorf("principal=%s catalog=%s: %w", principal, catalogID, err)
		}
	}

	for _, db := range data.Catalog.Database {
		dbName := db.Name.ValueString()
		dbRes := &lftypes.Resource{Database: &lftypes.DatabaseResource{
			CatalogId: aws.String(catalogID),
			Name:      aws.String(dbName),
		}}
		if db.Permissions != nil {
			if err := applyResource(ctx, client, principal, dbRes,
				permsToAPI(db.Permissions), permsToAPI(db.GrantablePermissions)); err != nil {
				return fmt.Errorf("principal=%s catalog=%s database=%s: %w", principal, catalogID, dbName, err)
			}
		}

		for _, tbl := range db.Table {
			tblName := tbl.Name.ValueString()
			tblRes := &lftypes.Resource{Table: &lftypes.TableResource{
				CatalogId:    aws.String(catalogID),
				DatabaseName: aws.String(dbName),
				Name:         aws.String(tblName),
			}}
			if err := applyResource(ctx, client, principal, tblRes,
				permsToAPI(tbl.Permissions), permsToAPI(tbl.GrantablePermissions)); err != nil {
				return fmt.Errorf("principal=%s catalog=%s database=%s table=%s: %w", principal, catalogID, dbName, tblName, err)
			}
		}

		if db.Wildcard != nil {
			wcRes := &lftypes.Resource{Table: &lftypes.TableResource{
				CatalogId:     aws.String(catalogID),
				DatabaseName:  aws.String(dbName),
				TableWildcard: &lftypes.TableWildcard{},
			}}
			if err := applyResource(ctx, client, principal, wcRes,
				permsToAPI(db.Wildcard.Permissions), permsToAPI(db.Wildcard.GrantablePermissions)); err != nil {
				return fmt.Errorf("principal=%s catalog=%s database=%s wildcard: %w", principal, catalogID, dbName, err)
			}
		}
	}
	return nil
}

// deletePermissions revokes every permission that was explicitly declared in data.
func deletePermissions(ctx context.Context, client lfClientIface, data *LakeFormationPermissionsResourceModel) error {
	if data.Catalog == nil {
		return nil
	}
	principal := data.Principal.ValueString()
	catalogID := data.Catalog.ID.ValueString()

	if data.Catalog.Permissions != nil {
		if err := applyResource(ctx, client, principal,
			&lftypes.Resource{Catalog: &lftypes.CatalogResource{Id: aws.String(catalogID)}}, nil, nil); err != nil {
			return fmt.Errorf("principal=%s catalog=%s: %w", principal, catalogID, err)
		}
	}

	for _, db := range data.Catalog.Database {
		dbName := db.Name.ValueString()

		if db.Permissions != nil {
			dbRes := &lftypes.Resource{Database: &lftypes.DatabaseResource{
				CatalogId: aws.String(catalogID),
				Name:      aws.String(dbName),
			}}
			if err := applyResource(ctx, client, principal, dbRes, nil, nil); err != nil {
				return fmt.Errorf("principal=%s catalog=%s database=%s: %w", principal, catalogID, dbName, err)
			}
		}

		for _, tbl := range db.Table {
			tblRes := &lftypes.Resource{Table: &lftypes.TableResource{
				CatalogId:    aws.String(catalogID),
				DatabaseName: aws.String(dbName),
				Name:         aws.String(tbl.Name.ValueString()),
			}}
			if err := applyResource(ctx, client, principal, tblRes, nil, nil); err != nil {
				return fmt.Errorf("principal=%s catalog=%s database=%s table=%s: %w", principal, catalogID, dbName, tbl.Name.ValueString(), err)
			}
		}

		if db.Wildcard != nil {
			wcRes := &lftypes.Resource{Table: &lftypes.TableResource{
				CatalogId:     aws.String(catalogID),
				DatabaseName:  aws.String(dbName),
				TableWildcard: &lftypes.TableWildcard{},
			}}
			if err := applyResource(ctx, client, principal, wcRes, nil, nil); err != nil {
				return fmt.Errorf("principal=%s catalog=%s database=%s wildcard: %w", principal, catalogID, dbName, err)
			}
		}
	}
	return nil
}

// permSetsEqual reports whether a and b contain the same permissions (order-independent).
func permSetsEqual(a, b []lftypes.Permission) bool {
	if len(a) != len(b) {
		return false
	}
	s := permSet(a)
	for _, p := range b {
		if !s[p] {
			return false
		}
	}
	return true
}

// needsUpdate reports whether a resource's permissions differ between state and plan.
// Returns false when both plan pointers are nil — nil means "not managed, leave unchanged".
func needsUpdate(stP, plP, stG, plG *Permissions) bool {
	if plP == nil && plG == nil {
		return false
	}
	return !permSetsEqual(permsToAPI(stP), permsToAPI(plP)) ||
		!permSetsEqual(permsToAPI(stG), permsToAPI(plG))
}

// applyDiff applies the minimum revoke/grant operations to bring an AWS resource from
// curP/curG to planP/planG. When ALL appears on either side it uses a full revoke+grant
// cycle. For permissions that stay in planP but lose their grant option, it uses a
// revoke+regrant: AWS requires Permissions to be non-empty in RevokePermissions, so a lone
// PermissionsWithGrantOption entry is not valid. The revoke+regrant is issued as a separate
// call from the main revoke so that a SUPER_USER grant error (externally-managed grant
// options that cannot be revoked individually) can be caught and skipped without aborting
// the rest of the diff.
func applyDiff(ctx context.Context, client lfClientIface, principal string, res *lftypes.Resource,
	curP, planP, curG, planG []lftypes.Permission) error {

	if len(curP) == 0 && len(planP) == 0 {
		return nil
	}

	if containsPermission(planP, lftypes.PermissionAll) || containsPermission(curP, lftypes.PermissionAll) {
		if err := revokeLFPerms(ctx, client, principal, res, curP, curG); err != nil {
			return err
		}
		return grantLFPerms(ctx, client, principal, res, planP, planG)
	}

	revokeG := setSubtract(curG, planG)
	grantP := setSubtract(planP, curP)
	grantG := setSubtract(planG, curG)

	// Revoke permissions being fully removed (not staying in the plan).
	revokeOnlyP := setSubtract(curP, planP)
	revokeOnlyG := intersect(revokeG, revokeOnlyP)
	if len(revokeOnlyP) > 0 || len(revokeOnlyG) > 0 {
		if err := revokeLFPerms(ctx, client, principal, res, revokeOnlyP, revokeOnlyG); err != nil {
			return err
		}
	}

	// Permissions staying in planP that are losing their grant option require a separate
	// revoke+regrant. If AWS rejects with a SUPER_USER grant error the grant option is
	// externally managed and cannot be touched; skip rather than fail.
	if keepRevokeG := intersect(revokeG, planP); len(keepRevokeG) > 0 {
		err := revokeLFPerms(ctx, client, principal, res, keepRevokeG, keepRevokeG)
		if err == nil {
			grantP = permUnion(grantP, keepRevokeG)
		} else if !isSuperUserGrantErr(err) {
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

// applyResource reads the current permissions from AWS and applies the minimum changes to
// reach planP/planG. Retries automatically on ConcurrentModificationException.
func applyResource(ctx context.Context, client lfClientIface, principal string, res *lftypes.Resource,
	planP, planG []lftypes.Permission) error {

	for attempt := 0; ; attempt++ {
		curP, curG, err := listLFPerms(ctx, client, principal, res)
		if err != nil {
			return err
		}
		err = applyDiff(ctx, client, principal, res, curP, planP, curG, planG)
		if err == nil || !isConcurrencyErr(err) || attempt >= lfPermsMaxRetries {
			return err
		}
		if sleepErr := lfPermsSleepFn(ctx, attempt); sleepErr != nil {
			return err
		}
	}
}

// updatePermissions compares state to plan and, for each resource that has changed,
// reads the current AWS permissions and applies the minimum changes needed.
func updatePermissions(ctx context.Context, client lfClientIface, state, plan *LakeFormationPermissionsResourceModel) error {
	if plan.Catalog == nil {
		return nil
	}
	principal := plan.Principal.ValueString()
	catalogID := plan.Catalog.ID.ValueString()

	var stateCat *CatalogPermModel
	if state != nil {
		stateCat = state.Catalog
	}

	// Catalog-level.
	var stCP, stCG *Permissions
	if stateCat != nil {
		stCP, stCG = stateCat.Permissions, stateCat.GrantablePermissions
	}
	if needsUpdate(stCP, plan.Catalog.Permissions, stCG, plan.Catalog.GrantablePermissions) {
		if err := applyResource(ctx, client, principal,
			&lftypes.Resource{Catalog: &lftypes.CatalogResource{Id: aws.String(catalogID)}},
			permsToAPI(plan.Catalog.Permissions), permsToAPI(plan.Catalog.GrantablePermissions)); err != nil {
			return fmt.Errorf("principal=%s catalog=%s: %w", principal, catalogID, err)
		}
	}

	// Index state databases.
	stateDBs := make(map[string]DatabasePermModel)
	if stateCat != nil {
		for _, db := range stateCat.Database {
			stateDBs[db.Name.ValueString()] = db
		}
	}

	// Apply plan databases.
	for _, plDB := range plan.Catalog.Database {
		name := plDB.Name.ValueString()
		stDB := stateDBs[name]

		dbRes := &lftypes.Resource{Database: &lftypes.DatabaseResource{
			CatalogId: aws.String(catalogID),
			Name:      aws.String(name),
		}}
		if needsUpdate(stDB.Permissions, plDB.Permissions, stDB.GrantablePermissions, plDB.GrantablePermissions) {
			if err := applyResource(ctx, client, principal, dbRes,
				permsToAPI(plDB.Permissions), permsToAPI(plDB.GrantablePermissions)); err != nil {
				return fmt.Errorf("principal=%s catalog=%s database=%s: %w", principal, catalogID, name, err)
			}
		}

		// Named tables.
		stTbls := make(map[string]TablePermModel)
		for _, tbl := range stDB.Table {
			stTbls[tbl.Name.ValueString()] = tbl
		}
		for _, plTbl := range plDB.Table {
			tblName := plTbl.Name.ValueString()
			stTbl := stTbls[tblName]
			tblRes := &lftypes.Resource{Table: &lftypes.TableResource{
				CatalogId:    aws.String(catalogID),
				DatabaseName: aws.String(name),
				Name:         aws.String(tblName),
			}}
			if needsUpdate(stTbl.Permissions, plTbl.Permissions, stTbl.GrantablePermissions, plTbl.GrantablePermissions) {
				if err := applyResource(ctx, client, principal, tblRes,
					permsToAPI(plTbl.Permissions), permsToAPI(plTbl.GrantablePermissions)); err != nil {
					return fmt.Errorf("principal=%s catalog=%s database=%s table=%s: %w", principal, catalogID, name, tblName, err)
				}
			}
		}

		// Wildcard.
		var stWP, stWG, plWP, plWG *Permissions
		if stDB.Wildcard != nil {
			stWP, stWG = stDB.Wildcard.Permissions, stDB.Wildcard.GrantablePermissions
		}
		if plDB.Wildcard != nil {
			plWP, plWG = plDB.Wildcard.Permissions, plDB.Wildcard.GrantablePermissions
		}
		if needsUpdate(stWP, plWP, stWG, plWG) {
			wcRes := &lftypes.Resource{Table: &lftypes.TableResource{
				CatalogId:     aws.String(catalogID),
				DatabaseName:  aws.String(name),
				TableWildcard: &lftypes.TableWildcard{},
			}}
			if err := applyResource(ctx, client, principal, wcRes,
				permsToAPI(plWP), permsToAPI(plWG)); err != nil {
				return fmt.Errorf("principal=%s catalog=%s database=%s wildcard: %w", principal, catalogID, name, err)
			}
		}
	}

	return nil
}

// permUnion returns a with any elements of b that are not already present appended.
func permUnion(a, b []lftypes.Permission) []lftypes.Permission {
	if len(b) == 0 {
		return a
	}
	extra := setSubtract(b, a)
	if len(extra) == 0 {
		return a
	}
	return append(a, extra...)
}

// intersect returns elements present in both a and b.
func intersect(a, b []lftypes.Permission) []lftypes.Permission {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	bs := permSet(b)
	var out []lftypes.Permission
	for _, p := range a {
		if bs[p] {
			out = append(out, p)
		}
	}
	return out
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

// grantLFPerms calls GrantPermissions for a single principal/resource pair; no-ops when both lists are empty.
// AWS requires Permissions ⊇ PermissionsWithGrantOption and Permissions to be non-empty when
// PermissionsWithGrantOption is set. When the diff produces only new grant options (regular
// permission already held), grantPerms entries are merged into perms before the API call.
// Re-granting an already-held permission is idempotent in LakeFormation.
func grantLFPerms(ctx context.Context, client lfClientIface, principal string, res *lftypes.Resource, perms, grantPerms []lftypes.Permission) error {
	if len(perms) == 0 && len(grantPerms) == 0 {
		return nil
	}
	perms = permUnion(perms, grantPerms)
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
	rt := resourceLFType(res)
	return collapseImplicitAll(perms, rt), collapseImplicitAll(grantPerms, rt), nil
}

// resourceLFType infers the LFResourceType from a Resource descriptor.
func resourceLFType(res *lftypes.Resource) LFResourceType {
	switch {
	case res.Database != nil:
		return LFResourceTypeDatabase
	case res.Table != nil:
		return LFResourceTypeTable
	default:
		return LFResourceTypeCatalog
	}
}

// collapseImplicitAll replaces a full set of individual permissions with [ALL].
// AWS can return all individual permissions in place of the literal ALL permission
// when grants from multiple sources combine. Collapsing them prevents spurious
// plan diffs and state drift when the Terraform config uses all=true.
func collapseImplicitAll(perms []lftypes.Permission, rt LFResourceType) []lftypes.Permission {
	if len(perms) == 0 || containsPermission(perms, lftypes.PermissionAll) {
		return perms
	}
	valid := validPermsForType[rt]
	required := len(valid) - 1 // exclude ALL itself
	if len(perms) < required {
		return perms
	}
	s := permSet(perms)
	for p := range valid {
		if p != lftypes.PermissionAll && !s[p] {
			return perms
		}
	}
	return []lftypes.Permission{lftypes.PermissionAll}
}

// --- Permission struct ↔ API conversions ---

// permsToAPI converts a *Permissions struct to an API permission list.
// If All is true it returns [ALL] immediately.
func permsToAPI(p *Permissions) []lftypes.Permission {
	if p == nil {
		return nil
	}
	if p.All {
		return []lftypes.Permission{lftypes.PermissionAll}
	}
	var out []lftypes.Permission
	if p.Alter {
		out = append(out, lftypes.PermissionAlter)
	}
	if p.CreateCatalog {
		out = append(out, lftypes.PermissionCreateCatalog)
	}
	if p.CreateDatabase {
		out = append(out, lftypes.PermissionCreateDatabase)
	}
	if p.CreateTable {
		out = append(out, lftypes.PermissionCreateTable)
	}
	if p.Delete {
		out = append(out, lftypes.PermissionDelete)
	}
	if p.Describe {
		out = append(out, lftypes.PermissionDescribe)
	}
	if p.Drop {
		out = append(out, lftypes.PermissionDrop)
	}
	if p.Insert {
		out = append(out, lftypes.PermissionInsert)
	}
	if p.Select {
		out = append(out, lftypes.PermissionSelect)
	}
	return out
}

// refreshPerms returns a new Permissions struct reflecting which declared permissions are
// currently active. ALL in current sets every declared individual flag true. The "all" field
// itself is refreshed: if the user declared all=true and ALL is still active in AWS, it stays true.
func refreshPerms(declared *Permissions, current []lftypes.Permission) *Permissions {
	if declared == nil {
		return nil
	}
	s := permSet(current)
	hasAll := s[lftypes.PermissionAll]
	active := func(p lftypes.Permission) bool { return s[p] || hasAll }
	return &Permissions{
		All:            declared.All && active(lftypes.PermissionAll),
		Alter:          declared.Alter && active(lftypes.PermissionAlter),
		CreateCatalog:  declared.CreateCatalog && active(lftypes.PermissionCreateCatalog),
		CreateDatabase: declared.CreateDatabase && active(lftypes.PermissionCreateDatabase),
		CreateTable:    declared.CreateTable && active(lftypes.PermissionCreateTable),
		Delete:         declared.Delete && active(lftypes.PermissionDelete),
		Describe:       declared.Describe && active(lftypes.PermissionDescribe),
		Drop:           declared.Drop && active(lftypes.PermissionDrop),
		Insert:         declared.Insert && active(lftypes.PermissionInsert),
		Select:         declared.Select && active(lftypes.PermissionSelect),
	}
}

// permSet converts a permission slice to a set for O(1) membership checks.
func permSet(perms []lftypes.Permission) map[lftypes.Permission]bool {
	s := make(map[lftypes.Permission]bool, len(perms))
	for _, p := range perms {
		s[p] = true
	}
	return s
}
