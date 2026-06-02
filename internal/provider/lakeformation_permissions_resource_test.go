// Copyright BrightAI 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	lakeformation "github.com/aws/aws-sdk-go-v2/service/lakeformation"
	lftypes "github.com/aws/aws-sdk-go-v2/service/lakeformation/types"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// ── Mock LF client ────────────────────────────────────────────────────────────

type mockLFClient struct {
	grantCalls  []*lakeformation.GrantPermissionsInput
	revokeCalls []*lakeformation.RevokePermissionsInput
	listResult  []lftypes.PrincipalResourcePermissions
	grantErr    error
	revokeErr   error
	listErr     error
}

func (m *mockLFClient) GrantPermissions(_ context.Context, params *lakeformation.GrantPermissionsInput, _ ...func(*lakeformation.Options)) (*lakeformation.GrantPermissionsOutput, error) {
	m.grantCalls = append(m.grantCalls, params)
	return &lakeformation.GrantPermissionsOutput{}, m.grantErr
}

func (m *mockLFClient) RevokePermissions(_ context.Context, params *lakeformation.RevokePermissionsInput, _ ...func(*lakeformation.Options)) (*lakeformation.RevokePermissionsOutput, error) {
	m.revokeCalls = append(m.revokeCalls, params)
	return &lakeformation.RevokePermissionsOutput{}, m.revokeErr
}

func (m *mockLFClient) ListPermissions(_ context.Context, _ *lakeformation.ListPermissionsInput, _ ...func(*lakeformation.Options)) (*lakeformation.ListPermissionsOutput, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return &lakeformation.ListPermissionsOutput{PrincipalResourcePermissions: m.listResult}, nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func permsEqual(a, b []lftypes.Permission) bool {
	if len(a) != len(b) {
		return false
	}
	sa, sb := permSet(a), permSet(b)
	for k := range sa {
		if !sb[k] {
			return false
		}
	}
	return true
}

// findGrantCall returns the first call whose Resource matches the provided matcher.
func findGrantCall(calls []*lakeformation.GrantPermissionsInput, match func(*lftypes.Resource) bool) *lakeformation.GrantPermissionsInput {
	for _, c := range calls {
		if match(c.Resource) {
			return c
		}
	}
	return nil
}

func findRevokeCall(calls []*lakeformation.RevokePermissionsInput, match func(*lftypes.Resource) bool) *lakeformation.RevokePermissionsInput {
	for _, c := range calls {
		if match(c.Resource) {
			return c
		}
	}
	return nil
}

func isCatalogResource(r *lftypes.Resource) bool  { return r != nil && r.Catalog != nil }
func isDatabaseResource(name string) func(*lftypes.Resource) bool {
	return func(r *lftypes.Resource) bool {
		return r != nil && r.Database != nil && aws.ToString(r.Database.Name) == name
	}
}
func isTableResource(db, tbl string) func(*lftypes.Resource) bool {
	return func(r *lftypes.Resource) bool {
		return r != nil && r.Table != nil &&
			aws.ToString(r.Table.DatabaseName) == db &&
			aws.ToString(r.Table.Name) == tbl &&
			r.Table.TableWildcard == nil
	}
}
func isWildcardResource(db string) func(*lftypes.Resource) bool {
	return func(r *lftypes.Resource) bool {
		return r != nil && r.Table != nil &&
			aws.ToString(r.Table.DatabaseName) == db &&
			r.Table.TableWildcard != nil
	}
}

// ── Permissions → API conversion ──────────────────────────────────────────────

func TestCatalogPermsToAPI(t *testing.T) {
	tests := []struct {
		name  string
		input *CatalogPermissions
		want  []lftypes.Permission
	}{
		{
			name:  "nil",
			input: nil,
			want:  nil,
		},
		{
			name:  "all_false",
			input: &CatalogPermissions{},
			want:  nil,
		},
		{
			name:  "create_database_only",
			input: &CatalogPermissions{CreateDatabase: types.BoolValue(true)},
			want:  []lftypes.Permission{lftypes.PermissionCreateDatabase},
		},
		{
			name: "describe_and_alter",
			input: &CatalogPermissions{
				Describe: types.BoolValue(true),
				Alter:    types.BoolValue(true),
			},
			want: []lftypes.Permission{lftypes.PermissionAlter, lftypes.PermissionDescribe},
		},
		{
			name: "all_individual_true_collapses_to_ALL",
			input: &CatalogPermissions{
				Alter:          types.BoolValue(true),
				CreateCatalog:  types.BoolValue(true),
				CreateDatabase: types.BoolValue(true),
				Describe:       types.BoolValue(true),
				Drop:           types.BoolValue(true),
			},
			want: []lftypes.Permission{lftypes.PermissionAll},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := catalogPermsToAPI(tt.input)
			if !permsEqual(got, tt.want) {
				t.Errorf("catalogPermsToAPI() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDatabasePermsToAPI(t *testing.T) {
	tests := []struct {
		name  string
		input *DatabasePermissions
		want  []lftypes.Permission
	}{
		{name: "nil", input: nil, want: nil},
		{name: "all_false", input: &DatabasePermissions{}, want: nil},
		{
			name:  "describe_only",
			input: &DatabasePermissions{Describe: types.BoolValue(true)},
			want:  []lftypes.Permission{lftypes.PermissionDescribe},
		},
		{
			name:  "create_table_and_drop",
			input: &DatabasePermissions{CreateTable: types.BoolValue(true), Drop: types.BoolValue(true)},
			want:  []lftypes.Permission{lftypes.PermissionCreateTable, lftypes.PermissionDrop},
		},
		{
			name: "all_individual_true_collapses_to_ALL",
			input: &DatabasePermissions{
				Alter:       types.BoolValue(true),
				CreateTable: types.BoolValue(true),
				Describe:    types.BoolValue(true),
				Drop:        types.BoolValue(true),
			},
			want: []lftypes.Permission{lftypes.PermissionAll},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := databasePermsToAPI(tt.input)
			if !permsEqual(got, tt.want) {
				t.Errorf("databasePermsToAPI() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTablePermsToAPI(t *testing.T) {
	tests := []struct {
		name  string
		input *TablePermissions
		want  []lftypes.Permission
	}{
		{name: "nil", input: nil, want: nil},
		{name: "all_false", input: &TablePermissions{}, want: nil},
		{
			name:  "select_only",
			input: &TablePermissions{Select: types.BoolValue(true)},
			want:  []lftypes.Permission{lftypes.PermissionSelect},
		},
		{
			name: "select_and_describe",
			input: &TablePermissions{
				Select:   types.BoolValue(true),
				Describe: types.BoolValue(true),
			},
			want: []lftypes.Permission{lftypes.PermissionDescribe, lftypes.PermissionSelect},
		},
		{
			name: "insert_and_delete",
			input: &TablePermissions{
				Insert: types.BoolValue(true),
				Delete: types.BoolValue(true),
			},
			want: []lftypes.Permission{lftypes.PermissionDelete, lftypes.PermissionInsert},
		},
		{
			name: "all_individual_true_collapses_to_ALL",
			input: &TablePermissions{
				Alter:    types.BoolValue(true),
				Delete:   types.BoolValue(true),
				Describe: types.BoolValue(true),
				Drop:     types.BoolValue(true),
				Insert:   types.BoolValue(true),
				Select:   types.BoolValue(true),
			},
			want: []lftypes.Permission{lftypes.PermissionAll},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tablePermsToAPI(tt.input)
			if !permsEqual(got, tt.want) {
				t.Errorf("tablePermsToAPI() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ── API response → permission struct ─────────────────────────────────────────

// ── refreshPerms ─────────────────────────────────────────────────────────────

func TestRefreshPerms(t *testing.T) {
	// refreshBool always emits explicit true/false; zero-value types.Bool (null) must not appear.
	f := types.BoolValue(false)

	t.Run("catalog", func(t *testing.T) {
		t.Run("nil_returns_nil", func(t *testing.T) {
			if refreshPerms[CatalogPermissions](nil, nil) != nil {
				t.Error("expected nil")
			}
		})
		t.Run("declared_perm_present_in_current", func(t *testing.T) {
			got := refreshPerms(&CatalogPermissions{CreateDatabase: types.BoolValue(true)},
				[]lftypes.Permission{lftypes.PermissionCreateDatabase})
			want := &CatalogPermissions{Alter: f, CreateCatalog: f, CreateDatabase: types.BoolValue(true), Describe: f, Drop: f}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("declared_perm_absent_becomes_false", func(t *testing.T) {
			got := refreshPerms(&CatalogPermissions{CreateDatabase: types.BoolValue(true), Describe: types.BoolValue(true)},
				[]lftypes.Permission{lftypes.PermissionCreateDatabase})
			want := &CatalogPermissions{Alter: f, CreateCatalog: f, CreateDatabase: types.BoolValue(true), Describe: f, Drop: f}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("ALL_sets_all_declared_true", func(t *testing.T) {
			got := refreshPerms(&CatalogPermissions{Alter: types.BoolValue(true), CreateDatabase: types.BoolValue(true)},
				[]lftypes.Permission{lftypes.PermissionAll})
			want := &CatalogPermissions{Alter: types.BoolValue(true), CreateCatalog: f, CreateDatabase: types.BoolValue(true), Describe: f, Drop: f}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("undeclared_perms_not_tracked", func(t *testing.T) {
			got := refreshPerms(&CatalogPermissions{},
				[]lftypes.Permission{lftypes.PermissionCreateDatabase, lftypes.PermissionDescribe})
			want := &CatalogPermissions{Alter: f, CreateCatalog: f, CreateDatabase: f, Describe: f, Drop: f}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	})

	t.Run("database", func(t *testing.T) {
		t.Run("nil_returns_nil", func(t *testing.T) {
			if refreshPerms[DatabasePermissions](nil, nil) != nil {
				t.Error("expected nil")
			}
		})
		t.Run("describe_present", func(t *testing.T) {
			got := refreshPerms(&DatabasePermissions{Describe: types.BoolValue(true)},
				[]lftypes.Permission{lftypes.PermissionDescribe})
			want := &DatabasePermissions{Alter: f, CreateTable: f, Describe: types.BoolValue(true), Drop: f}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("perm_revoked_externally", func(t *testing.T) {
			got := refreshPerms(&DatabasePermissions{CreateTable: types.BoolValue(true), Alter: types.BoolValue(true)},
				[]lftypes.Permission{lftypes.PermissionAlter})
			want := &DatabasePermissions{Alter: types.BoolValue(true), CreateTable: f, Describe: f, Drop: f}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("ALL_expands_to_declared", func(t *testing.T) {
			got := refreshPerms(&DatabasePermissions{Alter: types.BoolValue(true), Drop: types.BoolValue(true)},
				[]lftypes.Permission{lftypes.PermissionAll})
			want := &DatabasePermissions{Alter: types.BoolValue(true), CreateTable: f, Describe: f, Drop: types.BoolValue(true)}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	})

	t.Run("table", func(t *testing.T) {
		t.Run("nil_returns_nil", func(t *testing.T) {
			if refreshPerms[TablePermissions](nil, nil) != nil {
				t.Error("expected nil")
			}
		})
		t.Run("select_and_describe_present", func(t *testing.T) {
			got := refreshPerms(&TablePermissions{Select: types.BoolValue(true), Describe: types.BoolValue(true)},
				[]lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionDescribe})
			want := &TablePermissions{Alter: f, Delete: f, Describe: types.BoolValue(true), Drop: f, Insert: f, Select: types.BoolValue(true)}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("perm_revoked_externally", func(t *testing.T) {
			got := refreshPerms(&TablePermissions{Select: types.BoolValue(true), Insert: types.BoolValue(true)},
				[]lftypes.Permission{lftypes.PermissionSelect})
			want := &TablePermissions{Alter: f, Delete: f, Describe: f, Drop: f, Insert: f, Select: types.BoolValue(true)}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("ALL_expands_to_declared", func(t *testing.T) {
			got := refreshPerms(&TablePermissions{Select: types.BoolValue(true), Delete: types.BoolValue(true)},
				[]lftypes.Permission{lftypes.PermissionAll})
			want := &TablePermissions{Alter: f, Delete: types.BoolValue(true), Describe: f, Drop: f, Insert: f, Select: types.BoolValue(true)}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("undeclared_perm_in_current_not_tracked", func(t *testing.T) {
			got := refreshPerms(&TablePermissions{Select: types.BoolValue(true)},
				[]lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionInsert})
			want := &TablePermissions{Alter: f, Delete: f, Describe: f, Drop: f, Insert: f, Select: types.BoolValue(true)}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	})
}

// ── expandAllPerms ────────────────────────────────────────────────────────────

func TestExpandAllPerms(t *testing.T) {
	t.Run("catalog", func(t *testing.T) {
		t.Run("nil_passthrough", func(t *testing.T) {
			if expandAllPerms[CatalogPermissions](nil) != nil {
				t.Error("expected nil")
			}
		})
		t.Run("all_false_passthrough", func(t *testing.T) {
			p := &CatalogPermissions{Describe: types.BoolValue(true)}
			if expandAllPerms(p) != p {
				t.Error("expected same pointer when all=false")
			}
		})
		t.Run("all_true_expands_flags", func(t *testing.T) {
			got := expandAllPerms(&CatalogPermissions{All: types.BoolValue(true)})
			if got == nil {
				t.Fatal("expected non-nil")
			}
			if !got.Alter.ValueBool() || !got.CreateCatalog.ValueBool() ||
				!got.CreateDatabase.ValueBool() || !got.Describe.ValueBool() || !got.Drop.ValueBool() {
				t.Errorf("expected all individual flags true, got %+v", got)
			}
			if got.All.ValueBool() {
				t.Error("All should be null/false after expansion")
			}
		})
	})

	t.Run("database", func(t *testing.T) {
		t.Run("nil_passthrough", func(t *testing.T) {
			if expandAllPerms[DatabasePermissions](nil) != nil {
				t.Error("expected nil")
			}
		})
		t.Run("all_false_passthrough", func(t *testing.T) {
			p := &DatabasePermissions{Describe: types.BoolValue(true)}
			if expandAllPerms(p) != p {
				t.Error("expected same pointer when all=false")
			}
		})
		t.Run("all_true_expands_flags", func(t *testing.T) {
			got := expandAllPerms(&DatabasePermissions{All: types.BoolValue(true)})
			if got == nil {
				t.Fatal("expected non-nil")
			}
			if !got.Alter.ValueBool() || !got.CreateTable.ValueBool() ||
				!got.Describe.ValueBool() || !got.Drop.ValueBool() {
				t.Errorf("expected all individual flags true, got %+v", got)
			}
			if got.All.ValueBool() {
				t.Error("All should be null/false after expansion")
			}
		})
	})

	t.Run("table", func(t *testing.T) {
		t.Run("nil_passthrough", func(t *testing.T) {
			if expandAllPerms[TablePermissions](nil) != nil {
				t.Error("expected nil")
			}
		})
		t.Run("all_false_passthrough", func(t *testing.T) {
			p := &TablePermissions{Select: types.BoolValue(true)}
			if expandAllPerms(p) != p {
				t.Error("expected same pointer when all=false")
			}
		})
		t.Run("all_true_expands_flags", func(t *testing.T) {
			got := expandAllPerms(&TablePermissions{All: types.BoolValue(true)})
			if got == nil {
				t.Fatal("expected non-nil")
			}
			if !got.Alter.ValueBool() || !got.Delete.ValueBool() || !got.Describe.ValueBool() ||
				!got.Drop.ValueBool() || !got.Insert.ValueBool() || !got.Select.ValueBool() {
				t.Errorf("expected all individual flags true, got %+v", got)
			}
			if got.All.ValueBool() {
				t.Error("All should be null/false after expansion")
			}
		})
	})
}

// ── grantAll ─────────────────────────────────────────────────────────────────

func TestGrantAll(t *testing.T) {
	const principal = "arn:aws:iam::123456789012:role/TestRole"
	const catalogID = "123456789012"
	ctx := context.Background()

	t.Run("catalog_permissions", func(t *testing.T) {
		mock := &mockLFClient{}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Permissions: &CatalogPermissions{
					CreateDatabase: types.BoolValue(true),
				},
			},
		}
		if err := grantAll(ctx, mock, data); err != nil {
			t.Fatalf("grantAll() error: %v", err)
		}
		call := findGrantCall(mock.grantCalls, isCatalogResource)
		if call == nil {
			t.Fatal("expected GrantPermissions call for catalog resource")
		}
		if aws.ToString(call.Principal.DataLakePrincipalIdentifier) != principal {
			t.Errorf("principal = %q, want %q", aws.ToString(call.Principal.DataLakePrincipalIdentifier), principal)
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionCreateDatabase}) {
			t.Errorf("permissions = %v, want [CREATE_DATABASE]", call.Permissions)
		}
		if len(call.PermissionsWithGrantOption) != 0 {
			t.Errorf("grantable permissions should be empty, got %v", call.PermissionsWithGrantOption)
		}
	})

	t.Run("catalog_grantable_permissions", func(t *testing.T) {
		mock := &mockLFClient{}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				GrantablePermissions: &CatalogPermissions{
					Describe: types.BoolValue(true),
				},
			},
		}
		if err := grantAll(ctx, mock, data); err != nil {
			t.Fatalf("grantAll() error: %v", err)
		}
		call := findGrantCall(mock.grantCalls, isCatalogResource)
		if call == nil {
			t.Fatal("expected GrantPermissions call for catalog resource")
		}
		if len(call.Permissions) != 0 {
			t.Errorf("permissions should be empty, got %v", call.Permissions)
		}
		if !permsEqual(call.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("grantable = %v, want [DESCRIBE]", call.PermissionsWithGrantOption)
		}
	})

	t.Run("catalog_permissions_nil_no_call", func(t *testing.T) {
		mock := &mockLFClient{}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				// Permissions and GrantablePermissions both nil
			},
		}
		if err := grantAll(ctx, mock, data); err != nil {
			t.Fatalf("grantAll() error: %v", err)
		}
		if call := findGrantCall(mock.grantCalls, isCatalogResource); call != nil {
			t.Error("expected no GrantPermissions call when catalog permissions are nil")
		}
	})

	t.Run("database_permissions", func(t *testing.T) {
		mock := &mockLFClient{}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{
						Name: types.StringValue("analytics"),
						Permissions: &DatabasePermissions{
							Describe:    types.BoolValue(true),
							CreateTable: types.BoolValue(true),
						},
					},
				},
			},
		}
		if err := grantAll(ctx, mock, data); err != nil {
			t.Fatalf("grantAll() error: %v", err)
		}
		call := findGrantCall(mock.grantCalls, isDatabaseResource("analytics"))
		if call == nil {
			t.Fatal("expected GrantPermissions call for database resource")
		}
		if aws.ToString(call.Resource.Database.CatalogId) != catalogID {
			t.Errorf("catalogId = %q, want %q", aws.ToString(call.Resource.Database.CatalogId), catalogID)
		}
		if aws.ToString(call.Resource.Database.Name) != "analytics" {
			t.Errorf("database name = %q, want %q", aws.ToString(call.Resource.Database.Name), "analytics")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionDescribe, lftypes.PermissionCreateTable}) {
			t.Errorf("permissions = %v, want [DESCRIBE, CREATE_TABLE]", call.Permissions)
		}
	})

	t.Run("named_table_permissions", func(t *testing.T) {
		mock := &mockLFClient{}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{
						Name: types.StringValue("analytics"),
						Tables: []TablePermModel{
							{
								Name: types.StringValue("events"),
								Permissions: &TablePermissions{
									Select:   types.BoolValue(true),
									Describe: types.BoolValue(true),
								},
								GrantablePermissions: &TablePermissions{
									Select: types.BoolValue(true),
								},
							},
						},
					},
				},
			},
		}
		if err := grantAll(ctx, mock, data); err != nil {
			t.Fatalf("grantAll() error: %v", err)
		}
		call := findGrantCall(mock.grantCalls, isTableResource("analytics", "events"))
		if call == nil {
			t.Fatal("expected GrantPermissions call for named table resource")
		}
		if aws.ToString(call.Resource.Table.CatalogId) != catalogID {
			t.Errorf("catalogId = %q, want %q", aws.ToString(call.Resource.Table.CatalogId), catalogID)
		}
		if aws.ToString(call.Resource.Table.DatabaseName) != "analytics" {
			t.Errorf("databaseName = %q, want analytics", aws.ToString(call.Resource.Table.DatabaseName))
		}
		if aws.ToString(call.Resource.Table.Name) != "events" {
			t.Errorf("tableName = %q, want events", aws.ToString(call.Resource.Table.Name))
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionDescribe}) {
			t.Errorf("permissions = %v, want [SELECT, DESCRIBE]", call.Permissions)
		}
		if !permsEqual(call.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("grantable = %v, want [SELECT]", call.PermissionsWithGrantOption)
		}
	})

	t.Run("wildcard_permissions", func(t *testing.T) {
		mock := &mockLFClient{}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{
						Name: types.StringValue("analytics"),
						Wildcard: &TablePermModel{
							Permissions: &TablePermissions{
								Select: types.BoolValue(true),
							},
						},
					},
				},
			},
		}
		if err := grantAll(ctx, mock, data); err != nil {
			t.Fatalf("grantAll() error: %v", err)
		}
		call := findGrantCall(mock.grantCalls, isWildcardResource("analytics"))
		if call == nil {
			t.Fatal("expected GrantPermissions call for wildcard resource")
		}
		if call.Resource.Table.TableWildcard == nil {
			t.Error("TableWildcard must be set for wildcard resource")
		}
		if aws.ToString(call.Resource.Table.CatalogId) != catalogID {
			t.Errorf("catalogId = %q, want %q", aws.ToString(call.Resource.Table.CatalogId), catalogID)
		}
		if aws.ToString(call.Resource.Table.DatabaseName) != "analytics" {
			t.Errorf("databaseName = %q, want analytics", aws.ToString(call.Resource.Table.DatabaseName))
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("permissions = %v, want [SELECT]", call.Permissions)
		}
	})

	t.Run("all_permissions_collapse_to_ALL_in_api_call", func(t *testing.T) {
		mock := &mockLFClient{}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{
						Name: types.StringValue("db"),
						Tables: []TablePermModel{
							{
								Name: types.StringValue("tbl"),
								Permissions: &TablePermissions{
									Alter:    types.BoolValue(true),
									Delete:   types.BoolValue(true),
									Describe: types.BoolValue(true),
									Drop:     types.BoolValue(true),
									Insert:   types.BoolValue(true),
									Select:   types.BoolValue(true),
								},
							},
						},
					},
				},
			},
		}
		if err := grantAll(ctx, mock, data); err != nil {
			t.Fatalf("grantAll() error: %v", err)
		}
		call := findGrantCall(mock.grantCalls, isTableResource("db", "tbl"))
		if call == nil {
			t.Fatal("expected GrantPermissions call for table resource")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("all individual table perms should collapse to ALL, got %v", call.Permissions)
		}
	})
}

// ── revokeAll ─────────────────────────────────────────────────────────────────

// TestDelete exercises the delete path: revokeForUpdate(state, state) only revokes
// resources where permissions or grantable_permissions are explicitly declared.
func TestDelete(t *testing.T) {
	const principal = "arn:aws:iam::123456789012:role/TestRole"
	const catalogID = "123456789012"
	ctx := context.Background()

	// Helper: simulate delete by calling revokeForUpdate with state as both arguments.
	del := func(mock *mockLFClient, state *LakeFormationPermissionsResourceModel) error {
		return revokeForUpdate(ctx, mock, state, state)
	}

	t.Run("catalog_permissions_revoked", func(t *testing.T) {
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:          types.StringValue(catalogID),
				Permissions: &CatalogPermissions{Describe: types.BoolValue(true), Drop: types.BoolValue(true)},
			},
		}
		if err := del(mock, state); err != nil {
			t.Fatalf("delete error: %v", err)
		}
		call := findRevokeCall(mock.revokeCalls, isCatalogResource)
		if call == nil {
			t.Fatal("expected RevokePermissions call for catalog resource")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionDescribe, lftypes.PermissionDrop}) {
			t.Errorf("permissions = %v, want [DESCRIBE, DROP]", call.Permissions)
		}
	})

	t.Run("nil_catalog_permissions_no_revoke", func(t *testing.T) {
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				// Permissions and GrantablePermissions nil
			},
		}
		if err := del(mock, state); err != nil {
			t.Fatalf("delete error: %v", err)
		}
		if call := findRevokeCall(mock.revokeCalls, isCatalogResource); call != nil {
			t.Errorf("expected no revoke call when catalog permissions are nil; got %+v", call)
		}
	})

	t.Run("database_permissions_revoked", func(t *testing.T) {
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{Name: types.StringValue("analytics"), Permissions: &DatabasePermissions{Describe: types.BoolValue(true)}},
				},
			},
		}
		if err := del(mock, state); err != nil {
			t.Fatalf("delete error: %v", err)
		}
		call := findRevokeCall(mock.revokeCalls, isDatabaseResource("analytics"))
		if call == nil {
			t.Fatal("expected RevokePermissions call for database resource")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("permissions = %v, want [DESCRIBE]", call.Permissions)
		}
	})

	t.Run("nil_database_permissions_no_revoke", func(t *testing.T) {
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{Name: types.StringValue("analytics")}, // Permissions nil
				},
			},
		}
		if err := del(mock, state); err != nil {
			t.Fatalf("delete error: %v", err)
		}
		if call := findRevokeCall(mock.revokeCalls, isDatabaseResource("analytics")); call != nil {
			t.Errorf("expected no revoke call for database with nil permissions; got %+v", call)
		}
	})

	t.Run("named_table_permissions_revoked", func(t *testing.T) {
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{
						Name: types.StringValue("analytics"),
						Tables: []TablePermModel{
							{
								Name:        types.StringValue("events"),
								Permissions: &TablePermissions{Select: types.BoolValue(true), Insert: types.BoolValue(true)},
							},
						},
					},
				},
			},
		}
		if err := del(mock, state); err != nil {
			t.Fatalf("delete error: %v", err)
		}
		call := findRevokeCall(mock.revokeCalls, isTableResource("analytics", "events"))
		if call == nil {
			t.Fatal("expected RevokePermissions call for named table")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionInsert}) {
			t.Errorf("permissions = %v, want [SELECT, INSERT]", call.Permissions)
		}
	})

	t.Run("wildcard_permissions_revoked", func(t *testing.T) {
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{
						Name:     types.StringValue("raw"),
						Wildcard: &TablePermModel{Permissions: &TablePermissions{Select: types.BoolValue(true)}},
					},
				},
			},
		}
		if err := del(mock, state); err != nil {
			t.Fatalf("delete error: %v", err)
		}
		call := findRevokeCall(mock.revokeCalls, isWildcardResource("raw"))
		if call == nil {
			t.Fatal("expected RevokePermissions call for wildcard resource")
		}
		if call.Resource.Table.TableWildcard == nil {
			t.Error("TableWildcard must be set")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("permissions = %v, want [SELECT]", call.Permissions)
		}
	})

	t.Run("grantable_only_revokes_grant_option_field", func(t *testing.T) {
		// Table has only GrantablePermissions declared; only PermissionsWithGrantOption should be revoked.
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{
						Name: types.StringValue("analytics"),
						Tables: []TablePermModel{
							{
								Name:                 types.StringValue("events"),
								GrantablePermissions: &TablePermissions{Select: types.BoolValue(true)},
							},
						},
					},
				},
			},
		}
		if err := del(mock, state); err != nil {
			t.Fatalf("delete error: %v", err)
		}
		call := findRevokeCall(mock.revokeCalls, isTableResource("analytics", "events"))
		if call == nil {
			t.Fatal("expected RevokePermissions call for named table")
		}
		if len(call.Permissions) != 0 {
			t.Errorf("Permissions should be empty, got %v", call.Permissions)
		}
		if !permsEqual(call.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("PermissionsWithGrantOption = %v, want [SELECT]", call.PermissionsWithGrantOption)
		}
	})
}

// ── revokeForUpdate ───────────────────────────────────────────────────────────

func TestRevokeForUpdate(t *testing.T) {
	const principal = "arn:aws:iam::123456789012:role/TestRole"
	const catalogID = "123456789012"
	ctx := context.Background()

	t.Run("omit_catalog_permissions_no_revoke", func(t *testing.T) {
		// State has catalog permissions; plan omits them (nil). They must not be revoked.
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:          types.StringValue(catalogID),
				Permissions: &CatalogPermissions{CreateDatabase: types.BoolValue(true)},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				// Permissions nil — omitted
			},
		}
		if err := revokeForUpdate(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// revokeLFPerms is a no-op when both lists are nil/empty, so no API call is made.
		if call := findRevokeCall(mock.revokeCalls, isCatalogResource); call != nil {
			t.Errorf("expected no revoke call for catalog; got %+v", call)
		}
	})

	t.Run("explicit_empty_catalog_permissions_revokes_state", func(t *testing.T) {
		// Plan has an explicit (non-nil but empty) permissions block → revoke what is in state.
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:          types.StringValue(catalogID),
				Permissions: &CatalogPermissions{Describe: types.BoolValue(true)},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:          types.StringValue(catalogID),
				Permissions: &CatalogPermissions{}, // explicit empty
			},
		}
		if err := revokeForUpdate(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		call := findRevokeCall(mock.revokeCalls, isCatalogResource)
		if call == nil {
			t.Fatal("expected RevokePermissions call for catalog")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("revoked = %v, want [DESCRIBE]", call.Permissions)
		}
	})

	t.Run("omit_grantable_but_set_permissions", func(t *testing.T) {
		// Plan sets permissions but omits grantable_permissions. Only permissions should be revoked.
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:                   types.StringValue(catalogID),
				Permissions:          &CatalogPermissions{Describe: types.BoolValue(true)},
				GrantablePermissions: &CatalogPermissions{Describe: types.BoolValue(true)},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:          types.StringValue(catalogID),
				Permissions: &CatalogPermissions{Describe: types.BoolValue(true)},
				// GrantablePermissions nil — omitted
			},
		}
		if err := revokeForUpdate(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		call := findRevokeCall(mock.revokeCalls, isCatalogResource)
		if call == nil {
			t.Fatal("expected RevokePermissions call for catalog")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("permissions = %v, want [DESCRIBE]", call.Permissions)
		}
		if len(call.PermissionsWithGrantOption) != 0 {
			t.Errorf("grantable permissions should not be revoked, got %v", call.PermissionsWithGrantOption)
		}
	})

	t.Run("omit_database_permissions_no_revoke", func(t *testing.T) {
		// State has database permissions; plan has the same database but omits permissions.
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{
						Name:        types.StringValue("analytics"),
						Permissions: &DatabasePermissions{Describe: types.BoolValue(true)},
					},
				},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{
						Name: types.StringValue("analytics"),
						// Permissions nil
					},
				},
			},
		}
		if err := revokeForUpdate(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if call := findRevokeCall(mock.revokeCalls, isDatabaseResource("analytics")); call != nil {
			t.Errorf("expected no revoke call for database; got %+v", call)
		}
	})

	t.Run("explicit_empty_database_permissions_revokes_state", func(t *testing.T) {
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{
						Name:        types.StringValue("analytics"),
						Permissions: &DatabasePermissions{CreateTable: types.BoolValue(true)},
					},
				},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{
						Name:        types.StringValue("analytics"),
						Permissions: &DatabasePermissions{}, // explicit empty
					},
				},
			},
		}
		if err := revokeForUpdate(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		call := findRevokeCall(mock.revokeCalls, isDatabaseResource("analytics"))
		if call == nil {
			t.Fatal("expected RevokePermissions call for database")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionCreateTable}) {
			t.Errorf("revoked = %v, want [CREATE_TABLE]", call.Permissions)
		}
	})

	t.Run("database_absent_from_plan_fully_revoked", func(t *testing.T) {
		// State has "analytics" database; plan has no databases. The database must be revoked.
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{
						Name:        types.StringValue("analytics"),
						Permissions: &DatabasePermissions{Describe: types.BoolValue(true)},
					},
				},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				// No databases
			},
		}
		if err := revokeForUpdate(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		call := findRevokeCall(mock.revokeCalls, isDatabaseResource("analytics"))
		if call == nil {
			t.Fatal("expected RevokePermissions call for removed database")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("revoked = %v, want [DESCRIBE]", call.Permissions)
		}
	})

	t.Run("table_absent_from_plan_fully_revoked", func(t *testing.T) {
		// State has table "events"; plan has the database but no tables.
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{
						Name: types.StringValue("analytics"),
						Tables: []TablePermModel{
							{
								Name:        types.StringValue("events"),
								Permissions: &TablePermissions{Select: types.BoolValue(true)},
							},
						},
					},
				},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{Name: types.StringValue("analytics")},
				},
			},
		}
		if err := revokeForUpdate(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		call := findRevokeCall(mock.revokeCalls, isTableResource("analytics", "events"))
		if call == nil {
			t.Fatal("expected RevokePermissions call for removed table")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("revoked = %v, want [SELECT]", call.Permissions)
		}
	})

	t.Run("omit_table_permissions_no_revoke", func(t *testing.T) {
		// Table is still in plan but permissions block is nil — leave it alone.
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{
						Name: types.StringValue("analytics"),
						Tables: []TablePermModel{
							{
								Name:        types.StringValue("events"),
								Permissions: &TablePermissions{Select: types.BoolValue(true)},
							},
						},
					},
				},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{
						Name: types.StringValue("analytics"),
						Tables: []TablePermModel{
							{Name: types.StringValue("events")}, // permissions nil
						},
					},
				},
			},
		}
		if err := revokeForUpdate(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if call := findRevokeCall(mock.revokeCalls, isTableResource("analytics", "events")); call != nil {
			t.Errorf("expected no revoke call for table with nil permissions; got %+v", call)
		}
	})

	t.Run("wildcard_absent_from_plan_revoked", func(t *testing.T) {
		// State has wildcard; plan has the same database but no wildcard.
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{
						Name: types.StringValue("raw"),
						Wildcard: &TablePermModel{
							Permissions: &TablePermissions{Select: types.BoolValue(true)},
						},
					},
				},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{Name: types.StringValue("raw")}, // wildcard nil
				},
			},
		}
		if err := revokeForUpdate(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		call := findRevokeCall(mock.revokeCalls, isWildcardResource("raw"))
		if call == nil {
			t.Fatal("expected RevokePermissions call for removed wildcard")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("revoked = %v, want [SELECT]", call.Permissions)
		}
	})

	t.Run("omit_wildcard_permissions_no_revoke", func(t *testing.T) {
		// Wildcard is in plan but its permissions block is nil — leave it alone.
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{
						Name: types.StringValue("raw"),
						Wildcard: &TablePermModel{
							Permissions: &TablePermissions{Select: types.BoolValue(true)},
						},
					},
				},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{
						Name:     types.StringValue("raw"),
						Wildcard: &TablePermModel{}, // present but permissions nil
					},
				},
			},
		}
		if err := revokeForUpdate(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if call := findRevokeCall(mock.revokeCalls, isWildcardResource("raw")); call != nil {
			t.Errorf("expected no revoke call for wildcard with nil permissions; got %+v", call)
		}
	})

	t.Run("multiple_databases_independent_handling", func(t *testing.T) {
		// "analytics" is in plan with explicit permissions (revoke + re-grant),
		// "staging" is in plan with nil permissions (leave alone),
		// "old" is absent from plan (fully revoke).
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{Name: types.StringValue("analytics"), Permissions: &DatabasePermissions{Describe: types.BoolValue(true)}},
					{Name: types.StringValue("staging"), Permissions: &DatabasePermissions{Alter: types.BoolValue(true)}},
					{Name: types.StringValue("old"), Permissions: &DatabasePermissions{Drop: types.BoolValue(true)}},
				},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Databases: []DatabasePermModel{
					{Name: types.StringValue("analytics"), Permissions: &DatabasePermissions{CreateTable: types.BoolValue(true)}},
					{Name: types.StringValue("staging")}, // permissions nil
					// "old" absent
				},
			},
		}
		if err := revokeForUpdate(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// analytics: permissions block present → revoke state permissions
		if call := findRevokeCall(mock.revokeCalls, isDatabaseResource("analytics")); call == nil {
			t.Error("expected revoke call for analytics")
		} else if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("analytics revoked = %v, want [DESCRIBE]", call.Permissions)
		}

		// staging: permissions nil → no revoke
		if call := findRevokeCall(mock.revokeCalls, isDatabaseResource("staging")); call != nil {
			t.Errorf("expected no revoke call for staging; got %+v", call)
		}

		// old: absent from plan → revoke
		if call := findRevokeCall(mock.revokeCalls, isDatabaseResource("old")); call == nil {
			t.Error("expected revoke call for old (absent from plan)")
		} else if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionDrop}) {
			t.Errorf("old revoked = %v, want [DROP]", call.Permissions)
		}
	})
}
