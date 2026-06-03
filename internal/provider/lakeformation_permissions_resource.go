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

// lfClientIface is the subset of the LF client API used by this resource.
// *lakeformation.Client satisfies it; tests substitute a mock.
type lfClientIface interface {
	GrantPermissions(ctx context.Context, params *lakeformation.GrantPermissionsInput, optFns ...func(*lakeformation.Options)) (*lakeformation.GrantPermissionsOutput, error)
	RevokePermissions(ctx context.Context, params *lakeformation.RevokePermissionsInput, optFns ...func(*lakeformation.Options)) (*lakeformation.RevokePermissionsOutput, error)
	ListPermissions(ctx context.Context, params *lakeformation.ListPermissionsInput, optFns ...func(*lakeformation.Options)) (*lakeformation.ListPermissionsOutput, error)
}

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

func catalogPermAttr(desc string) schema.SingleNestedAttribute {
	b := func(d string) schema.BoolAttribute { return schema.BoolAttribute{Optional: true, MarkdownDescription: d} }
	return schema.SingleNestedAttribute{
		Optional:            true,
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

func databasePermAttr(desc string) schema.SingleNestedAttribute {
	b := func(d string) schema.BoolAttribute { return schema.BoolAttribute{Optional: true, MarkdownDescription: d} }
	return schema.SingleNestedAttribute{
		Optional:            true,
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

func tablePermAttr(desc string) schema.SingleNestedAttribute {
	b := func(d string) schema.BoolAttribute { return schema.BoolAttribute{Optional: true, MarkdownDescription: d} }
	return schema.SingleNestedAttribute{
		Optional:            true,
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

func (r *LakeFormationPermissionsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_lakeformation_permissions"
}

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
				Required:            true,
				MarkdownDescription: "AWS region where the Lake Formation permissions reside.",
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
					"permissions":           catalogPermAttr("Catalog-level permissions to grant."),
					"grantable_permissions": catalogPermAttr("Catalog-level permissions the principal can grant to others."),
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
								"permissions":           databasePermAttr("Database-level permissions to grant."),
								"grantable_permissions": databasePermAttr("Database-level permissions the principal can grant to others."),
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
											"permissions":           tablePermAttr("Table-level permissions to grant."),
											"grantable_permissions": tablePermAttr("Table-level permissions the principal can grant to others."),
										},
									},
								},
								"wildcard": schema.SingleNestedBlock{
									MarkdownDescription: "Permissions on all tables in this database. Mutually exclusive with `table`.",
									Attributes: map[string]schema.Attribute{
										"name":                  schema.StringAttribute{Optional: true, MarkdownDescription: "Must be omitted or empty; present only for struct compatibility with named table entries."},
										"permissions":           tablePermAttr("Table-level permissions to grant on all tables."),
										"grantable_permissions": tablePermAttr("Table-level permissions the principal can grant to others on all tables."),
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

func (v *lfPermissionsValidator) Description(_ context.Context) string {
	return "Validates Lake Formation mutual exclusivity constraints."
}

func (v *lfPermissionsValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

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

	// checkAllExclusive errors if `all` is true alongside any individual permission boolean.
	checkCatalogExclusive := func(p *CatalogPermissions, attrPath path.Path) {
		if p == nil || !p.All.ValueBool() {
			return
		}
		if p.Alter.ValueBool() || p.CreateCatalog.ValueBool() || p.CreateDatabase.ValueBool() ||
			p.Describe.ValueBool() || p.Drop.ValueBool() {
			resp.Diagnostics.AddAttributeError(attrPath.AtName("all"), "Conflicting attributes",
				"Cannot set 'all' alongside individual permission attributes.")
		}
	}
	checkDatabaseExclusive := func(p *DatabasePermissions, attrPath path.Path) {
		if p == nil || !p.All.ValueBool() {
			return
		}
		if p.Alter.ValueBool() || p.CreateTable.ValueBool() || p.Describe.ValueBool() || p.Drop.ValueBool() {
			resp.Diagnostics.AddAttributeError(attrPath.AtName("all"), "Conflicting attributes",
				"Cannot set 'all' alongside individual permission attributes.")
		}
	}
	checkTableExclusive := func(p *TablePermissions, attrPath path.Path) {
		if p == nil || !p.All.ValueBool() {
			return
		}
		if p.Alter.ValueBool() || p.Delete.ValueBool() || p.Describe.ValueBool() ||
			p.Drop.ValueBool() || p.Insert.ValueBool() || p.Select.ValueBool() {
			resp.Diagnostics.AddAttributeError(attrPath.AtName("all"), "Conflicting attributes",
				"Cannot set 'all' alongside individual permission attributes.")
		}
	}

	catPath := path.Root("catalog")
	checkCatalogExclusive(data.Catalog.Permissions, catPath.AtName("permissions"))
	checkCatalogExclusive(data.Catalog.GrantablePermissions, catPath.AtName("grantable_permissions"))

	for i, db := range data.Catalog.Database {
		dbPath := catPath.AtName("database").AtListIndex(i)

		if len(db.Table) > 0 && db.Wildcard != nil {
			resp.Diagnostics.AddAttributeError(dbPath, "Conflicting configuration",
				"A database block cannot specify both table and wildcard.")
		}

		checkDatabaseExclusive(db.Permissions, dbPath.AtName("permissions"))
		checkDatabaseExclusive(db.GrantablePermissions, dbPath.AtName("grantable_permissions"))

		for j, tbl := range db.Table {
			tblPath := dbPath.AtName("table").AtListIndex(j)
			checkTableExclusive(tbl.Permissions, tblPath.AtName("permissions"))
			checkTableExclusive(tbl.GrantablePermissions, tblPath.AtName("grantable_permissions"))
			if tbl.Permissions == nil && tbl.GrantablePermissions == nil {
				resp.Diagnostics.AddAttributeError(tblPath, "Missing required attribute",
					"A table block must specify at least one of 'permissions' or 'grantable_permissions'.")
			}
		}

		if db.Wildcard != nil {
			wcPath := dbPath.AtName("wildcard")
			checkTableExclusive(db.Wildcard.Permissions, wcPath.AtName("permissions"))
			checkTableExclusive(db.Wildcard.GrantablePermissions, wcPath.AtName("grantable_permissions"))
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
			resp.Diagnostics.AddError("Failed to read catalog permissions", err.Error())
			return
		}
		data.Catalog.Permissions = refreshPerms(data.Catalog.Permissions, p)
		data.Catalog.GrantablePermissions = refreshPerms(data.Catalog.GrantablePermissions, g)
	}

	for i, db := range data.Catalog.Database {
		if db.Permissions != nil || db.GrantablePermissions != nil {
			p, g, err := listLFPerms(ctx, client, principal, &lftypes.Resource{
				Database: &lftypes.DatabaseResource{
					CatalogId: aws.String(catalogID),
					Name:      aws.String(db.Name.ValueString()),
				},
			})
			if err != nil {
				resp.Diagnostics.AddError("Failed to read database permissions", err.Error())
				return
			}
			data.Catalog.Database[i].Permissions = refreshPerms(db.Permissions, p)
			data.Catalog.Database[i].GrantablePermissions = refreshPerms(db.GrantablePermissions, g)
		}

		for j, tbl := range db.Table {
			if tbl.Permissions != nil || tbl.GrantablePermissions != nil {
				tp, tg, err := listLFPerms(ctx, client, principal, &lftypes.Resource{
					Table: &lftypes.TableResource{
						CatalogId:    aws.String(catalogID),
						DatabaseName: aws.String(db.Name.ValueString()),
						Name:         aws.String(tbl.Name.ValueString()),
					},
				})
				if err != nil {
					resp.Diagnostics.AddError("Failed to read table permissions", err.Error())
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
					DatabaseName:  aws.String(db.Name.ValueString()),
					TableWildcard: &lftypes.TableWildcard{},
				},
			})
			if err != nil {
				resp.Diagnostics.AddError("Failed to read wildcard permissions", err.Error())
				return
			}
			data.Catalog.Database[i].Wildcard.IsWildcard = true
			data.Catalog.Database[i].Wildcard.Permissions = refreshPerms(db.Wildcard.Permissions, wp)
			data.Catalog.Database[i].Wildcard.GrantablePermissions = refreshPerms(db.Wildcard.GrantablePermissions, wg)
		}
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *LakeFormationPermissionsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var state, plan LakeFormationPermissionsResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client := r.lfClient(plan.Region.ValueString())
	if err := revokeForUpdate(ctx, client, &state, &plan); err != nil {
		resp.Diagnostics.AddError("Failed to revoke previous Lake Formation permissions", err.Error())
		return
	}
	if err := grantAll(ctx, client, &plan); err != nil {
		resp.Diagnostics.AddError("Failed to grant Lake Formation permissions", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *LakeFormationPermissionsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data LakeFormationPermissionsResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client := r.lfClient(data.Region.ValueString())
	if err := revokeForUpdate(ctx, client, &data, &data); err != nil {
		resp.Diagnostics.AddError("Failed to revoke Lake Formation permissions", err.Error())
	}
}

// --- Helpers ---

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
		catalogPermsToAPI(data.Catalog.Permissions),
		catalogPermsToAPI(data.Catalog.GrantablePermissions)); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}

	for _, db := range data.Catalog.Database {
		dbRes := &lftypes.Resource{
			Database: &lftypes.DatabaseResource{
				CatalogId: aws.String(catalogID),
				Name:      aws.String(db.Name.ValueString()),
			},
		}
		if err := grantLFPerms(ctx, client, principal, dbRes,
			databasePermsToAPI(db.Permissions),
			databasePermsToAPI(db.GrantablePermissions)); err != nil {
			return fmt.Errorf("database %s: %w", db.Name.ValueString(), err)
		}

		for _, tbl := range db.Table {
			tblRes := &lftypes.Resource{
				Table: &lftypes.TableResource{
					CatalogId:    aws.String(catalogID),
					DatabaseName: aws.String(db.Name.ValueString()),
					Name:         aws.String(tbl.Name.ValueString()),
				},
			}
			if err := grantLFPerms(ctx, client, principal, tblRes,
				tablePermsToAPI(tbl.Permissions),
				tablePermsToAPI(tbl.GrantablePermissions)); err != nil {
				return fmt.Errorf("table %s.%s: %w", db.Name.ValueString(), tbl.Name.ValueString(), err)
			}
		}

		if db.Wildcard != nil {
			wcRes := &lftypes.Resource{
				Table: &lftypes.TableResource{
					CatalogId:     aws.String(catalogID),
					DatabaseName:  aws.String(db.Name.ValueString()),
					TableWildcard: &lftypes.TableWildcard{},
				},
			}
			if err := grantLFPerms(ctx, client, principal, wcRes,
				tablePermsToAPI(db.Wildcard.Permissions),
				tablePermsToAPI(db.Wildcard.GrantablePermissions)); err != nil {
				return fmt.Errorf("wildcard in database %s: %w", db.Name.ValueString(), err)
			}
		}
	}
	return nil
}

// revokeForUpdate selectively revokes state permissions for resources where plan explicitly declares
// (non-nil) permissions or grantable_permissions. Resources absent from the plan are fully revoked.
// Resources whose fields are nil in the plan are left untouched for those fields.
func revokeForUpdate(ctx context.Context, client lfClientIface, state, plan *LakeFormationPermissionsResourceModel) error {
	if state.Catalog == nil || plan.Catalog == nil {
		return nil
	}
	principal := state.Principal.ValueString()
	catalogID := state.Catalog.ID.ValueString()

	var catP, catG []lftypes.Permission
	if plan.Catalog.Permissions != nil {
		catP = catalogPermsToAPI(state.Catalog.Permissions)
	}
	if plan.Catalog.GrantablePermissions != nil {
		catG = catalogPermsToAPI(state.Catalog.GrantablePermissions)
	}
	if err := revokeLFPerms(ctx, client, principal,
		&lftypes.Resource{Catalog: &lftypes.CatalogResource{}},
		catP, catG); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}

	planDBIdx := make(map[string]DatabasePermModel, len(plan.Catalog.Database))
	for _, db := range plan.Catalog.Database {
		planDBIdx[db.Name.ValueString()] = db
	}

	for _, stDB := range state.Catalog.Database {
		name := stDB.Name.ValueString()
		plDB, inPlan := planDBIdx[name]
		dbRes := &lftypes.Resource{
			Database: &lftypes.DatabaseResource{
				CatalogId: aws.String(catalogID),
				Name:      aws.String(name),
			},
		}

		var dbP, dbG []lftypes.Permission
		if !inPlan || plDB.Permissions != nil {
			dbP = databasePermsToAPI(stDB.Permissions)
		}
		if !inPlan || plDB.GrantablePermissions != nil {
			dbG = databasePermsToAPI(stDB.GrantablePermissions)
		}
		if err := revokeLFPerms(ctx, client, principal, dbRes, dbP, dbG); err != nil {
			return fmt.Errorf("database %s: %w", name, err)
		}

		planTblIdx := make(map[string]TablePermModel)
		if inPlan {
			for _, tbl := range plDB.Table {
				planTblIdx[tbl.Name.ValueString()] = tbl
			}
		}

		for _, stTbl := range stDB.Table {
			tblName := stTbl.Name.ValueString()
			plTbl, tblInPlan := planTblIdx[tblName]
			tblRes := &lftypes.Resource{
				Table: &lftypes.TableResource{
					CatalogId:    aws.String(catalogID),
					DatabaseName: aws.String(name),
					Name:         aws.String(tblName),
				},
			}
			var tP, tG []lftypes.Permission
			if !tblInPlan || plTbl.Permissions != nil {
				tP = tablePermsToAPI(stTbl.Permissions)
			}
			if !tblInPlan || plTbl.GrantablePermissions != nil {
				tG = tablePermsToAPI(stTbl.GrantablePermissions)
			}
			if err := revokeLFPerms(ctx, client, principal, tblRes, tP, tG); err != nil {
				return fmt.Errorf("table %s.%s: %w", name, tblName, err)
			}
		}

		if stDB.Wildcard != nil {
			wcRes := &lftypes.Resource{
				Table: &lftypes.TableResource{
					CatalogId:     aws.String(catalogID),
					DatabaseName:  aws.String(name),
					TableWildcard: &lftypes.TableWildcard{},
				},
			}
			var wP, wG []lftypes.Permission
			if !inPlan || plDB.Wildcard == nil {
				wP = tablePermsToAPI(stDB.Wildcard.Permissions)
				wG = tablePermsToAPI(stDB.Wildcard.GrantablePermissions)
			} else {
				if plDB.Wildcard.Permissions != nil {
					wP = tablePermsToAPI(stDB.Wildcard.Permissions)
				}
				if plDB.Wildcard.GrantablePermissions != nil {
					wG = tablePermsToAPI(stDB.Wildcard.GrantablePermissions)
				}
			}
			if err := revokeLFPerms(ctx, client, principal, wcRes, wP, wG); err != nil {
				return fmt.Errorf("wildcard in database %s: %w", name, err)
			}
		}
	}
	return nil
}

// grantLFPerms calls GrantPermissions for a single principal/resource pair; no-ops when both lists are empty.
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

// catalogPermsToAPI converts to an API permission list.
// all=true sends ALL directly; individual flags collapse to ALL when every one is set.
func catalogPermsToAPI(p *CatalogPermissions) []lftypes.Permission {
	if p == nil {
		return nil
	}
	if p.All.ValueBool() {
		return []lftypes.Permission{lftypes.PermissionAll}
	}
	if p.Alter.ValueBool() && p.CreateCatalog.ValueBool() && p.CreateDatabase.ValueBool() &&
		p.Describe.ValueBool() && p.Drop.ValueBool() {
		return []lftypes.Permission{lftypes.PermissionAll}
	}
	var out []lftypes.Permission
	if p.Alter.ValueBool()          { out = append(out, lftypes.PermissionAlter) }
	if p.CreateCatalog.ValueBool()  { out = append(out, lftypes.PermissionCreateCatalog) }
	if p.CreateDatabase.ValueBool() { out = append(out, lftypes.PermissionCreateDatabase) }
	if p.Describe.ValueBool()       { out = append(out, lftypes.PermissionDescribe) }
	if p.Drop.ValueBool()           { out = append(out, lftypes.PermissionDrop) }
	return out
}

// databasePermsToAPI converts to an API permission list.
// all=true sends ALL directly; individual flags collapse to ALL when every one is set.
func databasePermsToAPI(p *DatabasePermissions) []lftypes.Permission {
	if p == nil {
		return nil
	}
	if p.All.ValueBool() {
		return []lftypes.Permission{lftypes.PermissionAll}
	}
	if p.Alter.ValueBool() && p.CreateTable.ValueBool() && p.Describe.ValueBool() && p.Drop.ValueBool() {
		return []lftypes.Permission{lftypes.PermissionAll}
	}
	var out []lftypes.Permission
	if p.Alter.ValueBool()       { out = append(out, lftypes.PermissionAlter) }
	if p.CreateTable.ValueBool() { out = append(out, lftypes.PermissionCreateTable) }
	if p.Describe.ValueBool()    { out = append(out, lftypes.PermissionDescribe) }
	if p.Drop.ValueBool()        { out = append(out, lftypes.PermissionDrop) }
	return out
}

// tablePermsToAPI converts to an API permission list.
// all=true sends ALL directly; individual flags collapse to ALL when every one is set.
func tablePermsToAPI(p *TablePermissions) []lftypes.Permission {
	if p == nil {
		return nil
	}
	if p.All.ValueBool() {
		return []lftypes.Permission{lftypes.PermissionAll}
	}
	if p.Alter.ValueBool() && p.Delete.ValueBool() && p.Describe.ValueBool() &&
		p.Drop.ValueBool() && p.Insert.ValueBool() && p.Select.ValueBool() {
		return []lftypes.Permission{lftypes.PermissionAll}
	}
	var out []lftypes.Permission
	if p.Alter.ValueBool()    { out = append(out, lftypes.PermissionAlter) }
	if p.Delete.ValueBool()   { out = append(out, lftypes.PermissionDelete) }
	if p.Describe.ValueBool() { out = append(out, lftypes.PermissionDescribe) }
	if p.Drop.ValueBool()     { out = append(out, lftypes.PermissionDrop) }
	if p.Insert.ValueBool()   { out = append(out, lftypes.PermissionInsert) }
	if p.Select.ValueBool()   { out = append(out, lftypes.PermissionSelect) }
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
