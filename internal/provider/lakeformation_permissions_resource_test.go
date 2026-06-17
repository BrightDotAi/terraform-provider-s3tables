// Copyright BrightAI 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	lakeformation "github.com/aws/aws-sdk-go-v2/service/lakeformation"
	lftypes "github.com/aws/aws-sdk-go-v2/service/lakeformation/types"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
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

// mockLFClientWithPerms returns a mock that reports at least one active permission on
// any ListPermissions call, so that revokeIfPermitted proceeds to revoke rather than skip.
func mockLFClientWithPerms() *mockLFClient {
	return &mockLFClient{
		listResult: []lftypes.PrincipalResourcePermissions{
			{Permissions: []lftypes.Permission{lftypes.PermissionDescribe}},
		},
	}
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
			name: "all_individual_true_returns_each_perm",
			input: &CatalogPermissions{
				Alter:          types.BoolValue(true),
				CreateCatalog:  types.BoolValue(true),
				CreateDatabase: types.BoolValue(true),
				Describe:       types.BoolValue(true),
				Drop:           types.BoolValue(true),
			},
			want: []lftypes.Permission{
				lftypes.PermissionAlter, lftypes.PermissionCreateCatalog,
				lftypes.PermissionCreateDatabase, lftypes.PermissionDescribe,
				lftypes.PermissionDrop,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := permsToAPI(tt.input)
			if !permsEqual(got, tt.want) {
				t.Errorf("permsToAPI() = %v, want %v", got, tt.want)
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
			name: "all_individual_true_returns_each_perm",
			input: &DatabasePermissions{
				Alter:       types.BoolValue(true),
				CreateTable: types.BoolValue(true),
				Describe:    types.BoolValue(true),
				Drop:        types.BoolValue(true),
			},
			want: []lftypes.Permission{
				lftypes.PermissionAlter, lftypes.PermissionCreateTable,
				lftypes.PermissionDescribe, lftypes.PermissionDrop,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := permsToAPI(tt.input)
			if !permsEqual(got, tt.want) {
				t.Errorf("permsToAPI() = %v, want %v", got, tt.want)
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
			name: "all_individual_true_returns_each_perm",
			input: &TablePermissions{
				Alter:    types.BoolValue(true),
				Delete:   types.BoolValue(true),
				Describe: types.BoolValue(true),
				Drop:     types.BoolValue(true),
				Insert:   types.BoolValue(true),
				Select:   types.BoolValue(true),
			},
			want: []lftypes.Permission{
				lftypes.PermissionAlter, lftypes.PermissionDelete, lftypes.PermissionDescribe,
				lftypes.PermissionDrop, lftypes.PermissionInsert, lftypes.PermissionSelect,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := permsToAPI(tt.input)
			if !permsEqual(got, tt.want) {
				t.Errorf("permsToAPI() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ── API response → permission struct ─────────────────────────────────────────

// ── refreshPerms ─────────────────────────────────────────────────────────────

func TestRefreshPerms(t *testing.T) {
	// refreshBool returns null (not false) for undeclared fields so that state
	// agrees with the plan value produced by clearBoolIfUnset (cty: false ≠ null).
	// Only fields declared true but revoked externally emit explicit false.
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
			want := &CatalogPermissions{CreateDatabase: types.BoolValue(true)}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("declared_perm_absent_becomes_false", func(t *testing.T) {
			got := refreshPerms(&CatalogPermissions{CreateDatabase: types.BoolValue(true), Describe: types.BoolValue(true)},
				[]lftypes.Permission{lftypes.PermissionCreateDatabase})
			// Describe was declared true but is absent from current → explicit false.
			// Undeclared fields (Alter, CreateCatalog, Drop) → null (zero value).
			want := &CatalogPermissions{CreateDatabase: types.BoolValue(true), Describe: f}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("ALL_sets_all_declared_true", func(t *testing.T) {
			got := refreshPerms(&CatalogPermissions{Alter: types.BoolValue(true), CreateDatabase: types.BoolValue(true)},
				[]lftypes.Permission{lftypes.PermissionAll})
			want := &CatalogPermissions{Alter: types.BoolValue(true), CreateDatabase: types.BoolValue(true)}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("undeclared_perms_not_tracked", func(t *testing.T) {
			// All fields null in declared → all returned as null regardless of current.
			got := refreshPerms(&CatalogPermissions{},
				[]lftypes.Permission{lftypes.PermissionCreateDatabase, lftypes.PermissionDescribe})
			want := &CatalogPermissions{}
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
			want := &DatabasePermissions{Describe: types.BoolValue(true)}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("perm_revoked_externally", func(t *testing.T) {
			got := refreshPerms(&DatabasePermissions{CreateTable: types.BoolValue(true), Alter: types.BoolValue(true)},
				[]lftypes.Permission{lftypes.PermissionAlter})
			// CreateTable declared true but absent → explicit false. Undeclared → null.
			want := &DatabasePermissions{Alter: types.BoolValue(true), CreateTable: f}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("ALL_expands_to_declared", func(t *testing.T) {
			got := refreshPerms(&DatabasePermissions{Alter: types.BoolValue(true), Drop: types.BoolValue(true)},
				[]lftypes.Permission{lftypes.PermissionAll})
			want := &DatabasePermissions{Alter: types.BoolValue(true), Drop: types.BoolValue(true)}
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
			want := &TablePermissions{Describe: types.BoolValue(true), Select: types.BoolValue(true)}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("perm_revoked_externally", func(t *testing.T) {
			got := refreshPerms(&TablePermissions{Select: types.BoolValue(true), Insert: types.BoolValue(true)},
				[]lftypes.Permission{lftypes.PermissionSelect})
			// Insert declared true but absent → explicit false. Undeclared → null.
			want := &TablePermissions{Insert: f, Select: types.BoolValue(true)}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("ALL_expands_to_declared", func(t *testing.T) {
			got := refreshPerms(&TablePermissions{Select: types.BoolValue(true), Delete: types.BoolValue(true)},
				[]lftypes.Permission{lftypes.PermissionAll})
			want := &TablePermissions{Delete: types.BoolValue(true), Select: types.BoolValue(true)}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("undeclared_perm_in_current_not_tracked", func(t *testing.T) {
			got := refreshPerms(&TablePermissions{Select: types.BoolValue(true)},
				[]lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionInsert})
			want := &TablePermissions{Select: types.BoolValue(true)}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	})
}

// ── refreshBool ───────────────────────────────────────────────────────────────

func TestRefreshBool(t *testing.T) {
	granted := map[lftypes.Permission]bool{lftypes.PermissionSelect: true}

	tests := []struct {
		name     string
		declared types.Bool
		perm     lftypes.Permission
		current  map[lftypes.Permission]bool
		hasAll   bool
		want     types.Bool
	}{
		// Undeclared (null) → always null, even when the permission is present in AWS.
		// This is the key invariant: undeclared fields must never diverge from the plan
		// value produced by clearBoolIfUnset, which also yields null for unconfigured bools.
		{
			name: "undeclared_perm_absent_returns_null",
			declared: types.BoolNull(), perm: lftypes.PermissionInsert,
			current: granted, hasAll: false, want: types.BoolNull(),
		},
		{
			name: "undeclared_perm_present_in_aws_returns_null",
			declared: types.BoolNull(), perm: lftypes.PermissionSelect,
			current: granted, hasAll: false, want: types.BoolNull(),
		},
		{
			name: "undeclared_with_all_in_aws_returns_null",
			declared: types.BoolNull(), perm: lftypes.PermissionSelect,
			current: granted, hasAll: true, want: types.BoolNull(),
		},
		// Declared false is semantically the same as null: not managed → null in state.
		{
			name: "declared_false_perm_absent_returns_null",
			declared: types.BoolValue(false), perm: lftypes.PermissionInsert,
			current: granted, hasAll: false, want: types.BoolNull(),
		},
		{
			name: "declared_false_perm_present_returns_null",
			declared: types.BoolValue(false), perm: lftypes.PermissionSelect,
			current: granted, hasAll: false, want: types.BoolNull(),
		},
		// Declared true → reflect actual AWS state.
		{
			name: "declared_true_perm_present_returns_true",
			declared: types.BoolValue(true), perm: lftypes.PermissionSelect,
			current: granted, hasAll: false, want: types.BoolValue(true),
		},
		{
			name: "declared_true_perm_absent_returns_false",
			declared: types.BoolValue(true), perm: lftypes.PermissionInsert,
			current: granted, hasAll: false, want: types.BoolValue(false),
		},
		{
			name: "declared_true_all_in_aws_returns_true",
			declared: types.BoolValue(true), perm: lftypes.PermissionInsert,
			current: granted, hasAll: true, want: types.BoolValue(true),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := refreshBool(tt.declared, tt.perm, tt.current, tt.hasAll)
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// ── no-drift guarantees ───────────────────────────────────────────────────────

// TestNoDriftForOmittedPermissions verifies the core invariant: Terraform should
// not plan an Update when external actors change permissions that are not declared
// in the resource config (either because the whole block is absent, or because
// individual flags within a declared block are not configured).
//
// The mechanism: Read calls refreshPerms, which calls refreshBool. Both must
// produce values that agree with what the config puts into the plan — otherwise
// cty sees false ≠ null and triggers a phantom diff on every plan.
func TestNoDriftForOmittedPermissions(t *testing.T) {
	anyPerms := []lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionDescribe}

	// Nil permissions block (block omitted in config) → refreshPerms returns nil.
	// plan: nil (Optional attr not set) == state: nil → no diff → no Update.
	t.Run("nil_catalog_permissions_block_returns_nil", func(t *testing.T) {
		if got := refreshPerms[CatalogPermissions](nil, anyPerms); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})
	t.Run("nil_database_permissions_block_returns_nil", func(t *testing.T) {
		if got := refreshPerms[DatabasePermissions](nil, anyPerms); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})
	t.Run("nil_table_permissions_block_returns_nil", func(t *testing.T) {
		if got := refreshPerms[TablePermissions](nil, anyPerms); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})
	t.Run("nil_grantable_permissions_block_returns_nil", func(t *testing.T) {
		// Same nil semantics apply to grantable_permissions.
		if got := refreshPerms[CatalogPermissions](nil, anyPerms); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	// Undeclared flag within a present permissions block → null in state.
	// plan: null (Optional attr not configured) == state: null → no diff for that flag.
	t.Run("undeclared_flag_is_null_in_state_even_when_granted_in_aws", func(t *testing.T) {
		// permissions { select = true } — ALTER not declared.
		// AWS externally grants ALTER. State must not track it.
		got := refreshPerms(
			&TablePermissions{Select: types.BoolValue(true)},
			[]lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionAlter},
		)
		if !got.Alter.IsNull() {
			t.Errorf("undeclared Alter: want null in state, got %v", got.Alter)
		}
		if got.Select != types.BoolValue(true) {
			t.Errorf("declared Select: want true, got %v", got.Select)
		}
	})
	t.Run("undeclared_database_flag_is_null_in_state", func(t *testing.T) {
		// permissions { describe = true } — CREATE_TABLE not declared.
		got := refreshPerms(
			&DatabasePermissions{Describe: types.BoolValue(true)},
			[]lftypes.Permission{lftypes.PermissionDescribe, lftypes.PermissionCreateTable},
		)
		if !got.CreateTable.IsNull() {
			t.Errorf("undeclared CreateTable: want null, got %v", got.CreateTable)
		}
	})

	// Declared permission still granted → true in state == true in plan → no diff.
	t.Run("declared_perm_present_no_drift", func(t *testing.T) {
		got := refreshPerms(
			&TablePermissions{Select: types.BoolValue(true)},
			[]lftypes.Permission{lftypes.PermissionSelect},
		)
		if got.Select != types.BoolValue(true) {
			t.Errorf("declared Select still granted: want true, got %v", got.Select)
		}
	})

	// Declared permission externally revoked → false in state ≠ true in plan → Update.
	t.Run("declared_perm_revoked_triggers_drift", func(t *testing.T) {
		got := refreshPerms(
			&TablePermissions{Select: types.BoolValue(true)},
			[]lftypes.Permission{}, // SELECT absent
		)
		if got.Select != types.BoolValue(false) {
			t.Errorf("declared Select externally revoked: want false (drift signal), got %v", got.Select)
		}
	})
	t.Run("declared_catalog_perm_revoked_triggers_drift", func(t *testing.T) {
		got := refreshPerms(
			&CatalogPermissions{CreateDatabase: types.BoolValue(true)},
			[]lftypes.Permission{}, // CREATE_DATABASE absent
		)
		if got.CreateDatabase != types.BoolValue(false) {
			t.Errorf("declared CreateDatabase revoked: want false, got %v", got.CreateDatabase)
		}
	})

	// ALL in AWS covers all declared permissions — no drift even if declared via individual flags.
	t.Run("all_in_aws_satisfies_individual_declared_flags", func(t *testing.T) {
		got := refreshPerms(
			&TablePermissions{Select: types.BoolValue(true), Insert: types.BoolValue(true)},
			[]lftypes.Permission{lftypes.PermissionAll},
		)
		if got.Select != types.BoolValue(true) {
			t.Errorf("Select covered by ALL: want true, got %v", got.Select)
		}
		if got.Insert != types.BoolValue(true) {
			t.Errorf("Insert covered by ALL: want true, got %v", got.Insert)
		}
		if !got.Alter.IsNull() {
			t.Errorf("undeclared Alter: want null even when ALL granted, got %v", got.Alter)
		}
	})
}

// ── all=true shorthand ────────────────────────────────────────────────────────

// TestAllShorthand verifies that all=true in a permissions block:
//  1. Sends [ALL] to the AWS API via the *PermsToAPI functions.
//  2. Is persisted to state as-is (no plan-time expansion).
//  3. Is refreshed correctly: stays true when ALL is still granted, becomes
//     false when ALL is revoked externally (triggering an Update to re-grant).
func TestAllShorthand(t *testing.T) {
	t.Run("catalog_all_true_sends_ALL_to_api", func(t *testing.T) {
		got := permsToAPI(&CatalogPermissions{All: types.BoolValue(true)})
		if !permsEqual(got, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("catalogPermsToAPI all=true = %v, want [ALL]", got)
		}
	})
	t.Run("database_all_true_sends_ALL_to_api", func(t *testing.T) {
		got := permsToAPI(&DatabasePermissions{All: types.BoolValue(true)})
		if !permsEqual(got, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("databasePermsToAPI all=true = %v, want [ALL]", got)
		}
	})
	t.Run("table_all_true_sends_ALL_to_api", func(t *testing.T) {
		got := permsToAPI(&TablePermissions{All: types.BoolValue(true)})
		if !permsEqual(got, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("tablePermsToAPI all=true = %v, want [ALL]", got)
		}
	})

	// all=true persists in state and is refreshed via the same tfsdk-tag→permission
	// mechanism as individual flags ("all" uppercases to "ALL").
	t.Run("all_true_refreshed_stays_true_when_ALL_active", func(t *testing.T) {
		got := refreshPerms(
			&CatalogPermissions{All: types.BoolValue(true)},
			[]lftypes.Permission{lftypes.PermissionAll},
		)
		if got.All != types.BoolValue(true) {
			t.Errorf("all=true with ALL in AWS: want true, got %v", got.All)
		}
	})
	t.Run("all_true_refreshed_becomes_false_when_ALL_revoked", func(t *testing.T) {
		// ALL was externally revoked. State flips to false → plan still has all=true →
		// Terraform detects drift and triggers an Update to re-grant ALL.
		got := refreshPerms(
			&CatalogPermissions{All: types.BoolValue(true)},
			[]lftypes.Permission{},
		)
		if got.All != types.BoolValue(false) {
			t.Errorf("all=true with ALL revoked: want false (drift signal), got %v", got.All)
		}
	})
	t.Run("all_true_individual_flags_remain_null_in_state", func(t *testing.T) {
		// When all=true, individual flags are not declared by the user and must
		// stay null in state so they don't produce phantom plan diffs.
		got := refreshPerms(
			&TablePermissions{All: types.BoolValue(true)},
			[]lftypes.Permission{lftypes.PermissionAll},
		)
		if !got.Select.IsNull() || !got.Alter.IsNull() || !got.Insert.IsNull() {
			t.Errorf("undeclared individual flags must be null when all=true is used; got %+v", got)
		}
	})

	// grantAll must send ALL when all=true is set.
	t.Run("grantAll_with_all_true_sends_ALL", func(t *testing.T) {
		const principal = "arn:aws:iam::123456789012:role/TestRole"
		mock := &mockLFClient{}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID:          types.StringValue("123456789012"),
				Permissions: &CatalogPermissions{All: types.BoolValue(true)},
			},
		}
		if err := grantAll(context.Background(), mock, data); err != nil {
			t.Fatalf("grantAll error: %v", err)
		}
		call := findGrantCall(mock.grantCalls, isCatalogResource)
		if call == nil {
			t.Fatal("expected GrantPermissions call")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("permissions = %v, want [ALL]", call.Permissions)
		}
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
		// ModifyPlan sets permissions = grantable_permissions when only grantable_permissions
		// is configured; by the time grantAll runs both fields are non-nil and equal.
		mock := &mockLFClient{}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID:                   types.StringValue(catalogID),
				Permissions:          &CatalogPermissions{Describe: types.BoolValue(true)},
				GrantablePermissions: &CatalogPermissions{Describe: types.BoolValue(true)},
			},
		}
		if err := grantAll(ctx, mock, data); err != nil {
			t.Fatalf("grantAll() error: %v", err)
		}
		call := findGrantCall(mock.grantCalls, isCatalogResource)
		if call == nil {
			t.Fatal("expected GrantPermissions call for catalog resource")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("permissions = %v, want [DESCRIBE]", call.Permissions)
		}
		if !permsEqual(call.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("grantable = %v, want [DESCRIBE]", call.PermissionsWithGrantOption)
		}
	})

	t.Run("grantable_only_table_permissions_equal", func(t *testing.T) {
		// When only grantable_permissions is configured, ModifyPlan sets permissions = grantable_permissions.
		// Both fields are equal in the model that reaches grantAll.
		mock := &mockLFClient{}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{
					{
						Name: types.StringValue("analytics"),
						Table: []TablePermModel{
							{
								Name:                 types.StringValue("events"),
								Permissions:          &TablePermissions{Select: types.BoolValue(true)},
								GrantablePermissions: &TablePermissions{Select: types.BoolValue(true)},
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
			t.Fatal("expected GrantPermissions call for table resource")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("permissions = %v, want [SELECT]", call.Permissions)
		}
		if !permsEqual(call.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("grantable = %v, want [SELECT]", call.PermissionsWithGrantOption)
		}
	})

	t.Run("permissions_proper_superset_of_grantable", func(t *testing.T) {
		// permissions = {DESCRIBE, SELECT}, grantable = {DESCRIBE}
		// The API must receive Permissions=[DESCRIBE,SELECT] and PermissionsWithGrantOption=[DESCRIBE].
		mock := &mockLFClient{}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{
					{
						Name: types.StringValue("analytics"),
						Table: []TablePermModel{
							{
								Name:                 types.StringValue("events"),
								Permissions:          &TablePermissions{Describe: types.BoolValue(true), Select: types.BoolValue(true)},
								GrantablePermissions: &TablePermissions{Describe: types.BoolValue(true)},
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
			t.Fatal("expected GrantPermissions call for table resource")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionDescribe, lftypes.PermissionSelect}) {
			t.Errorf("permissions = %v, want [DESCRIBE, SELECT]", call.Permissions)
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
				Database: []DatabasePermModel{
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
				Database: []DatabasePermModel{
					{
						Name: types.StringValue("analytics"),
						Table: []TablePermModel{
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
				Database: []DatabasePermModel{
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

	t.Run("all_individual_true_sends_each_perm", func(t *testing.T) {
		// Validation prevents this config in practice; permsToAPI no longer collapses.
		mock := &mockLFClient{}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{
					{
						Name: types.StringValue("db"),
						Table: []TablePermModel{
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
		want := []lftypes.Permission{
			lftypes.PermissionAlter, lftypes.PermissionDelete, lftypes.PermissionDescribe,
			lftypes.PermissionDrop, lftypes.PermissionInsert, lftypes.PermissionSelect,
		}
		if !permsEqual(call.Permissions, want) {
			t.Errorf("permissions = %v, want %v", call.Permissions, want)
		}
	})
}

// ── revokeAll ─────────────────────────────────────────────────────────────────

// TestDelete exercises the delete path via deletePermissions.
func TestDelete(t *testing.T) {
	const principal = "arn:aws:iam::123456789012:role/TestRole"
	const catalogID = "123456789012"
	ctx := context.Background()

	del := func(mock *mockLFClient, state *LakeFormationPermissionsResourceModel) error {
		return deletePermissions(ctx, mock, state)
	}

	t.Run("catalog_permissions_revoked", func(t *testing.T) {
		mock := mockLFClientWithPerms()
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
		mock := mockLFClientWithPerms()
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{
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
				Database: []DatabasePermModel{
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

	t.Run("nil_db_permissions_but_table_permissions_revoked", func(t *testing.T) {
		// DB-level permissions nil → no DB revoke; table-level permissions set → table revoke fires.
		mock := mockLFClientWithPerms()
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{
					{
						Name: types.StringValue("analytics"), // Permissions nil at DB level
						Table: []TablePermModel{
							{
								Name:        types.StringValue("events"),
								Permissions: &TablePermissions{Select: types.BoolValue(true)},
							},
						},
					},
				},
			},
		}
		if err := del(mock, state); err != nil {
			t.Fatalf("delete error: %v", err)
		}
		if call := findRevokeCall(mock.revokeCalls, isDatabaseResource("analytics")); call != nil {
			t.Errorf("expected no revoke call for database with nil permissions; got %+v", call)
		}
		call := findRevokeCall(mock.revokeCalls, isTableResource("analytics", "events"))
		if call == nil {
			t.Fatal("expected RevokePermissions call for table despite nil db permissions")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("table permissions = %v, want [SELECT]", call.Permissions)
		}
	})

	t.Run("named_table_permissions_revoked", func(t *testing.T) {
		mock := mockLFClientWithPerms()
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{
					{
						Name: types.StringValue("analytics"),
						Table: []TablePermModel{
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
		mock := mockLFClientWithPerms()
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{
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

	t.Run("grantable_only_revokes_perm_and_grant_option", func(t *testing.T) {
		// State has permissions = grantable_permissions = {SELECT} (as ModifyPlan would have set them).
		// Both Permissions and PermissionsWithGrantOption must be revoked.
		mock := mockLFClientWithPerms()
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{
					{
						Name: types.StringValue("analytics"),
						Table: []TablePermModel{
							{
								Name:                 types.StringValue("events"),
								Permissions:          &TablePermissions{Select: types.BoolValue(true)},
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
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("Permissions = %v, want [SELECT]", call.Permissions)
		}
		if !permsEqual(call.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("PermissionsWithGrantOption = %v, want [SELECT]", call.PermissionsWithGrantOption)
		}
	})
}


// ── checkPerms ───────────────────────────────────────────────────────────────

func TestCheckPerms(t *testing.T) {
	check := func(p any) bool {
		var diags diag.Diagnostics
		checkPerms(p, path.Root("permissions"), &diags)
		return diags.HasError()
	}

	t.Run("nil_no_error", func(t *testing.T) {
		if check((*CatalogPermissions)(nil)) {
			t.Error("nil pointer: expected no error")
		}
	})

	t.Run("empty_struct_no_error", func(t *testing.T) {
		if check(&CatalogPermissions{}) {
			t.Error("empty struct: expected no error")
		}
	})

	t.Run("all_true_no_error", func(t *testing.T) {
		// Explicitly setting all = true is always valid.
		if check(&CatalogPermissions{All: types.BoolValue(true)}) {
			t.Error("all=true: expected no error")
		}
	})

	t.Run("partial_catalog_no_error", func(t *testing.T) {
		p := &CatalogPermissions{Alter: types.BoolValue(true), Describe: types.BoolValue(true)}
		if check(p) {
			t.Error("partial catalog subset: expected no error")
		}
	})

	t.Run("all_individual_catalog_error", func(t *testing.T) {
		p := &CatalogPermissions{
			Alter:          types.BoolValue(true),
			CreateCatalog:  types.BoolValue(true),
			CreateDatabase: types.BoolValue(true),
			Describe:       types.BoolValue(true),
			Drop:           types.BoolValue(true),
		}
		if !check(p) {
			t.Error("all individual catalog perms set: expected error")
		}
	})

	t.Run("partial_database_no_error", func(t *testing.T) {
		p := &DatabasePermissions{CreateTable: types.BoolValue(true), Describe: types.BoolValue(true)}
		if check(p) {
			t.Error("partial database subset: expected no error")
		}
	})

	t.Run("all_individual_database_error", func(t *testing.T) {
		p := &DatabasePermissions{
			Alter:       types.BoolValue(true),
			CreateTable: types.BoolValue(true),
			Describe:    types.BoolValue(true),
			Drop:        types.BoolValue(true),
		}
		if !check(p) {
			t.Error("all individual database perms set: expected error")
		}
	})

	t.Run("partial_table_no_error", func(t *testing.T) {
		p := &TablePermissions{Select: types.BoolValue(true), Describe: types.BoolValue(true)}
		if check(p) {
			t.Error("partial table subset: expected no error")
		}
	})

	t.Run("all_individual_table_error", func(t *testing.T) {
		p := &TablePermissions{
			Alter:    types.BoolValue(true),
			Delete:   types.BoolValue(true),
			Describe: types.BoolValue(true),
			Drop:     types.BoolValue(true),
			Insert:   types.BoolValue(true),
			Select:   types.BoolValue(true),
		}
		if !check(p) {
			t.Error("all individual table perms set: expected error")
		}
	})

	t.Run("one_below_full_table_no_error", func(t *testing.T) {
		// All but one field set — strict subset, so no error.
		p := &TablePermissions{
			Alter:    types.BoolValue(true),
			Delete:   types.BoolValue(true),
			Describe: types.BoolValue(true),
			Drop:     types.BoolValue(true),
			Insert:   types.BoolValue(true),
			// Select omitted
		}
		if check(p) {
			t.Error("five of six table perms set: expected no error")
		}
	})
}

// ── checkSupersetPerms ───────────────────────────────────────────────────────

func TestCheckSupersetPerms(t *testing.T) {
	check := func(perms, grantPerms any) bool {
		var diags diag.Diagnostics
		checkSupersetPerms(perms, grantPerms, path.Root("catalog"), &diags)
		return diags.HasError()
	}

	t.Run("nil_perms_no_error", func(t *testing.T) {
		// nil permissions will be computed — skip superset check.
		if check((*TablePermissions)(nil), &TablePermissions{Select: types.BoolValue(true)}) {
			t.Error("nil permissions: expected no error")
		}
	})

	t.Run("nil_grantable_no_error", func(t *testing.T) {
		if check(&TablePermissions{Select: types.BoolValue(true)}, (*TablePermissions)(nil)) {
			t.Error("nil grantable_permissions: expected no error")
		}
	})

	t.Run("equal_no_error", func(t *testing.T) {
		p := &TablePermissions{Select: types.BoolValue(true)}
		g := &TablePermissions{Select: types.BoolValue(true)}
		if check(p, g) {
			t.Error("equal permissions: expected no error")
		}
	})

	t.Run("superset_no_error", func(t *testing.T) {
		p := &TablePermissions{Select: types.BoolValue(true), Insert: types.BoolValue(true)}
		g := &TablePermissions{Select: types.BoolValue(true)}
		if check(p, g) {
			t.Error("proper superset: expected no error")
		}
	})

	t.Run("permissions_all_no_error", func(t *testing.T) {
		// permissions.All=true is a superset of any grantable_permissions.
		p := &TablePermissions{All: types.BoolValue(true)}
		g := &TablePermissions{Select: types.BoolValue(true), Insert: types.BoolValue(true)}
		if check(p, g) {
			t.Error("permissions.All=true: expected no error for any grantable_permissions")
		}
	})

	t.Run("missing_permission_error", func(t *testing.T) {
		p := &TablePermissions{Select: types.BoolValue(true)}
		g := &TablePermissions{Select: types.BoolValue(true), Insert: types.BoolValue(true)}
		if !check(p, g) {
			t.Error("insert in grantable but not in permissions: expected error")
		}
	})

	t.Run("grantable_all_without_perms_all_error", func(t *testing.T) {
		p := &CatalogPermissions{Describe: types.BoolValue(true)}
		g := &CatalogPermissions{All: types.BoolValue(true)}
		if !check(p, g) {
			t.Error("grantable ALL but permissions lacks ALL: expected error")
		}
	})

	t.Run("both_all_no_error", func(t *testing.T) {
		p := &CatalogPermissions{All: types.BoolValue(true)}
		g := &CatalogPermissions{All: types.BoolValue(true)}
		if check(p, g) {
			t.Error("both ALL: expected no error")
		}
	})
}

// ── defaultPermissions ───────────────────────────────────────────────

func TestDefaultPermissions(t *testing.T) {
	const principal = "arn:aws:iam::123456789012:role/TestRole"
	const catalogID = "123456789012"

	t.Run("catalog_grantable_only_sets_permissions", func(t *testing.T) {
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:                   types.StringValue(catalogID),
				GrantablePermissions: &CatalogPermissions{Describe: types.BoolValue(true)},
			},
		}
		changed := defaultPermissions(plan)
		if !changed {
			t.Fatal("expected changed=true")
		}
		if plan.Catalog.Permissions == nil {
			t.Fatal("expected Permissions to be set")
		}
		if !plan.Catalog.Permissions.Describe.ValueBool() {
			t.Error("expected Permissions.Describe=true")
		}
	})

	t.Run("catalog_both_set_unchanged", func(t *testing.T) {
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:                   types.StringValue(catalogID),
				Permissions:          &CatalogPermissions{Describe: types.BoolValue(true), Alter: types.BoolValue(true)},
				GrantablePermissions: &CatalogPermissions{Describe: types.BoolValue(true)},
			},
		}
		defaultPermissions(plan)
		// permissions should not be overwritten since it was already set.
		if !plan.Catalog.Permissions.Alter.ValueBool() {
			t.Error("Alter should still be true — permissions must not be overwritten")
		}
	})

	t.Run("table_grantable_only_sets_permissions", func(t *testing.T) {
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					Table: []TablePermModel{{
						Name:                 types.StringValue("tbl"),
						GrantablePermissions: &TablePermissions{Select: types.BoolValue(true)},
					}},
				}},
			},
		}
		defaultPermissions(plan)
		tbl := plan.Catalog.Database[0].Table[0]
		if tbl.Permissions == nil {
			t.Fatal("expected table Permissions to be set")
		}
		if !tbl.Permissions.Select.ValueBool() {
			t.Error("expected table Permissions.Select=true")
		}
	})

	t.Run("wildcard_grantable_only_sets_permissions", func(t *testing.T) {
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					Wildcard: &TablePermModel{
						GrantablePermissions: &TablePermissions{Select: types.BoolValue(true)},
					},
				}},
			},
		}
		defaultPermissions(plan)
		wc := plan.Catalog.Database[0].Wildcard
		if wc.Permissions == nil {
			t.Fatal("expected wildcard Permissions to be set")
		}
		if !wc.Permissions.Select.ValueBool() {
			t.Error("expected wildcard Permissions.Select=true")
		}
	})

	t.Run("nil_catalog_no_panic", func(t *testing.T) {
		plan := &LakeFormationPermissionsResourceModel{Principal: types.StringValue(principal)}
		changed := defaultPermissions(plan)
		if changed {
			t.Error("nil catalog: expected changed=false")
		}
	})
}

// ── updatePermissions (diff-based update) ────────────────────────────────────

// TestUpdatePermissions verifies that the diff-based Update path only revokes permissions
// absent from the plan and only grants permissions absent from state.
func TestUpdatePermissions(t *testing.T) {
	const principal = "arn:aws:iam::123456789012:role/TestRole"
	const catalogID = "123456789012"
	ctx := context.Background()

	t.Run("unchanged_permissions_not_revoked_or_regranted", func(t *testing.T) {
		// SELECT exists in both state and plan → no revoke, no grant.
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					Table: []TablePermModel{{
						Name:        types.StringValue("tbl"),
						Permissions: &TablePermissions{Select: types.BoolValue(true)},
					}},
				}},
			},
		}
		plan := state // identical
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.revokeCalls) != 0 {
			t.Errorf("expected no revoke calls; got %d", len(mock.revokeCalls))
		}
		if len(mock.grantCalls) != 0 {
			t.Errorf("expected no grant calls; got %d", len(mock.grantCalls))
		}
	})

	t.Run("removed_permission_revoked_only", func(t *testing.T) {
		// State has SELECT+DESCRIBE; plan has only SELECT → DESCRIBE revoked, SELECT untouched.
		mock := mockLFClientWithPerms()
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					Table: []TablePermModel{{
						Name: types.StringValue("tbl"),
						Permissions: &TablePermissions{
							Select:   types.BoolValue(true),
							Describe: types.BoolValue(true),
						},
					}},
				}},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					Table: []TablePermModel{{
						Name:        types.StringValue("tbl"),
						Permissions: &TablePermissions{Select: types.BoolValue(true)},
					}},
				}},
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		rc := findRevokeCall(mock.revokeCalls, isTableResource("db", "tbl"))
		if rc == nil {
			t.Fatal("expected RevokePermissions call for table")
		}
		if !permsEqual(rc.Permissions, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("revoked = %v, want [DESCRIBE]", rc.Permissions)
		}
		if len(rc.PermissionsWithGrantOption) != 0 {
			t.Errorf("unexpected grantable revoke: %v", rc.PermissionsWithGrantOption)
		}
		// SELECT must not appear in any grant call.
		gc := findGrantCall(mock.grantCalls, isTableResource("db", "tbl"))
		if gc != nil {
			t.Errorf("expected no grant call for unchanged SELECT; got Permissions=%v", gc.Permissions)
		}
	})

	t.Run("added_permission_granted_only", func(t *testing.T) {
		// State has SELECT; plan has SELECT+INSERT → INSERT granted, SELECT untouched.
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					Table: []TablePermModel{{
						Name:        types.StringValue("tbl"),
						Permissions: &TablePermissions{Select: types.BoolValue(true)},
					}},
				}},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					Table: []TablePermModel{{
						Name: types.StringValue("tbl"),
						Permissions: &TablePermissions{
							Select: types.BoolValue(true),
							Insert: types.BoolValue(true),
						},
					}},
				}},
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.revokeCalls) != 0 {
			t.Errorf("expected no revoke calls; got %d", len(mock.revokeCalls))
		}
		gc := findGrantCall(mock.grantCalls, isTableResource("db", "tbl"))
		if gc == nil {
			t.Fatal("expected GrantPermissions call for table")
		}
		if !permsEqual(gc.Permissions, []lftypes.Permission{lftypes.PermissionInsert}) {
			t.Errorf("granted = %v, want [INSERT]", gc.Permissions)
		}
	})

	t.Run("all_in_plan_triggers_full_revoke_then_grant", func(t *testing.T) {
		// State has SELECT; plan has ALL → full revoke+grant cycle (ALL exception).
		mock := mockLFClientWithPerms()
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("db"),
					Permissions: &DatabasePermissions{Describe: types.BoolValue(true)},
				}},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("db"),
					Permissions: &DatabasePermissions{All: types.BoolValue(true)},
				}},
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		rc := findRevokeCall(mock.revokeCalls, isDatabaseResource("db"))
		if rc == nil {
			t.Fatal("expected RevokePermissions call (ALL exception requires full cycle)")
		}
		gc := findGrantCall(mock.grantCalls, isDatabaseResource("db"))
		if gc == nil {
			t.Fatal("expected GrantPermissions call")
		}
		if !permsEqual(gc.Permissions, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("granted = %v, want [ALL]", gc.Permissions)
		}
	})

	t.Run("all_in_state_triggers_full_revoke_then_grant", func(t *testing.T) {
		// State has ALL; plan has specific perms → full revoke+grant cycle.
		mock := mockLFClientWithPerms()
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("db"),
					Permissions: &DatabasePermissions{All: types.BoolValue(true)},
				}},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("db"),
					Permissions: &DatabasePermissions{Describe: types.BoolValue(true)},
				}},
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		rc := findRevokeCall(mock.revokeCalls, isDatabaseResource("db"))
		if rc == nil {
			t.Fatal("expected RevokePermissions call (ALL exception requires full cycle)")
		}
		gc := findGrantCall(mock.grantCalls, isDatabaseResource("db"))
		if gc == nil {
			t.Fatal("expected GrantPermissions call")
		}
		if !permsEqual(gc.Permissions, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("granted = %v, want [DESCRIBE]", gc.Permissions)
		}
	})

	t.Run("grant_option_added_without_regranting_existing_permission", func(t *testing.T) {
		// State: SELECT regular. Plan: SELECT regular + SELECT grantable.
		// SELECT already exists → no revoke. Grant call must add grant option.
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					Table: []TablePermModel{{
						Name:        types.StringValue("tbl"),
						Permissions: &TablePermissions{Select: types.BoolValue(true)},
					}},
				}},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					Table: []TablePermModel{{
						Name:                 types.StringValue("tbl"),
						Permissions:          &TablePermissions{Select: types.BoolValue(true)},
						GrantablePermissions: &TablePermissions{Select: types.BoolValue(true)},
					}},
				}},
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.revokeCalls) != 0 {
			t.Errorf("expected no revoke; got %d call(s)", len(mock.revokeCalls))
		}
		gc := findGrantCall(mock.grantCalls, isTableResource("db", "tbl"))
		if gc == nil {
			t.Fatal("expected GrantPermissions call to add grant option")
		}
		if !permsEqual(gc.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("grant option = %v, want [SELECT]", gc.PermissionsWithGrantOption)
		}
	})

	t.Run("grant_option_removed_without_revoking_regular_permission", func(t *testing.T) {
		// State: SELECT regular + SELECT grantable.
		// Plan: SELECT regular + explicit empty grantable block (removes the grant option).
		// Regular SELECT must stay; only the grant option is revoked.
		mock := mockLFClientWithPerms()
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					Table: []TablePermModel{{
						Name:                 types.StringValue("tbl"),
						Permissions:          &TablePermissions{Select: types.BoolValue(true)},
						GrantablePermissions: &TablePermissions{Select: types.BoolValue(true)},
					}},
				}},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					Table: []TablePermModel{{
						Name:                 types.StringValue("tbl"),
						Permissions:          &TablePermissions{Select: types.BoolValue(true)},
						GrantablePermissions: &TablePermissions{}, // explicit empty = remove grant option
					}},
				}},
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		rc := findRevokeCall(mock.revokeCalls, isTableResource("db", "tbl"))
		if rc == nil {
			t.Fatal("expected RevokePermissions call to remove grant option")
		}
		if len(rc.Permissions) != 0 {
			t.Errorf("regular Permissions should be empty in revoke (only removing grant option); got %v", rc.Permissions)
		}
		if !permsEqual(rc.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("PermissionsWithGrantOption = %v, want [SELECT]", rc.PermissionsWithGrantOption)
		}
		if len(mock.grantCalls) != 0 {
			t.Errorf("expected no grant calls; got %d", len(mock.grantCalls))
		}
	})

	t.Run("new_database_in_plan_fully_granted", func(t *testing.T) {
		// Plan adds a new database not present in state → full grant.
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog:   &CatalogPermModel{ID: types.StringValue(catalogID)},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("new_db"),
					Permissions: &DatabasePermissions{Describe: types.BoolValue(true)},
				}},
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		gc := findGrantCall(mock.grantCalls, isDatabaseResource("new_db"))
		if gc == nil {
			t.Fatal("expected GrantPermissions call for new database")
		}
		if !permsEqual(gc.Permissions, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("granted = %v, want [DESCRIBE]", gc.Permissions)
		}
		if len(mock.revokeCalls) != 0 {
			t.Errorf("expected no revoke calls; got %d", len(mock.revokeCalls))
		}
	})

	t.Run("removed_database_fully_revoked", func(t *testing.T) {
		// State has a database; plan does not → revoke all.
		mock := mockLFClientWithPerms()
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("old_db"),
					Permissions: &DatabasePermissions{Describe: types.BoolValue(true)},
				}},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog:   &CatalogPermModel{ID: types.StringValue(catalogID)},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		rc := findRevokeCall(mock.revokeCalls, isDatabaseResource("old_db"))
		if rc == nil {
			t.Fatal("expected RevokePermissions call for removed database")
		}
		if !permsEqual(rc.Permissions, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("revoked = %v, want [DESCRIBE]", rc.Permissions)
		}
	})
}

func TestResolveRegion(t *testing.T) {
	t.Run("resource_region_used_when_set", func(t *testing.T) {
		r := &LakeFormationPermissionsResource{awsCfg: aws.Config{Region: "us-west-2"}}
		got, err := r.resolveRegion(types.StringValue("eu-west-1"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "eu-west-1" {
			t.Errorf("got %q, want eu-west-1", got)
		}
	})

	t.Run("provider_region_used_when_resource_region_null", func(t *testing.T) {
		r := &LakeFormationPermissionsResource{awsCfg: aws.Config{Region: "us-west-2"}}
		got, err := r.resolveRegion(types.StringNull())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "us-west-2" {
			t.Errorf("got %q, want us-west-2", got)
		}
	})

	t.Run("error_when_no_region_available", func(t *testing.T) {
		r := &LakeFormationPermissionsResource{awsCfg: aws.Config{}}
		_, err := r.resolveRegion(types.StringNull())
		if err == nil {
			t.Fatal("expected error when no region is configured")
		}
	})
}
