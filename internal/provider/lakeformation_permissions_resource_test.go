// Copyright BrightAI 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	lakeformation "github.com/aws/aws-sdk-go-v2/service/lakeformation"
	lftypes "github.com/aws/aws-sdk-go-v2/service/lakeformation/types"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// ── Mock LF client ────────────────────────────────────────────────────────────

type mockLFClient struct {
	grantCalls  []*lakeformation.GrantPermissionsInput
	revokeCalls []*lakeformation.RevokePermissionsInput
	listResult  []lftypes.PrincipalResourcePermissions
	grantErr    error
	revokeErr   error
	listErr     error
	listCallIdx int

	// listPages simulates multi-page ListPermissions responses. Each element is one
	// page of results; the mock returns a NextToken between pages automatically.
	listPages [][]lftypes.PrincipalResourcePermissions

	// Optional per-call overrides. The function receives the zero-based call index and
	// returns the error for that call. Takes precedence over the error fields above.
	grantFn  func(callIdx int, in *lakeformation.GrantPermissionsInput) error
	revokeFn func(callIdx int, in *lakeformation.RevokePermissionsInput) error
	listFn   func(callIdx int, in *lakeformation.ListPermissionsInput) ([]lftypes.PrincipalResourcePermissions, error)
}

func (m *mockLFClient) GrantPermissions(_ context.Context, params *lakeformation.GrantPermissionsInput, _ ...func(*lakeformation.Options)) (*lakeformation.GrantPermissionsOutput, error) {
	idx := len(m.grantCalls)
	m.grantCalls = append(m.grantCalls, params)
	if m.grantFn != nil {
		return &lakeformation.GrantPermissionsOutput{}, m.grantFn(idx, params)
	}
	return &lakeformation.GrantPermissionsOutput{}, m.grantErr
}

func (m *mockLFClient) RevokePermissions(_ context.Context, params *lakeformation.RevokePermissionsInput, _ ...func(*lakeformation.Options)) (*lakeformation.RevokePermissionsOutput, error) {
	idx := len(m.revokeCalls)
	m.revokeCalls = append(m.revokeCalls, params)
	if m.revokeFn != nil {
		return &lakeformation.RevokePermissionsOutput{}, m.revokeFn(idx, params)
	}
	return &lakeformation.RevokePermissionsOutput{}, m.revokeErr
}

func (m *mockLFClient) ListPermissions(_ context.Context, params *lakeformation.ListPermissionsInput, _ ...func(*lakeformation.Options)) (*lakeformation.ListPermissionsOutput, error) {
	if m.listFn != nil {
		idx := m.listCallIdx
		m.listCallIdx++
		result, err := m.listFn(idx, params)
		return &lakeformation.ListPermissionsOutput{PrincipalResourcePermissions: result}, err
	}
	if m.listPages != nil {
		idx := m.listCallIdx
		m.listCallIdx++
		out := &lakeformation.ListPermissionsOutput{PrincipalResourcePermissions: m.listPages[idx]}
		if idx+1 < len(m.listPages) {
			out.NextToken = aws.String("page-token")
		}
		return out, nil
	}
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

// mockWithCurrentPerms returns a mock whose ListPermissions always reports the given
// currently-held permissions. Use this when a test expects specific permissions to be
// revoked — the intersection of the requested revoke with the current list must be non-empty.
func mockWithCurrentPerms(perms []lftypes.Permission, grantPerms []lftypes.Permission) *mockLFClient {
	return &mockLFClient{
		listResult: []lftypes.PrincipalResourcePermissions{
			{
				Permissions:                perms,
				PermissionsWithGrantOption: grantPerms,
			},
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
		input *Permissions
		want  []lftypes.Permission
	}{
		{
			name:  "nil",
			input: nil,
			want:  nil,
		},
		{
			name:  "all_false",
			input: &Permissions{},
			want:  nil,
		},
		{
			name:  "create_database_only",
			input: &Permissions{CreateDatabase: true},
			want:  []lftypes.Permission{lftypes.PermissionCreateDatabase},
		},
		{
			name: "describe_and_alter",
			input: &Permissions{
				Describe: true,
				Alter:    true,
			},
			want: []lftypes.Permission{lftypes.PermissionAlter, lftypes.PermissionDescribe},
		},
		{
			name: "all_individual_true_returns_each_perm",
			input: &Permissions{
				Alter:          true,
				CreateCatalog:  true,
				CreateDatabase: true,
				Describe:       true,
				Drop:           true,
			},
			want: []lftypes.Permission{
				lftypes.PermissionAlter, lftypes.PermissionCreateCatalog,
				lftypes.PermissionCreateDatabase, lftypes.PermissionDescribe,
				lftypes.PermissionDrop,
			},
		},
		{
			name:  "all_true_returns_ALL",
			input: &Permissions{All: true},
			want:  []lftypes.Permission{lftypes.PermissionAll},
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
		input *Permissions
		want  []lftypes.Permission
	}{
		{name: "nil", input: nil, want: nil},
		{name: "all_false", input: &Permissions{}, want: nil},
		{
			name:  "describe_only",
			input: &Permissions{Describe: true},
			want:  []lftypes.Permission{lftypes.PermissionDescribe},
		},
		{
			name:  "create_table_and_drop",
			input: &Permissions{CreateTable: true, Drop: true},
			want:  []lftypes.Permission{lftypes.PermissionCreateTable, lftypes.PermissionDrop},
		},
		{
			name: "all_individual_true_returns_each_perm",
			input: &Permissions{
				Alter:       true,
				CreateTable: true,
				Describe:    true,
				Drop:        true,
			},
			want: []lftypes.Permission{
				lftypes.PermissionAlter, lftypes.PermissionCreateTable,
				lftypes.PermissionDescribe, lftypes.PermissionDrop,
			},
		},
		{
			name:  "all_true_returns_ALL",
			input: &Permissions{All: true},
			want:  []lftypes.Permission{lftypes.PermissionAll},
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
		input *Permissions
		want  []lftypes.Permission
	}{
		{name: "nil", input: nil, want: nil},
		{name: "all_false", input: &Permissions{}, want: nil},
		{
			name:  "select_only",
			input: &Permissions{Select: true},
			want:  []lftypes.Permission{lftypes.PermissionSelect},
		},
		{
			name: "select_and_describe",
			input: &Permissions{
				Select:   true,
				Describe: true,
			},
			want: []lftypes.Permission{lftypes.PermissionDescribe, lftypes.PermissionSelect},
		},
		{
			name: "insert_and_delete",
			input: &Permissions{
				Insert: true,
				Delete: true,
			},
			want: []lftypes.Permission{lftypes.PermissionDelete, lftypes.PermissionInsert},
		},
		{
			name: "all_individual_true_returns_each_perm",
			input: &Permissions{
				Alter:    true,
				Delete:   true,
				Describe: true,
				Drop:     true,
				Insert:   true,
				Select:   true,
			},
			want: []lftypes.Permission{
				lftypes.PermissionAlter, lftypes.PermissionDelete, lftypes.PermissionDescribe,
				lftypes.PermissionDrop, lftypes.PermissionInsert, lftypes.PermissionSelect,
			},
		},
		{
			name:  "all_true_returns_ALL",
			input: &Permissions{All: true},
			want:  []lftypes.Permission{lftypes.PermissionAll},
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
	t.Run("catalog", func(t *testing.T) {
		t.Run("nil_returns_nil", func(t *testing.T) {
			if refreshPerms((*Permissions)(nil), nil) != nil {
				t.Error("expected nil")
			}
		})
		t.Run("declared_perm_present_in_current", func(t *testing.T) {
			got := refreshPerms(&Permissions{CreateDatabase: true},
				[]lftypes.Permission{lftypes.PermissionCreateDatabase})
			want := &Permissions{CreateDatabase: true}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("declared_perm_absent_becomes_false", func(t *testing.T) {
			got := refreshPerms(&Permissions{CreateDatabase: true, Describe: true},
				[]lftypes.Permission{lftypes.PermissionCreateDatabase})
			// Describe was declared true but is absent from current → explicit false.
			// Undeclared fields (Alter, CreateCatalog, Drop) → false (zero value).
			want := &Permissions{CreateDatabase: true, Describe: false}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("ALL_sets_all_declared_true", func(t *testing.T) {
			got := refreshPerms(&Permissions{Alter: true, CreateDatabase: true},
				[]lftypes.Permission{lftypes.PermissionAll})
			want := &Permissions{Alter: true, CreateDatabase: true}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("undeclared_perms_not_tracked", func(t *testing.T) {
			// All fields null in declared → all returned as null regardless of current.
			got := refreshPerms(&Permissions{},
				[]lftypes.Permission{lftypes.PermissionCreateDatabase, lftypes.PermissionDescribe})
			want := &Permissions{}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	})

	t.Run("database", func(t *testing.T) {
		t.Run("nil_returns_nil", func(t *testing.T) {
			if refreshPerms((*Permissions)(nil), nil) != nil {
				t.Error("expected nil")
			}
		})
		t.Run("describe_present", func(t *testing.T) {
			got := refreshPerms(&Permissions{Describe: true},
				[]lftypes.Permission{lftypes.PermissionDescribe})
			want := &Permissions{Describe: true}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("perm_revoked_externally", func(t *testing.T) {
			got := refreshPerms(&Permissions{CreateTable: true, Alter: true},
				[]lftypes.Permission{lftypes.PermissionAlter})
			// CreateTable declared true but absent → explicit false.
			want := &Permissions{Alter: true, CreateTable: false}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("ALL_expands_to_declared", func(t *testing.T) {
			got := refreshPerms(&Permissions{Alter: true, Drop: true},
				[]lftypes.Permission{lftypes.PermissionAll})
			want := &Permissions{Alter: true, Drop: true}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	})

	t.Run("table", func(t *testing.T) {
		t.Run("nil_returns_nil", func(t *testing.T) {
			if refreshPerms((*Permissions)(nil), nil) != nil {
				t.Error("expected nil")
			}
		})
		t.Run("select_and_describe_present", func(t *testing.T) {
			got := refreshPerms(&Permissions{Select: true, Describe: true},
				[]lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionDescribe})
			want := &Permissions{Describe: true, Select: true}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("perm_revoked_externally", func(t *testing.T) {
			got := refreshPerms(&Permissions{Select: true, Insert: true},
				[]lftypes.Permission{lftypes.PermissionSelect})
			// Insert declared true but absent → explicit false.
			want := &Permissions{Insert: false, Select: true}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("ALL_expands_to_declared", func(t *testing.T) {
			got := refreshPerms(&Permissions{Select: true, Delete: true},
				[]lftypes.Permission{lftypes.PermissionAll})
			want := &Permissions{Delete: true, Select: true}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
		t.Run("undeclared_perm_in_current_not_tracked", func(t *testing.T) {
			got := refreshPerms(&Permissions{Select: true},
				[]lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionInsert})
			want := &Permissions{Select: true}
			if *got != *want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	})

	t.Run("all_field", func(t *testing.T) {
		t.Run("all_true_declared_ALL_in_current_stays_true", func(t *testing.T) {
			got := refreshPerms(&Permissions{All: true},
				[]lftypes.Permission{lftypes.PermissionAll})
			if !got.All {
				t.Errorf("All: got false, want true")
			}
		})
		t.Run("all_true_declared_ALL_absent_becomes_false", func(t *testing.T) {
			// AWS returns only individual perms (partial set — not collapsed to ALL).
			got := refreshPerms(&Permissions{All: true},
				[]lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionDescribe})
			if got.All {
				t.Errorf("All: got true, want false when ALL absent from current")
			}
		})
		t.Run("all_true_declared_empty_current_all_false", func(t *testing.T) {
			got := refreshPerms(&Permissions{All: true}, nil)
			if got.All {
				t.Errorf("All: got true, want false when current is empty")
			}
		})
	})
}

// TestRefreshBool was removed — refreshBool no longer exists; permission refresh
// is handled directly in refreshPerms using plain bool fields.

// ── no-drift guarantees ───────────────────────────────────────────────────────

// TestNoDriftForOmittedPermissions verifies the core invariant: Terraform should
// not plan an Update when external actors change permissions that are not declared
// in the resource config (either because the whole block is absent, or because
// individual flags within a declared block are not configured).
//
// With plain bool fields, undeclared permissions default to false. refreshPerms
// only sets a field true when (a) it was declared true AND (b) it is active in AWS.
// Fields declared false remain false regardless of AWS state → no phantom diffs.
func TestNoDriftForOmittedPermissions(t *testing.T) {
	anyPerms := []lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionDescribe}

	// Nil permissions block (block omitted in config) → refreshPerms returns nil.
	// plan: nil (Optional attr not set) == state: nil → no diff → no Update.
	t.Run("nil_catalog_permissions_block_returns_nil", func(t *testing.T) {
		if got := refreshPerms((*Permissions)(nil), anyPerms); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})
	t.Run("nil_database_permissions_block_returns_nil", func(t *testing.T) {
		if got := refreshPerms((*Permissions)(nil), anyPerms); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})
	t.Run("nil_table_permissions_block_returns_nil", func(t *testing.T) {
		if got := refreshPerms((*Permissions)(nil), anyPerms); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})
	t.Run("nil_grantable_permissions_block_returns_nil", func(t *testing.T) {
		// Same nil semantics apply to grantable_permissions.
		if got := refreshPerms((*Permissions)(nil), anyPerms); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	// Undeclared flag (false) within a present permissions block stays false in state.
	// plan: false == state: false → no diff for that flag.
	t.Run("undeclared_flag_stays_false_even_when_granted_in_aws", func(t *testing.T) {
		// permissions { select = true } — ALTER not declared (false).
		// AWS externally grants ALTER. State must not track it.
		got := refreshPerms(
			&Permissions{Select: true},
			[]lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionAlter},
		)
		if got.Alter != false {
			t.Errorf("undeclared Alter: want false in state, got %v", got.Alter)
		}
		if got.Select != true {
			t.Errorf("declared Select: want true, got %v", got.Select)
		}
	})
	t.Run("undeclared_database_flag_stays_false_in_state", func(t *testing.T) {
		// permissions { describe = true } — CREATE_TABLE not declared (false).
		got := refreshPerms(
			&Permissions{Describe: true},
			[]lftypes.Permission{lftypes.PermissionDescribe, lftypes.PermissionCreateTable},
		)
		if got.CreateTable != false {
			t.Errorf("undeclared CreateTable: want false, got %v", got.CreateTable)
		}
	})

	// Declared permission still granted → true in state == true in plan → no diff.
	t.Run("declared_perm_present_no_drift", func(t *testing.T) {
		got := refreshPerms(
			&Permissions{Select: true},
			[]lftypes.Permission{lftypes.PermissionSelect},
		)
		if got.Select != true {
			t.Errorf("declared Select still granted: want true, got %v", got.Select)
		}
	})

	// Declared permission externally revoked → false in state ≠ true in plan → Update.
	t.Run("declared_perm_revoked_triggers_drift", func(t *testing.T) {
		got := refreshPerms(
			&Permissions{Select: true},
			[]lftypes.Permission{}, // SELECT absent
		)
		if got.Select != false {
			t.Errorf("declared Select externally revoked: want false (drift signal), got %v", got.Select)
		}
	})
	t.Run("declared_catalog_perm_revoked_triggers_drift", func(t *testing.T) {
		got := refreshPerms(
			&Permissions{CreateDatabase: true},
			[]lftypes.Permission{}, // CREATE_DATABASE absent
		)
		if got.CreateDatabase != false {
			t.Errorf("declared CreateDatabase revoked: want false, got %v", got.CreateDatabase)
		}
	})

	// ALL in AWS covers all declared permissions — no drift even if declared via individual flags.
	t.Run("all_in_aws_satisfies_individual_declared_flags", func(t *testing.T) {
		got := refreshPerms(
			&Permissions{Select: true, Insert: true},
			[]lftypes.Permission{lftypes.PermissionAll},
		)
		if got.Select != true {
			t.Errorf("Select covered by ALL: want true, got %v", got.Select)
		}
		if got.Insert != true {
			t.Errorf("Insert covered by ALL: want true, got %v", got.Insert)
		}
		if !got.Alter== false {
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
		got := permsToAPI(&Permissions{All: true})
		if !permsEqual(got, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("catalogPermsToAPI all=true = %v, want [ALL]", got)
		}
	})
	t.Run("database_all_true_sends_ALL_to_api", func(t *testing.T) {
		got := permsToAPI(&Permissions{All: true})
		if !permsEqual(got, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("databasePermsToAPI all=true = %v, want [ALL]", got)
		}
	})
	t.Run("table_all_true_sends_ALL_to_api", func(t *testing.T) {
		got := permsToAPI(&Permissions{All: true})
		if !permsEqual(got, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("tablePermsToAPI all=true = %v, want [ALL]", got)
		}
	})

	// all=true persists in state and is refreshed via the same tfsdk-tag→permission
	// mechanism as individual flags ("all" uppercases to "ALL").
	t.Run("all_true_refreshed_stays_true_when_ALL_active", func(t *testing.T) {
		got := refreshPerms(
			&Permissions{All: true},
			[]lftypes.Permission{lftypes.PermissionAll},
		)
		if got.All != true {
			t.Errorf("all=true with ALL in AWS: want true, got %v", got.All)
		}
	})
	t.Run("all_true_refreshed_becomes_false_when_ALL_revoked", func(t *testing.T) {
		// ALL was externally revoked. State flips to false → plan still has all=true →
		// Terraform detects drift and triggers an Update to re-grant ALL.
		got := refreshPerms(
			&Permissions{All: true},
			[]lftypes.Permission{},
		)
		if got.All != false {
			t.Errorf("all=true with ALL revoked: want false (drift signal), got %v", got.All)
		}
	})
	t.Run("all_true_individual_flags_remain_false_in_state", func(t *testing.T) {
		// When all=true, individual flags are not declared by the user and must
		// stay false in state so they don't produce phantom plan diffs.
		got := refreshPerms(
			&Permissions{All: true},
			[]lftypes.Permission{lftypes.PermissionAll},
		)
		if got.Select != false || got.Alter != false || got.Insert != false {
			t.Errorf("undeclared individual flags must be false when all=true is used; got %+v", got)
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
				Permissions: &Permissions{All: true},
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
				Permissions: &Permissions{
					CreateDatabase: true,
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
				Permissions:          &Permissions{Describe: true},
				GrantablePermissions: &Permissions{Describe: true},
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
								Permissions:          &Permissions{Select: true},
								GrantablePermissions: &Permissions{Select: true},
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
								Permissions:          &Permissions{Describe: true, Select: true},
								GrantablePermissions: &Permissions{Describe: true},
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

	t.Run("grantable_only_database_permissions_equal", func(t *testing.T) {
		// When only grantable_permissions is configured at the database level,
		// ModifyPlan sets permissions = grantable_permissions before grantAll runs.
		mock := &mockLFClient{}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:                 types.StringValue("analytics"),
					Permissions:          &Permissions{Describe: true},
					GrantablePermissions: &Permissions{Describe: true},
				}},
			},
		}
		if err := grantAll(ctx, mock, data); err != nil {
			t.Fatalf("grantAll() error: %v", err)
		}
		call := findGrantCall(mock.grantCalls, isDatabaseResource("analytics"))
		if call == nil {
			t.Fatal("expected GrantPermissions call for database resource")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("permissions = %v, want [DESCRIBE]", call.Permissions)
		}
		if !permsEqual(call.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("grantable = %v, want [DESCRIBE]", call.PermissionsWithGrantOption)
		}
	})

	t.Run("grantable_only_all_levels_permissions_auto_filled_and_granted", func(t *testing.T) {
		// End-to-end: user specifies only grantable_permissions at catalog, database, and
		// table levels. defaultPermissions fills in permissions = grantable_permissions at each
		// level, then grantAll sends correct Permissions + PermissionsWithGrantOption to AWS.
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID:                   types.StringValue(catalogID),
				GrantablePermissions: &Permissions{Describe: true},
				Database: []DatabasePermModel{{
					Name:                 types.StringValue("analytics"),
					GrantablePermissions: &Permissions{CreateTable: true},
					Table: []TablePermModel{{
						Name:                 types.StringValue("events"),
						GrantablePermissions: &Permissions{Select: true},
					}},
				}},
			},
		}
		defaultPermissions(plan)

		// Step 2: Verify all three levels have permissions == grantable_permissions.
		if plan.Catalog.Permissions == nil || !plan.Catalog.Permissions.Describe {
			t.Fatal("catalog.permissions not set from grantable_permissions")
		}
		db := plan.Catalog.Database[0]
		if db.Permissions == nil || !db.Permissions.CreateTable {
			t.Fatal("database.permissions not set from grantable_permissions")
		}
		tbl := db.Table[0]
		if tbl.Permissions == nil || !tbl.Permissions.Select {
			t.Fatal("table.permissions not set from grantable_permissions")
		}

		// Step 3: grantAll sends correct API calls at all three levels.
		mock := &mockLFClient{}
		if err := grantAll(ctx, mock, plan); err != nil {
			t.Fatalf("grantAll() error: %v", err)
		}

		catCall := findGrantCall(mock.grantCalls, isCatalogResource)
		if catCall == nil {
			t.Fatal("expected GrantPermissions call for catalog")
		}
		if !permsEqual(catCall.Permissions, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("catalog permissions = %v, want [DESCRIBE]", catCall.Permissions)
		}
		if !permsEqual(catCall.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("catalog grantable = %v, want [DESCRIBE]", catCall.PermissionsWithGrantOption)
		}

		dbCall := findGrantCall(mock.grantCalls, isDatabaseResource("analytics"))
		if dbCall == nil {
			t.Fatal("expected GrantPermissions call for database")
		}
		if !permsEqual(dbCall.Permissions, []lftypes.Permission{lftypes.PermissionCreateTable}) {
			t.Errorf("database permissions = %v, want [CREATE_TABLE]", dbCall.Permissions)
		}
		if !permsEqual(dbCall.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionCreateTable}) {
			t.Errorf("database grantable = %v, want [CREATE_TABLE]", dbCall.PermissionsWithGrantOption)
		}

		tblCall := findGrantCall(mock.grantCalls, isTableResource("analytics", "events"))
		if tblCall == nil {
			t.Fatal("expected GrantPermissions call for table")
		}
		if !permsEqual(tblCall.Permissions, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("table permissions = %v, want [SELECT]", tblCall.Permissions)
		}
		if !permsEqual(tblCall.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("table grantable = %v, want [SELECT]", tblCall.PermissionsWithGrantOption)
		}
	})

	t.Run("nil_catalog_no_api_calls", func(t *testing.T) {
		mock := &mockLFClient{grantErr: fmt.Errorf("should not be called")}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			// Catalog is nil
		}
		if err := grantAll(ctx, mock, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.grantCalls) != 0 {
			t.Errorf("expected no grant calls; got %d", len(mock.grantCalls))
		}
	})

	t.Run("grant_error_propagated", func(t *testing.T) {
		mock := &mockLFClient{grantErr: fmt.Errorf("access denied")}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID:          types.StringValue(catalogID),
				Permissions: &Permissions{Describe: true},
			},
		}
		if err := grantAll(ctx, mock, data); err == nil {
			t.Fatal("expected error from grantAll, got nil")
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
						Permissions: &Permissions{
							Describe:    true,
							CreateTable: true,
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
								Permissions: &Permissions{
									Select:   true,
									Describe: true,
								},
								GrantablePermissions: &Permissions{
									Select: true,
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
							Permissions: &Permissions{
								Select: true,
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
								Permissions: &Permissions{
									Alter:    true,
									Delete:   true,
									Describe: true,
									Drop:     true,
									Insert:   true,
									Select:   true,
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

	t.Run("all_true_no_existing_perms_no_revoke", func(t *testing.T) {
		// all=true with no current permissions: grant ALL but no revoke call.
		mock := &mockLFClient{}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID:          types.StringValue(catalogID),
				Permissions: &Permissions{All: true},
			},
		}
		if err := grantAll(ctx, mock, data); err != nil {
			t.Fatalf("grantAll() error: %v", err)
		}
		if call := findRevokeCall(mock.revokeCalls, isCatalogResource); call != nil {
			t.Errorf("expected no revoke call when no existing permissions; got %+v", call)
		}
		call := findGrantCall(mock.grantCalls, isCatalogResource)
		if call == nil {
			t.Fatal("expected GrantPermissions call for catalog")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("permissions = %v, want [ALL]", call.Permissions)
		}
	})

	t.Run("all_true_with_existing_individual_perms_revokes_then_grants", func(t *testing.T) {
		// all=true in plan, resource currently holds [SELECT, DESCRIBE].
		// applyDiff sees ALL in planP → full revoke+grant cycle.
		mock := mockWithCurrentPerms(
			[]lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionDescribe},
			nil,
		)
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("analytics"),
					Permissions: &Permissions{All: true},
				}},
			},
		}
		if err := grantAll(ctx, mock, data); err != nil {
			t.Fatalf("grantAll() error: %v", err)
		}
		revokeCall := findRevokeCall(mock.revokeCalls, isDatabaseResource("analytics"))
		if revokeCall == nil {
			t.Fatal("expected RevokePermissions call to clear existing permissions before granting ALL")
		}
		if !permsEqual(revokeCall.Permissions, []lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionDescribe}) {
			t.Errorf("revoke permissions = %v, want [SELECT, DESCRIBE]", revokeCall.Permissions)
		}
		grantCall := findGrantCall(mock.grantCalls, isDatabaseResource("analytics"))
		if grantCall == nil {
			t.Fatal("expected GrantPermissions call for database after revoke")
		}
		if !permsEqual(grantCall.Permissions, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("grant permissions = %v, want [ALL]", grantCall.Permissions)
		}
	})

	t.Run("grantable_all_true_with_existing_perms_revokes_then_grants", func(t *testing.T) {
		// grantable_permissions.all=true (ModifyPlan has set permissions=grantable_permissions).
		// Resource currently holds [SELECT] with no grant option.
		// applyDiff: ALL in planP → revoke [SELECT], grant ALL with PermissionsWithGrantOption=[ALL].
		mock := mockWithCurrentPerms(
			[]lftypes.Permission{lftypes.PermissionSelect},
			nil,
		)
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("analytics"),
					Table: []TablePermModel{{
						Name:                 types.StringValue("events"),
						Permissions:          &Permissions{All: true},
						GrantablePermissions: &Permissions{All: true},
					}},
				}},
			},
		}
		if err := grantAll(ctx, mock, data); err != nil {
			t.Fatalf("grantAll() error: %v", err)
		}
		revokeCall := findRevokeCall(mock.revokeCalls, isTableResource("analytics", "events"))
		if revokeCall == nil {
			t.Fatal("expected RevokePermissions call before granting ALL")
		}
		if !permsEqual(revokeCall.Permissions, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("revoke permissions = %v, want [SELECT]", revokeCall.Permissions)
		}
		grantCall := findGrantCall(mock.grantCalls, isTableResource("analytics", "events"))
		if grantCall == nil {
			t.Fatal("expected GrantPermissions call for table after revoke")
		}
		if !permsEqual(grantCall.Permissions, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("grant permissions = %v, want [ALL]", grantCall.Permissions)
		}
		if !permsEqual(grantCall.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("grant grantable = %v, want [ALL]", grantCall.PermissionsWithGrantOption)
		}
	})

	t.Run("existing_all_with_individual_plan_revokes_then_grants", func(t *testing.T) {
		// Resource currently holds ALL; plan sets specific individual permissions.
		// applyDiff sees ALL in curP → full revoke+grant cycle.
		mock := mockWithCurrentPerms(
			[]lftypes.Permission{lftypes.PermissionAll},
			nil,
		)
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("analytics"),
					Permissions: &Permissions{Select: true, Describe: true},
				}},
			},
		}
		if err := grantAll(ctx, mock, data); err != nil {
			t.Fatalf("grantAll() error: %v", err)
		}
		revokeCall := findRevokeCall(mock.revokeCalls, isDatabaseResource("analytics"))
		if revokeCall == nil {
			t.Fatal("expected RevokePermissions call to clear existing ALL before granting individual permissions")
		}
		if !permsEqual(revokeCall.Permissions, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("revoke permissions = %v, want [ALL]", revokeCall.Permissions)
		}
		grantCall := findGrantCall(mock.grantCalls, isDatabaseResource("analytics"))
		if grantCall == nil {
			t.Fatal("expected GrantPermissions call for database after revoke")
		}
		if !permsEqual(grantCall.Permissions, []lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionDescribe}) {
			t.Errorf("grant permissions = %v, want [SELECT, DESCRIBE]", grantCall.Permissions)
		}
	})

	t.Run("multiple_databases_granted_independently", func(t *testing.T) {
		// Two databases in plan: each gets its own grant call with the correct permissions.
		mock := &mockLFClient{}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{
					{Name: types.StringValue("db1"), Permissions: &Permissions{Describe: true}},
					{Name: types.StringValue("db2"), Permissions: &Permissions{CreateTable: true}},
				},
			},
		}
		if err := grantAll(ctx, mock, data); err != nil {
			t.Fatalf("grantAll() error: %v", err)
		}
		call1 := findGrantCall(mock.grantCalls, isDatabaseResource("db1"))
		if call1 == nil {
			t.Fatal("expected GrantPermissions call for db1")
		}
		if !permsEqual(call1.Permissions, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("db1 permissions = %v, want [DESCRIBE]", call1.Permissions)
		}
		call2 := findGrantCall(mock.grantCalls, isDatabaseResource("db2"))
		if call2 == nil {
			t.Fatal("expected GrantPermissions call for db2")
		}
		if !permsEqual(call2.Permissions, []lftypes.Permission{lftypes.PermissionCreateTable}) {
			t.Errorf("db2 permissions = %v, want [CREATE_TABLE]", call2.Permissions)
		}
	})

	t.Run("nil_db_permissions_but_table_granted", func(t *testing.T) {
		// Database-level permissions nil → no DB grant; table-level permissions set → table grant fires.
		mock := &mockLFClient{}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("analytics"), // Permissions nil at DB level
					Table: []TablePermModel{{
						Name:        types.StringValue("events"),
						Permissions: &Permissions{Select: true},
					}},
				}},
			},
		}
		if err := grantAll(ctx, mock, data); err != nil {
			t.Fatalf("grantAll() error: %v", err)
		}
		if call := findGrantCall(mock.grantCalls, isDatabaseResource("analytics")); call != nil {
			t.Errorf("expected no grant call for database with nil permissions; got %+v", call)
		}
		tblCall := findGrantCall(mock.grantCalls, isTableResource("analytics", "events"))
		if tblCall == nil {
			t.Fatal("expected GrantPermissions call for table despite nil database permissions")
		}
		if !permsEqual(tblCall.Permissions, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("table permissions = %v, want [SELECT]", tblCall.Permissions)
		}
	})

	t.Run("database_error_propagated", func(t *testing.T) {
		mock := mockWithCurrentPerms(nil, nil)
		mock.grantErr = fmt.Errorf("access denied")
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("analytics"),
					Permissions: &Permissions{Describe: true},
				}},
			},
		}
		if err := grantAll(ctx, mock, data); err == nil {
			t.Fatal("expected error from database grant, got nil")
		}
	})

	t.Run("table_error_propagated", func(t *testing.T) {
		mock := mockWithCurrentPerms(nil, nil)
		mock.grantErr = fmt.Errorf("access denied")
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("analytics"),
					Table: []TablePermModel{{
						Name:        types.StringValue("events"),
						Permissions: &Permissions{Select: true},
					}},
				}},
			},
		}
		if err := grantAll(ctx, mock, data); err == nil {
			t.Fatal("expected error from table grant, got nil")
		}
	})
}

// ── revokeAll ─────────────────────────────────────────────────────────────────

// ── revokeLFPerms ─────────────────────────────────────────────────────────────

func TestRevokeLFPerms(t *testing.T) {
	ctx := context.Background()
	principal := "arn:aws:iam::123456789012:role/role"
	res := &lftypes.Resource{Catalog: &lftypes.CatalogResource{Id: aws.String("123456789012")}}

	t.Run("both_empty_no_api_call", func(t *testing.T) {
		mock := &mockLFClient{}
		if err := revokeLFPerms(ctx, mock, principal, res, nil, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.revokeCalls) != 0 {
			t.Errorf("expected no RevokePermissions call; got %d", len(mock.revokeCalls))
		}
	})

	t.Run("perms_only_no_grant_option", func(t *testing.T) {
		mock := &mockLFClient{}
		if err := revokeLFPerms(ctx, mock, principal, res,
			[]lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionDescribe}, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.revokeCalls) != 1 {
			t.Fatalf("expected 1 revoke call; got %d", len(mock.revokeCalls))
		}
		call := mock.revokeCalls[0]
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionDescribe}) {
			t.Errorf("Permissions = %v, want [SELECT, DESCRIBE]", call.Permissions)
		}
		if len(call.PermissionsWithGrantOption) != 0 {
			t.Errorf("PermissionsWithGrantOption = %v, want empty", call.PermissionsWithGrantOption)
		}
	})

	t.Run("perms_and_grantable_both_sent", func(t *testing.T) {
		mock := &mockLFClient{}
		if err := revokeLFPerms(ctx, mock, principal, res,
			[]lftypes.Permission{lftypes.PermissionSelect},
			[]lftypes.Permission{lftypes.PermissionSelect}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		call := mock.revokeCalls[0]
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("Permissions = %v, want [SELECT]", call.Permissions)
		}
		if !permsEqual(call.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("PermissionsWithGrantOption = %v, want [SELECT]", call.PermissionsWithGrantOption)
		}
	})

	t.Run("error_propagated", func(t *testing.T) {
		mock := &mockLFClient{revokeErr: fmt.Errorf("access denied")}
		err := revokeLFPerms(ctx, mock, principal, res,
			[]lftypes.Permission{lftypes.PermissionSelect}, nil)
		if err == nil {
			t.Fatal("expected error from RevokePermissions, got nil")
		}
	})
}

// ── grantLFPerms ──────────────────────────────────────────────────────────────

func TestGrantLFPerms(t *testing.T) {
	ctx := context.Background()
	principal := "arn:aws:iam::123456789012:role/role"
	res := &lftypes.Resource{Catalog: &lftypes.CatalogResource{Id: aws.String("123456789012")}}

	t.Run("both_empty_no_api_call", func(t *testing.T) {
		mock := &mockLFClient{}
		if err := grantLFPerms(ctx, mock, principal, res, nil, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.grantCalls) != 0 {
			t.Errorf("expected no GrantPermissions call; got %d", len(mock.grantCalls))
		}
	})

	t.Run("perms_only_no_grant_option", func(t *testing.T) {
		mock := &mockLFClient{}
		if err := grantLFPerms(ctx, mock, principal, res,
			[]lftypes.Permission{lftypes.PermissionSelect}, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.grantCalls) != 1 {
			t.Fatalf("expected 1 grant call; got %d", len(mock.grantCalls))
		}
		call := mock.grantCalls[0]
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("Permissions = %v, want [SELECT]", call.Permissions)
		}
		if len(call.PermissionsWithGrantOption) != 0 {
			t.Errorf("PermissionsWithGrantOption = %v, want empty", call.PermissionsWithGrantOption)
		}
	})

	t.Run("grantable_already_subset_of_perms_no_merge_needed", func(t *testing.T) {
		// perms=[SELECT, DESCRIBE], grantPerms=[SELECT] — grantPerms ⊆ perms already.
		// permUnion is a no-op; API call sends Permissions as-is.
		mock := &mockLFClient{}
		if err := grantLFPerms(ctx, mock, principal, res,
			[]lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionDescribe},
			[]lftypes.Permission{lftypes.PermissionSelect}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		call := mock.grantCalls[0]
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionDescribe}) {
			t.Errorf("Permissions = %v, want [SELECT, DESCRIBE]", call.Permissions)
		}
		if !permsEqual(call.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("PermissionsWithGrantOption = %v, want [SELECT]", call.PermissionsWithGrantOption)
		}
	})

	t.Run("grantable_not_in_perms_merged_before_api_call", func(t *testing.T) {
		// applyDiff can produce grantP=[] with grantG=[SELECT] when a grant option is
		// being added for a permission the principal already holds. grantLFPerms must merge
		// SELECT into perms so the API receives a valid non-empty Permissions list.
		mock := &mockLFClient{}
		if err := grantLFPerms(ctx, mock, principal, res,
			nil,
			[]lftypes.Permission{lftypes.PermissionSelect}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		call := mock.grantCalls[0]
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("Permissions = %v, want [SELECT] (merged from grantPerms)", call.Permissions)
		}
		if !permsEqual(call.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("PermissionsWithGrantOption = %v, want [SELECT]", call.PermissionsWithGrantOption)
		}
	})

	t.Run("error_propagated", func(t *testing.T) {
		mock := &mockLFClient{grantErr: fmt.Errorf("access denied")}
		err := grantLFPerms(ctx, mock, principal, res,
			[]lftypes.Permission{lftypes.PermissionSelect}, nil)
		if err == nil {
			t.Fatal("expected error from GrantPermissions, got nil")
		}
	})
}

// ── readPermissions ───────────────────────────────────────────────────────────

func TestReadPermissions(t *testing.T) {
	const principal = "arn:aws:iam::123456789012:role/TestRole"
	const catalogID = "123456789012"
	ctx := context.Background()

	// listByResource returns a mock whose ListPermissions dispatches based on resource type.
	listByResource := func(
		catalogResult, dbResult, tableResult, wildcardResult []lftypes.Permission,
		catalogErr, dbErr, tableErr, wildcardErr error,
	) *mockLFClient {
		return &mockLFClient{
			listFn: func(_ int, input *lakeformation.ListPermissionsInput) ([]lftypes.PrincipalResourcePermissions, error) {
				res := input.Resource
				switch {
				case res.Catalog != nil:
					if catalogErr != nil {
						return nil, catalogErr
					}
					return []lftypes.PrincipalResourcePermissions{{Permissions: catalogResult}}, nil
				case res.Database != nil:
					if dbErr != nil {
						return nil, dbErr
					}
					return []lftypes.PrincipalResourcePermissions{{Permissions: dbResult}}, nil
				case res.Table != nil && res.Table.TableWildcard != nil:
					if wildcardErr != nil {
						return nil, wildcardErr
					}
					return []lftypes.PrincipalResourcePermissions{{Permissions: wildcardResult}}, nil
				default: // named table
					if tableErr != nil {
						return nil, tableErr
					}
					return []lftypes.PrincipalResourcePermissions{{Permissions: tableResult}}, nil
				}
			},
		}
	}

	t.Run("nil_catalog_no_api_calls", func(t *testing.T) {
		mock := &mockLFClient{listErr: fmt.Errorf("should not be called")}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
		}
		if err := readPermissions(ctx, mock, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("catalog_permissions_nil_skips_catalog_list", func(t *testing.T) {
		listCalled := false
		mock := &mockLFClient{
			listFn: func(_ int, input *lakeformation.ListPermissionsInput) ([]lftypes.PrincipalResourcePermissions, error) {
				if input.Resource.Catalog != nil {
					listCalled = true
				}
				return nil, nil
			},
		}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog:   &CatalogPermModel{ID: types.StringValue(catalogID)},
		}
		if err := readPermissions(ctx, mock, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if listCalled {
			t.Error("expected no catalog list call when Permissions is nil")
		}
	})

	t.Run("catalog_permissions_refreshed_from_aws", func(t *testing.T) {
		mock := listByResource(
			[]lftypes.Permission{lftypes.PermissionDescribe}, nil, nil, nil,
			nil, nil, nil, nil,
		)
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:          types.StringValue(catalogID),
				Permissions: &Permissions{Describe: true},
			},
		}
		if err := readPermissions(ctx, mock, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if data.Catalog.Permissions == nil || !data.Catalog.Permissions.Describe {
			t.Error("expected catalog Permissions.Describe=true after refresh")
		}
	})

	t.Run("catalog_list_error_propagated", func(t *testing.T) {
		mock := listByResource(nil, nil, nil, nil, fmt.Errorf("catalog list failed"), nil, nil, nil)
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:          types.StringValue(catalogID),
				Permissions: &Permissions{Describe: true},
			},
		}
		if err := readPermissions(ctx, mock, data); err == nil {
			t.Error("expected error from catalog list failure")
		}
	})

	t.Run("database_permissions_nil_skips_db_list", func(t *testing.T) {
		listCalled := false
		mock := &mockLFClient{
			listFn: func(_ int, input *lakeformation.ListPermissionsInput) ([]lftypes.PrincipalResourcePermissions, error) {
				if input.Resource.Database != nil {
					listCalled = true
				}
				return nil, nil
			},
		}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:       types.StringValue(catalogID),
				Database: []DatabasePermModel{{Name: types.StringValue("db")}},
			},
		}
		if err := readPermissions(ctx, mock, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if listCalled {
			t.Error("expected no database list call when Permissions is nil")
		}
	})

	t.Run("database_permissions_refreshed_from_aws", func(t *testing.T) {
		mock := listByResource(nil, []lftypes.Permission{lftypes.PermissionAlter}, nil, nil, nil, nil, nil, nil)
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("db"),
					Permissions: &Permissions{Alter: true},
				}},
			},
		}
		if err := readPermissions(ctx, mock, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if data.Catalog.Database[0].Permissions == nil || !data.Catalog.Database[0].Permissions.Alter {
			t.Error("expected database Permissions.Alter=true after refresh")
		}
	})

	t.Run("database_list_error_propagated", func(t *testing.T) {
		mock := listByResource(nil, nil, nil, nil, nil, fmt.Errorf("db list failed"), nil, nil)
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("db"),
					Permissions: &Permissions{Alter: true},
				}},
			},
		}
		if err := readPermissions(ctx, mock, data); err == nil {
			t.Error("expected error from database list failure")
		}
	})

	t.Run("table_permissions_refreshed_from_aws", func(t *testing.T) {
		mock := listByResource(nil, nil, []lftypes.Permission{lftypes.PermissionSelect}, nil, nil, nil, nil, nil)
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					Table: []TablePermModel{{
						Name:        types.StringValue("tbl"),
						Permissions: &Permissions{Select: true},
					}},
				}},
			},
		}
		if err := readPermissions(ctx, mock, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if data.Catalog.Database[0].Table[0].Permissions == nil || !data.Catalog.Database[0].Table[0].Permissions.Select {
			t.Error("expected table Permissions.Select=true after refresh")
		}
	})

	t.Run("table_list_error_propagated", func(t *testing.T) {
		mock := listByResource(nil, nil, nil, nil, nil, nil, fmt.Errorf("table list failed"), nil)
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					Table: []TablePermModel{{
						Name:        types.StringValue("tbl"),
						Permissions: &Permissions{Select: true},
					}},
				}},
			},
		}
		if err := readPermissions(ctx, mock, data); err == nil {
			t.Error("expected error from table list failure")
		}
	})

	t.Run("wildcard_nil_skips_wildcard_list", func(t *testing.T) {
		listCalled := false
		mock := &mockLFClient{
			listFn: func(_ int, input *lakeformation.ListPermissionsInput) ([]lftypes.PrincipalResourcePermissions, error) {
				if input.Resource.Table != nil && input.Resource.Table.TableWildcard != nil {
					listCalled = true
				}
				return nil, nil
			},
		}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:       types.StringValue(catalogID),
				Database: []DatabasePermModel{{Name: types.StringValue("db")}},
			},
		}
		if err := readPermissions(ctx, mock, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if listCalled {
			t.Error("expected no wildcard list call when Wildcard is nil")
		}
	})

	t.Run("wildcard_permissions_refreshed_and_iswildcard_set", func(t *testing.T) {
		mock := listByResource(nil, nil, nil, []lftypes.Permission{lftypes.PermissionSelect}, nil, nil, nil, nil)
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					Wildcard: &TablePermModel{
						Permissions: &Permissions{Select: true},
					},
				}},
			},
		}
		if err := readPermissions(ctx, mock, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		wc := data.Catalog.Database[0].Wildcard
		if !wc.IsWildcard {
			t.Error("expected IsWildcard=true after wildcard read")
		}
		if wc.Permissions == nil || !wc.Permissions.Select {
			t.Error("expected wildcard Permissions.Select=true after refresh")
		}
	})

	t.Run("wildcard_list_error_propagated", func(t *testing.T) {
		mock := listByResource(nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("wildcard list failed"))
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					Wildcard: &TablePermModel{
						Permissions: &Permissions{Select: true},
					},
				}},
			},
		}
		if err := readPermissions(ctx, mock, data); err == nil {
			t.Error("expected error from wildcard list failure")
		}
	})
}

// TestDelete exercises the delete path via deletePermissions.
func TestDelete(t *testing.T) {
	const principal = "arn:aws:iam::123456789012:role/TestRole"
	const catalogID = "123456789012"
	ctx := context.Background()

	del := func(mock *mockLFClient, state *LakeFormationPermissionsResourceModel) error {
		return deletePermissions(ctx, mock, state)
	}

	t.Run("catalog_permissions_revoked", func(t *testing.T) {
		mock := mockWithCurrentPerms([]lftypes.Permission{lftypes.PermissionDescribe, lftypes.PermissionDrop}, nil)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:          types.StringValue(catalogID),
				Permissions: &Permissions{Describe: true, Drop: true},
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
					{Name: types.StringValue("analytics"), Permissions: &Permissions{Describe: true}},
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
		mock := mockWithCurrentPerms([]lftypes.Permission{lftypes.PermissionSelect}, nil)
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
								Permissions: &Permissions{Select: true},
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
		mock := mockWithCurrentPerms([]lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionInsert}, nil)
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
								Permissions: &Permissions{Select: true, Insert: true},
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
		mock := mockWithCurrentPerms([]lftypes.Permission{lftypes.PermissionSelect}, nil)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{
					{
						Name:     types.StringValue("raw"),
						Wildcard: &TablePermModel{Permissions: &Permissions{Select: true}},
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
		mock := mockWithCurrentPerms(
			[]lftypes.Permission{lftypes.PermissionSelect},
			[]lftypes.Permission{lftypes.PermissionSelect},
		)
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
								Permissions:          &Permissions{Select: true},
								GrantablePermissions: &Permissions{Select: true},
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

	t.Run("nil_catalog_no_api_calls", func(t *testing.T) {
		mock := &mockLFClient{revokeErr: fmt.Errorf("should not be called")}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			// Catalog is nil
		}
		if err := del(mock, state); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.revokeCalls) != 0 {
			t.Errorf("expected no revoke calls; got %d", len(mock.revokeCalls))
		}
	})

	t.Run("multiple_databases_revoked_independently", func(t *testing.T) {
		mock := mockLFClientWithPerms()
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{
					{Name: types.StringValue("db1"), Permissions: &Permissions{Describe: true}},
					{Name: types.StringValue("db2"), Permissions: &Permissions{CreateTable: true}},
				},
			},
		}
		if err := del(mock, state); err != nil {
			t.Fatalf("delete error: %v", err)
		}
		if findRevokeCall(mock.revokeCalls, isDatabaseResource("db1")) == nil {
			t.Error("expected RevokePermissions call for db1")
		}
		if findRevokeCall(mock.revokeCalls, isDatabaseResource("db2")) == nil {
			t.Error("expected RevokePermissions call for db2")
		}
	})

	t.Run("all_true_in_state_revokes_all", func(t *testing.T) {
		// State holds all=true; resource currently reports ALL in AWS.
		// applyDiff sees ALL in curP → revoke ALL.
		mock := mockWithCurrentPerms([]lftypes.Permission{lftypes.PermissionAll}, nil)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:          types.StringValue(catalogID),
				Permissions: &Permissions{All: true},
			},
		}
		if err := del(mock, state); err != nil {
			t.Fatalf("delete error: %v", err)
		}
		call := findRevokeCall(mock.revokeCalls, isCatalogResource)
		if call == nil {
			t.Fatal("expected RevokePermissions call for catalog with all=true")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("permissions = %v, want [ALL]", call.Permissions)
		}
	})

	t.Run("wildcard_grantable_revokes_perm_and_grant_option", func(t *testing.T) {
		mock := mockWithCurrentPerms(
			[]lftypes.Permission{lftypes.PermissionSelect},
			[]lftypes.Permission{lftypes.PermissionSelect},
		)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("raw"),
					Wildcard: &TablePermModel{
						Permissions:          &Permissions{Select: true},
						GrantablePermissions: &Permissions{Select: true},
					},
				}},
			},
		}
		if err := del(mock, state); err != nil {
			t.Fatalf("delete error: %v", err)
		}
		call := findRevokeCall(mock.revokeCalls, isWildcardResource("raw"))
		if call == nil {
			t.Fatal("expected RevokePermissions call for wildcard")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("Permissions = %v, want [SELECT]", call.Permissions)
		}
		if !permsEqual(call.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("PermissionsWithGrantOption = %v, want [SELECT]", call.PermissionsWithGrantOption)
		}
	})

	t.Run("database_error_propagated", func(t *testing.T) {
		mock := mockWithCurrentPerms([]lftypes.Permission{lftypes.PermissionDescribe}, nil)
		mock.revokeErr = fmt.Errorf("access denied")
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("analytics"),
					Permissions: &Permissions{Describe: true},
				}},
			},
		}
		if err := del(mock, state); err == nil {
			t.Fatal("expected error from database revoke, got nil")
		}
	})

	t.Run("table_error_propagated", func(t *testing.T) {
		mock := mockWithCurrentPerms([]lftypes.Permission{lftypes.PermissionSelect}, nil)
		mock.revokeErr = fmt.Errorf("access denied")
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("analytics"),
					Table: []TablePermModel{{
						Name:        types.StringValue("events"),
						Permissions: &Permissions{Select: true},
					}},
				}},
			},
		}
		if err := del(mock, state); err == nil {
			t.Fatal("expected error from table revoke, got nil")
		}
	})

	t.Run("wildcard_error_propagated", func(t *testing.T) {
		mock := mockWithCurrentPerms([]lftypes.Permission{lftypes.PermissionSelect}, nil)
		mock.revokeErr = fmt.Errorf("access denied")
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:     types.StringValue("raw"),
					Wildcard: &TablePermModel{Permissions: &Permissions{Select: true}},
				}},
			},
		}
		if err := del(mock, state); err == nil {
			t.Fatal("expected error from wildcard revoke, got nil")
		}
	})

	t.Run("aws_has_subset_of_stated_permissions_revokes_only_subset", func(t *testing.T) {
		// State declares [SELECT, DESCRIBE] but AWS only holds [SELECT] (e.g. DESCRIBE was
		// externally removed). Only [SELECT] must appear in the revoke call.
		mock := mockWithCurrentPerms([]lftypes.Permission{lftypes.PermissionSelect}, nil)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("analytics"),
					Permissions: &Permissions{Select: true, Describe: true},
				}},
			},
		}
		if err := del(mock, state); err != nil {
			t.Fatalf("delete error: %v", err)
		}
		call := findRevokeCall(mock.revokeCalls, isDatabaseResource("analytics"))
		if call == nil {
			t.Fatal("expected RevokePermissions call")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("revoke permissions = %v, want [SELECT] only", call.Permissions)
		}
	})

	t.Run("all_in_state_but_aws_has_individual_perms", func(t *testing.T) {
		// State has all=true but AWS only holds [SELECT, DESCRIBE] (ALL was never applied or
		// was partially revoked externally). The revoke must send exactly what AWS has.
		mock := mockWithCurrentPerms(
			[]lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionDescribe},
			nil,
		)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("analytics"),
					Permissions: &Permissions{All: true},
				}},
			},
		}
		if err := del(mock, state); err != nil {
			t.Fatalf("delete error: %v", err)
		}
		call := findRevokeCall(mock.revokeCalls, isDatabaseResource("analytics"))
		if call == nil {
			t.Fatal("expected RevokePermissions call")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionDescribe}) {
			t.Errorf("revoke permissions = %v, want [SELECT, DESCRIBE]", call.Permissions)
		}
	})

	t.Run("aws_has_no_permissions_no_revoke", func(t *testing.T) {
		// AWS reports no permissions for the resource (already cleaned up externally).
		// No revoke call should be made.
		mock := mockWithCurrentPerms(nil, nil)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("analytics"),
					Permissions: &Permissions{Select: true},
				}},
			},
		}
		if err := del(mock, state); err != nil {
			t.Fatalf("delete error: %v", err)
		}
		if call := findRevokeCall(mock.revokeCalls, isDatabaseResource("analytics")); call != nil {
			t.Errorf("expected no revoke call when AWS has no permissions; got %+v", call)
		}
	})

	t.Run("aws_has_subset_of_grantable_revokes_only_that_subset", func(t *testing.T) {
		// State has Permissions=[SELECT, DESCRIBE] and GrantablePermissions=[SELECT, DESCRIBE].
		// AWS holds Permissions=[SELECT, DESCRIBE] but GrantablePermissions=[SELECT] only.
		// Revoke must send Permissions=[SELECT, DESCRIBE] with PermissionsWithGrantOption=[SELECT].
		mock := mockWithCurrentPerms(
			[]lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionDescribe},
			[]lftypes.Permission{lftypes.PermissionSelect},
		)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:                 types.StringValue("analytics"),
					Permissions:          &Permissions{Select: true, Describe: true},
					GrantablePermissions: &Permissions{Select: true, Describe: true},
				}},
			},
		}
		if err := del(mock, state); err != nil {
			t.Fatalf("delete error: %v", err)
		}
		call := findRevokeCall(mock.revokeCalls, isDatabaseResource("analytics"))
		if call == nil {
			t.Fatal("expected RevokePermissions call")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionDescribe}) {
			t.Errorf("revoke permissions = %v, want [SELECT, DESCRIBE]", call.Permissions)
		}
		if !permsEqual(call.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("revoke grantable = %v, want [SELECT] only", call.PermissionsWithGrantOption)
		}
	})

	t.Run("aws_has_no_grantable_revoke_omits_grant_option", func(t *testing.T) {
		// State has Permissions=[SELECT] and GrantablePermissions=[SELECT].
		// AWS holds Permissions=[SELECT] but GrantablePermissions=[] (grant option externally revoked).
		// Revoke must send Permissions=[SELECT] with no PermissionsWithGrantOption.
		mock := mockWithCurrentPerms(
			[]lftypes.Permission{lftypes.PermissionSelect},
			nil,
		)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:                 types.StringValue("analytics"),
					Permissions:          &Permissions{Select: true},
					GrantablePermissions: &Permissions{Select: true},
				}},
			},
		}
		if err := del(mock, state); err != nil {
			t.Fatalf("delete error: %v", err)
		}
		call := findRevokeCall(mock.revokeCalls, isDatabaseResource("analytics"))
		if call == nil {
			t.Fatal("expected RevokePermissions call")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("revoke permissions = %v, want [SELECT]", call.Permissions)
		}
		if len(call.PermissionsWithGrantOption) != 0 {
			t.Errorf("revoke grantable = %v, want empty", call.PermissionsWithGrantOption)
		}
	})
}


// ── ValidateResource ─────────────────────────────────────────────────────────

func TestValidateResource(t *testing.T) {
	ctx := context.Background()
	const principal = "arn:aws:iam::123456789012:role/TestRole"
	const catalogID = "123456789012"

	r := &LakeFormationPermissionsResource{}

	// buildConfig uses the Plan encoding path (same binary format) so we can
	// construct a tfsdk.Config without a Set method.
	buildConfig := func(model *LakeFormationPermissionsResourceModel) tfsdk.Config {
		var schemaResp resource.SchemaResponse
		r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
		planType := schemaResp.Schema.Type().TerraformType(ctx)
		p := tfsdk.Plan{
			Schema: schemaResp.Schema,
			Raw:    tftypes.NewValue(planType, nil),
		}
		if diags := p.Set(ctx, model); diags.HasError() {
			panic(fmt.Sprintf("buildConfig: %v", diags))
		}
		return tfsdk.Config{Schema: schemaResp.Schema, Raw: p.Raw}
	}

	validate := func(model *LakeFormationPermissionsResourceModel) diag.Diagnostics {
		v := &lfPermissionsValidator{}
		resp := &resource.ValidateConfigResponse{}
		v.ValidateResource(ctx, resource.ValidateConfigRequest{Config: buildConfig(model)}, resp)
		return resp.Diagnostics
	}

	t.Run("catalog_database_both_nil_no_error", func(t *testing.T) {
		// Catalog and database with no permissions at all is allowed.
		diags := validate(&LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("analytics"),
					// Permissions and GrantablePermissions both nil — allowed at catalog/db level.
				}},
			},
		})
		if diags.HasError() {
			t.Errorf("unexpected error for catalog/database with nil permissions: %v", diags)
		}
	})

	t.Run("table_both_nil_error", func(t *testing.T) {
		diags := validate(&LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("analytics"),
					Table: []TablePermModel{{
						Name: types.StringValue("events"),
						// Permissions and GrantablePermissions both nil — invalid for tables.
					}},
				}},
			},
		})
		if !diags.HasError() {
			t.Error("expected error when table has both permissions nil, got none")
		}
	})

	t.Run("table_permissions_only_ok", func(t *testing.T) {
		diags := validate(&LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("analytics"),
					Table: []TablePermModel{{
						Name:        types.StringValue("events"),
						Permissions: &Permissions{Select: true},
					}},
				}},
			},
		})
		if diags.HasError() {
			t.Errorf("unexpected error when table has only permissions set: %v", diags)
		}
	})

	t.Run("table_grantable_only_ok", func(t *testing.T) {
		diags := validate(&LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("analytics"),
					Table: []TablePermModel{{
						Name:                 types.StringValue("events"),
						GrantablePermissions: &Permissions{Select: true},
					}},
				}},
			},
		})
		if diags.HasError() {
			t.Errorf("unexpected error when table has only grantable_permissions set: %v", diags)
		}
	})

	t.Run("wildcard_both_nil_error", func(t *testing.T) {
		diags := validate(&LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:     types.StringValue("analytics"),
					Wildcard: &TablePermModel{},
					// Permissions and GrantablePermissions both nil — invalid for wildcards.
				}},
			},
		})
		if !diags.HasError() {
			t.Error("expected error when wildcard has both permissions nil, got none")
		}
	})

	t.Run("wildcard_permissions_only_ok", func(t *testing.T) {
		diags := validate(&LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:     types.StringValue("analytics"),
					Wildcard: &TablePermModel{Permissions: &Permissions{Select: true}},
				}},
			},
		})
		if diags.HasError() {
			t.Errorf("unexpected error when wildcard has only permissions set: %v", diags)
		}
	})

	t.Run("wildcard_grantable_only_ok", func(t *testing.T) {
		diags := validate(&LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:     types.StringValue("analytics"),
					Wildcard: &TablePermModel{GrantablePermissions: &Permissions{Select: true}},
				}},
			},
		})
		if diags.HasError() {
			t.Errorf("unexpected error when wildcard has only grantable_permissions set: %v", diags)
		}
	})
}

// ── checkPerms ───────────────────────────────────────────────────────────────

func TestCheckPerms(t *testing.T) {
	check := func(p *Permissions, rt LFResourceType) bool {
		var diags diag.Diagnostics
		checkPerms(p, rt, path.Root("permissions"), &diags)
		return diags.HasError()
	}

	t.Run("nil_no_error", func(t *testing.T) {
		if check((*Permissions)(nil), LFResourceTypeCatalog) {
			t.Error("nil pointer: expected no error")
		}
	})

	t.Run("empty_struct_no_error", func(t *testing.T) {
		if check(&Permissions{}, LFResourceTypeCatalog) {
			t.Error("empty struct: expected no error")
		}
	})

	t.Run("all_true_no_error", func(t *testing.T) {
		// Explicitly setting all = true is always valid.
		if check(&Permissions{All: true}, LFResourceTypeCatalog) {
			t.Error("all=true: expected no error")
		}
	})

	t.Run("partial_catalog_no_error", func(t *testing.T) {
		p := &Permissions{Alter: true, Describe: true}
		if check(p, LFResourceTypeCatalog) {
			t.Error("partial catalog subset: expected no error")
		}
	})

	t.Run("all_individual_catalog_error", func(t *testing.T) {
		p := &Permissions{
			Alter:          true,
			CreateCatalog:  true,
			CreateDatabase: true,
			Describe:       true,
			Drop:           true,
		}
		if !check(p, LFResourceTypeCatalog) {
			t.Error("all individual catalog perms set: expected error")
		}
	})

	t.Run("partial_database_no_error", func(t *testing.T) {
		p := &Permissions{CreateTable: true, Describe: true}
		if check(p, LFResourceTypeDatabase) {
			t.Error("partial database subset: expected no error")
		}
	})

	t.Run("all_individual_database_error", func(t *testing.T) {
		p := &Permissions{
			Alter:       true,
			CreateTable: true,
			Describe:    true,
			Drop:        true,
		}
		if !check(p, LFResourceTypeDatabase) {
			t.Error("all individual database perms set: expected error")
		}
	})

	t.Run("partial_table_no_error", func(t *testing.T) {
		p := &Permissions{Select: true, Describe: true}
		if check(p, LFResourceTypeTable) {
			t.Error("partial table subset: expected no error")
		}
	})

	t.Run("all_individual_table_error", func(t *testing.T) {
		p := &Permissions{
			Alter:    true,
			Delete:   true,
			Describe: true,
			Drop:     true,
			Insert:   true,
			Select:   true,
		}
		if !check(p, LFResourceTypeTable) {
			t.Error("all individual table perms set: expected error")
		}
	})

	t.Run("one_below_full_table_no_error", func(t *testing.T) {
		// All but one field set — strict subset, so no error.
		p := &Permissions{
			Alter:    true,
			Delete:   true,
			Describe: true,
			Drop:     true,
			Insert:   true,
			// Select omitted
		}
		if check(p, LFResourceTypeTable) {
			t.Error("five of six table perms set: expected no error")
		}
	})

	t.Run("invalid_perm_for_resource_type_error", func(t *testing.T) {
		// SELECT is not valid for catalog resources.
		p := &Permissions{Select: true}
		if !check(p, LFResourceTypeCatalog) {
			t.Error("select on catalog: expected error for invalid permission")
		}
	})

	t.Run("invalid_perm_for_database_error", func(t *testing.T) {
		// SELECT is not valid for database resources.
		p := &Permissions{Select: true}
		if !check(p, LFResourceTypeDatabase) {
			t.Error("select on database: expected error for invalid permission")
		}
	})

	t.Run("invalid_perm_for_table_error", func(t *testing.T) {
		// CREATE_CATALOG is not valid for table resources.
		p := &Permissions{CreateCatalog: true}
		if !check(p, LFResourceTypeTable) {
			t.Error("create_catalog on table: expected error for invalid permission")
		}
	})

	t.Run("all_true_with_other_flag_error", func(t *testing.T) {
		// all=true combined with any individual flag is invalid.
		p := &Permissions{All: true, Describe: true}
		if !check(p, LFResourceTypeCatalog) {
			t.Error("all=true with describe=true: expected conflicting attributes error")
		}
	})
}

// ── checkSupersetPerms ───────────────────────────────────────────────────────

func TestCheckSupersetPerms(t *testing.T) {
	check := func(perms, grantPerms *Permissions) bool {
		var diags diag.Diagnostics
		checkSupersetPerms(perms, grantPerms, path.Root("catalog"), &diags)
		return diags.HasError()
	}

	t.Run("nil_perms_no_error", func(t *testing.T) {
		// nil permissions will be computed — skip superset check.
		if check((*Permissions)(nil), &Permissions{Select: true}) {
			t.Error("nil permissions: expected no error")
		}
	})

	t.Run("nil_grantable_no_error", func(t *testing.T) {
		if check(&Permissions{Select: true}, (*Permissions)(nil)) {
			t.Error("nil grantable_permissions: expected no error")
		}
	})

	t.Run("equal_no_error", func(t *testing.T) {
		p := &Permissions{Select: true}
		g := &Permissions{Select: true}
		if check(p, g) {
			t.Error("equal permissions: expected no error")
		}
	})

	t.Run("superset_no_error", func(t *testing.T) {
		p := &Permissions{Select: true, Insert: true}
		g := &Permissions{Select: true}
		if check(p, g) {
			t.Error("proper superset: expected no error")
		}
	})

	t.Run("permissions_all_no_error", func(t *testing.T) {
		// permissions.All=true is a superset of any grantable_permissions.
		p := &Permissions{All: true}
		g := &Permissions{Select: true, Insert: true}
		if check(p, g) {
			t.Error("permissions.All=true: expected no error for any grantable_permissions")
		}
	})

	t.Run("table_missing_permission_error", func(t *testing.T) {
		// grantable has SELECT+INSERT; permissions only has SELECT → INSERT not covered.
		p := &Permissions{Select: true}
		g := &Permissions{Select: true, Insert: true}
		if !check(p, g) {
			t.Error("insert in grantable but not in permissions: expected error")
		}
	})

	t.Run("catalog_missing_permission_error", func(t *testing.T) {
		// grantable has DESCRIBE+ALTER; permissions only has DESCRIBE → ALTER not covered.
		p := &Permissions{Describe: true}
		g := &Permissions{Describe: true, Alter: true}
		if !check(p, g) {
			t.Error("alter in catalog grantable but not in permissions: expected error")
		}
	})

	t.Run("database_missing_permission_error", func(t *testing.T) {
		// grantable has DESCRIBE+CREATE_TABLE; permissions only has DESCRIBE → CREATE_TABLE not covered.
		p := &Permissions{Describe: true}
		g := &Permissions{Describe: true, CreateTable: true}
		if !check(p, g) {
			t.Error("create_table in database grantable but not in permissions: expected error")
		}
	})

	t.Run("grantable_all_without_perms_all_error", func(t *testing.T) {
		p := &Permissions{Describe: true}
		g := &Permissions{All: true}
		if !check(p, g) {
			t.Error("grantable ALL but permissions lacks ALL: expected error")
		}
	})

	t.Run("both_all_no_error", func(t *testing.T) {
		p := &Permissions{All: true}
		g := &Permissions{All: true}
		if check(p, g) {
			t.Error("both ALL: expected no error")
		}
	})

	t.Run("grantable_all_false_no_error", func(t *testing.T) {
		// grantPerms non-nil but all fields false → permsToAPI returns nil → len(g)==0 → no error.
		p := &Permissions{Describe: true}
		g := &Permissions{}
		if check(p, g) {
			t.Error("grantable all-false: expected no error")
		}
	})

	t.Run("perms_all_false_with_grantable_set_error", func(t *testing.T) {
		// perms non-nil but all fields false → p is empty; grantable has a real perm → not a subset → error.
		p := &Permissions{}
		g := &Permissions{Describe: true}
		if !check(p, g) {
			t.Error("perms all-false with grantable set: expected error")
		}
	})
}

// TestSupersetValidationAllLevels exercises checkSupersetPerms at every level that
// ValidateResource covers: catalog, database, table, and wildcard.
func TestSupersetValidationAllLevels(t *testing.T) {
	errFor := func(perms, grantPerms *Permissions, parentPath path.Path) diag.Diagnostics {
		var diags diag.Diagnostics
		checkSupersetPerms(perms, grantPerms, parentPath, &diags)
		return diags
	}

	catPath := path.Root("catalog")
	dbPath := catPath.AtName("database").AtListIndex(0)
	tblPath := dbPath.AtName("table").AtListIndex(0)
	wcPath := dbPath.AtName("wildcard")

	tests := []struct {
		name       string
		perms      *Permissions
		grantPerms *Permissions
		parentPath path.Path
		wantErr    bool
	}{
		// Both nil — skipped regardless of level
		{
			name:       "both_nil_no_error",
			perms:      nil,
			grantPerms: nil,
			parentPath: catPath,
			wantErr:    false,
		},
		{
			name:       "perms_nil_grantable_set_no_error",
			perms:      nil,
			grantPerms: &Permissions{Describe: true},
			parentPath: catPath,
			wantErr:    false,
		},
		// Catalog level
		{
			name:       "catalog_no_error_when_equal",
			perms:      &Permissions{Describe: true},
			grantPerms: &Permissions{Describe: true},
			parentPath: catPath,
			wantErr:    false,
		},
		{
			name:       "catalog_error_when_grantable_not_subset",
			perms:      &Permissions{Describe: true},
			grantPerms: &Permissions{Describe: true, CreateDatabase: true},
			parentPath: catPath,
			wantErr:    true,
		},
		{
			name:       "catalog_no_error_when_perms_all",
			perms:      &Permissions{All: true},
			grantPerms: &Permissions{Describe: true, CreateDatabase: true},
			parentPath: catPath,
			wantErr:    false,
		},
		// Database level
		{
			name:       "database_no_error_when_superset",
			perms:      &Permissions{Describe: true, CreateTable: true},
			grantPerms: &Permissions{Describe: true},
			parentPath: dbPath,
			wantErr:    false,
		},
		{
			name:       "database_error_when_grantable_not_subset",
			perms:      &Permissions{Describe: true},
			grantPerms: &Permissions{Describe: true, Alter: true},
			parentPath: dbPath,
			wantErr:    true,
		},
		{
			name:       "database_no_error_when_perms_all",
			perms:      &Permissions{All: true},
			grantPerms: &Permissions{Describe: true, Alter: true},
			parentPath: dbPath,
			wantErr:    false,
		},
		// Table level
		{
			name:       "table_no_error_when_superset",
			perms:      &Permissions{Select: true, Insert: true},
			grantPerms: &Permissions{Select: true},
			parentPath: tblPath,
			wantErr:    false,
		},
		{
			name:       "table_error_when_grantable_not_subset",
			perms:      &Permissions{Select: true},
			grantPerms: &Permissions{Select: true, Describe: true},
			parentPath: tblPath,
			wantErr:    true,
		},
		{
			name:       "table_no_error_when_perms_all",
			perms:      &Permissions{All: true},
			grantPerms: &Permissions{Select: true, Describe: true},
			parentPath: tblPath,
			wantErr:    false,
		},
		// Wildcard (same TablePermissions type as table)
		{
			name:       "wildcard_error_when_grantable_not_subset",
			perms:      &Permissions{Select: true},
			grantPerms: &Permissions{Select: true, Insert: true},
			parentPath: wcPath,
			wantErr:    true,
		},
		{
			name:       "wildcard_no_error_when_equal",
			perms:      &Permissions{Select: true},
			grantPerms: &Permissions{Select: true},
			parentPath: wcPath,
			wantErr:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			diags := errFor(tc.perms, tc.grantPerms, tc.parentPath)
			if tc.wantErr && !diags.HasError() {
				t.Error("expected error but got none")
			}
			if !tc.wantErr && diags.HasError() {
				t.Errorf("unexpected error: %s", diags[0].Detail())
			}
		})
	}
}

// ── fillPermPair ─────────────────────────────────────────────────────────────

func TestFillPermPair(t *testing.T) {
	t.Run("both_nil_returns_false", func(t *testing.T) {
		var p, g *Permissions
		if fillPermPair(&p, &g) {
			t.Error("expected false when both nil")
		}
		if p != nil || g != nil {
			t.Error("expected both to remain nil")
		}
	})
	t.Run("perms_set_grantPerms_nil_fills_empty_grantPerms", func(t *testing.T) {
		p := &Permissions{Describe: true}
		var g *Permissions
		if !fillPermPair(&p, &g) {
			t.Error("expected true")
		}
		if g == nil {
			t.Fatal("expected grantPerms to be set to empty struct")
		}
		if *g != (Permissions{}) {
			t.Errorf("expected empty grantPerms, got %+v", g)
		}
		if !p.Describe {
			t.Error("perms must not be modified")
		}
	})
	t.Run("grantPerms_set_perms_nil_copies_grantPerms_to_perms", func(t *testing.T) {
		var p *Permissions
		g := &Permissions{Select: true}
		if !fillPermPair(&p, &g) {
			t.Error("expected true")
		}
		if p == nil {
			t.Fatal("expected perms to be set")
		}
		if !p.Select {
			t.Errorf("expected perms to be a copy of grantPerms, got %+v", p)
		}
		if p == g {
			t.Error("expected a copy, not the same pointer")
		}
	})
	t.Run("both_set_returns_false_unchanged", func(t *testing.T) {
		p := &Permissions{Describe: true}
		g := &Permissions{Select: true}
		if fillPermPair(&p, &g) {
			t.Error("expected false when both non-nil")
		}
		if !p.Describe || !g.Select {
			t.Error("values must not be modified")
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
				GrantablePermissions: &Permissions{Describe: true},
			},
		}
		changed := defaultPermissions(plan)
		if !changed {
			t.Fatal("expected changed=true")
		}
		if plan.Catalog.Permissions == nil {
			t.Fatal("expected Permissions to be set")
		}
		if !plan.Catalog.Permissions.Describe {
			t.Error("expected Permissions.Describe=true")
		}
	})

	t.Run("catalog_both_set_unchanged", func(t *testing.T) {
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:                   types.StringValue(catalogID),
				Permissions:          &Permissions{Describe: true, Alter: true},
				GrantablePermissions: &Permissions{Describe: true},
			},
		}
		defaultPermissions(plan)
		if !plan.Catalog.Permissions.Alter {
			t.Error("Alter should still be true — user-set permissions must not be overwritten")
		}
	})

	t.Run("database_grantable_only_sets_permissions", func(t *testing.T) {
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:                 types.StringValue("db"),
					GrantablePermissions: &Permissions{Describe: true},
				}},
			},
		}
		changed := defaultPermissions(plan)
		db := plan.Catalog.Database[0]
		if db.Permissions == nil {
			t.Fatal("expected database Permissions to be set")
		}
		if !db.Permissions.Describe {
			t.Error("expected database Permissions.Describe=true")
		}
		if !changed {
			t.Error("expected changed=true")
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
						GrantablePermissions: &Permissions{Select: true},
					}},
				}},
			},
		}
		defaultPermissions(plan)
		tbl := plan.Catalog.Database[0].Table[0]
		if tbl.Permissions == nil {
			t.Fatal("expected table Permissions to be set")
		}
		if !tbl.Permissions.Select {
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
						GrantablePermissions: &Permissions{Select: true},
					},
				}},
			},
		}
		defaultPermissions(plan)
		wc := plan.Catalog.Database[0].Wildcard
		if wc.Permissions == nil {
			t.Fatal("expected wildcard Permissions to be set")
		}
		if !wc.Permissions.Select {
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

	// Database-level analogue of the catalog tests above.

	t.Run("database_neither_permissions_nor_grantable_no_change", func(t *testing.T) {
		// Simulates: database { name = "db" } with no permissions block at all.
		// UseStateForUnknown may have loaded nil from state — plan.db.Permissions stays nil.
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
				}},
			},
		}
		changed := defaultPermissions(plan)
		db := plan.Catalog.Database[0]
		if db.Permissions != nil {
			t.Error("expected db.Permissions to remain nil when neither block is specified")
		}
		if changed {
			t.Error("expected changed=false when nothing to fill")
		}
	})

	t.Run("database_both_omitted_no_change", func(t *testing.T) {
		// Both permissions and grantable_permissions omitted from config → both nil after
		// resolveUnknownToNull. fillPermPair must leave them nil so apply skips the resource.
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					// Permissions and GrantablePermissions both nil (both omitted in config).
				}},
			},
		}
		changed := defaultPermissions(plan)
		if changed {
			t.Error("expected changed=false when both permissions blocks are nil")
		}
		if plan.Catalog.Database[0].Permissions != nil {
			t.Error("Permissions must remain nil — nothing to compute from")
		}
		if plan.Catalog.Database[0].GrantablePermissions != nil {
			t.Error("GrantablePermissions must remain nil — nothing to compute from")
		}
	})

	t.Run("catalog_and_database_state_preserved_with_table_permissions", func(t *testing.T) {
		// The scenario the user reported: config has permissions at the table level only.
		// Catalog and database blocks exist in config but have no permissions or grantable_permissions.
		// State has both catalog and database permissions. They must remain in the plan unchanged
		// so that updatePermissions sees plan==state and makes no API calls at those levels.
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:          types.StringValue(catalogID),
				Permissions: &Permissions{Describe: true}, // state-loaded
				Database: []DatabasePermModel{{
					Name:        types.StringValue("db"),
					Permissions: &Permissions{Alter: true}, // state-loaded
					Table: []TablePermModel{{
						Name:        types.StringValue("tbl"),
						Permissions: &Permissions{Select: true}, // user-set
					}},
				}},
			},
		}
		changed := defaultPermissions(plan)
		if plan.Catalog.Permissions == nil || !plan.Catalog.Permissions.Describe {
			t.Error("catalog.permissions state value must be preserved when omitted from config")
		}
		db := plan.Catalog.Database[0]
		if db.Permissions == nil || !db.Permissions.Alter {
			t.Error("database.permissions state value must be preserved when omitted from config")
		}
		tbl := db.Table[0]
		if tbl.Permissions == nil || !tbl.Permissions.Select {
			t.Error("table.permissions must retain the user-set value")
		}
		if tbl.GrantablePermissions == nil {
			t.Error("table.grantable_permissions must be auto-filled to empty when only permissions is set")
		}
		if !changed {
			t.Error("expected changed=true — table grantable_permissions was auto-filled to empty")
		}
	})

	t.Run("database_grantable_only_fills_permissions_from_grantable", func(t *testing.T) {
		// User sets grantable_permissions only; permissions omitted → nil after resolveUnknownToNull.
		// fillPermPair must set permissions = copy of grantable_permissions.
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:                 types.StringValue("db"),
					GrantablePermissions: &Permissions{Describe: true, Alter: true},
				}},
			},
		}
		changed := defaultPermissions(plan)
		db := plan.Catalog.Database[0]
		if db.Permissions == nil {
			t.Fatal("expected db.Permissions to be set from grantable_permissions")
		}
		if !db.Permissions.Alter {
			t.Error("expected db.Permissions.Alter=true (copied from grantable)")
		}
		if !db.Permissions.Describe {
			t.Error("expected db.Permissions.Describe=true (copied from grantable)")
		}
		if !changed {
			t.Error("expected changed=true")
		}
	})

	t.Run("permissions_only_sets_grantable_to_empty_catalog", func(t *testing.T) {
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:          types.StringValue(catalogID),
				Permissions: &Permissions{Describe: true},
			},
		}
		changed := defaultPermissions(plan)
		if !changed {
			t.Fatal("expected changed=true")
		}
		if plan.Catalog.GrantablePermissions == nil {
			t.Fatal("expected GrantablePermissions to be auto-filled to empty struct")
		}
		if permsToAPI(plan.Catalog.GrantablePermissions) != nil {
			t.Error("expected empty grantable_permissions to produce no API permissions")
		}
	})

	t.Run("permissions_only_sets_grantable_to_empty_database", func(t *testing.T) {
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("db"),
					Permissions: &Permissions{Describe: true},
				}},
			},
		}
		changed := defaultPermissions(plan)
		if !changed {
			t.Fatal("expected changed=true")
		}
		db := plan.Catalog.Database[0]
		if db.GrantablePermissions == nil {
			t.Fatal("expected database GrantablePermissions to be auto-filled to empty")
		}
		if permsToAPI(db.GrantablePermissions) != nil {
			t.Error("expected empty grantable_permissions to produce no API permissions")
		}
	})

	t.Run("permissions_only_fills_grantable_to_empty", func(t *testing.T) {
		// User sets permissions only; grantable_permissions omitted → nil after resolveUnknownToNull.
		// fillPermPair must set grantable_permissions to an empty struct (no grantable perms).
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("db"),
					Permissions: &Permissions{Describe: true},
					// GrantablePermissions nil — omitted in config.
				}},
			},
		}
		changed := defaultPermissions(plan)
		if !changed {
			t.Fatal("expected changed=true: grantable_permissions auto-filled")
		}
		db := plan.Catalog.Database[0]
		if db.GrantablePermissions == nil {
			t.Fatal("expected GrantablePermissions to be auto-filled to empty struct")
		}
		if permsToAPI(db.GrantablePermissions) != nil {
			t.Errorf("expected empty grantable_permissions, got %v", permsToAPI(db.GrantablePermissions))
		}
	})

	t.Run("permissions_only_sets_grantable_to_empty_table", func(t *testing.T) {
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					Table: []TablePermModel{{
						Name:        types.StringValue("tbl"),
						Permissions: &Permissions{Select: true},
					}},
				}},
			},
		}
		changed := defaultPermissions(plan)
		if !changed {
			t.Fatal("expected changed=true")
		}
		tbl := plan.Catalog.Database[0].Table[0]
		if tbl.GrantablePermissions == nil {
			t.Fatal("expected table GrantablePermissions to be auto-filled to empty")
		}
	})

	t.Run("permissions_only_sets_grantable_to_empty_wildcard", func(t *testing.T) {
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					Wildcard: &TablePermModel{
						Permissions: &Permissions{Select: true},
					},
				}},
			},
		}
		changed := defaultPermissions(plan)
		if !changed {
			t.Fatal("expected changed=true")
		}
		wc := plan.Catalog.Database[0].Wildcard
		if wc.GrantablePermissions == nil {
			t.Fatal("expected wildcard GrantablePermissions to be auto-filled to empty")
		}
	})

	t.Run("multiple_databases_each_filled_independently", func(t *testing.T) {
		// Each database is processed independently: grantable-only, perms-only, and both-nil.
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{
					{
						Name:                 types.StringValue("db_grantable_only"),
						GrantablePermissions: &Permissions{Describe: true},
					},
					{
						Name:        types.StringValue("db_perms_only"),
						Permissions: &Permissions{Alter: true},
					},
					{
						Name: types.StringValue("db_both_nil"),
						// both nil — no fill expected
					},
				},
			},
		}
		changed := defaultPermissions(plan)
		if !changed {
			t.Fatal("expected changed=true")
		}

		db0 := plan.Catalog.Database[0]
		if db0.Permissions == nil || !db0.Permissions.Describe {
			t.Error("db_grantable_only: expected Permissions filled from GrantablePermissions")
		}

		db1 := plan.Catalog.Database[1]
		if db1.GrantablePermissions == nil {
			t.Error("db_perms_only: expected GrantablePermissions auto-filled to empty struct")
		}
		if permsToAPI(db1.GrantablePermissions) != nil {
			t.Error("db_perms_only: expected empty GrantablePermissions")
		}

		db2 := plan.Catalog.Database[2]
		if db2.Permissions != nil || db2.GrantablePermissions != nil {
			t.Error("db_both_nil: expected both to remain nil")
		}
	})

	t.Run("changed_true_from_nested_level_only", func(t *testing.T) {
		// Catalog level has both set (no change); only a table triggers the fill.
		// Verifies changed propagates up from a nested call.
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:                   types.StringValue(catalogID),
				Permissions:          &Permissions{Describe: true},
				GrantablePermissions: &Permissions{Describe: true},
				Database: []DatabasePermModel{{
					Name:                 types.StringValue("db"),
					Permissions:          &Permissions{CreateTable: true},
					GrantablePermissions: &Permissions{CreateTable: true},
					Table: []TablePermModel{{
						Name:        types.StringValue("tbl"),
						Permissions: &Permissions{Select: true},
						// GrantablePermissions nil — should be auto-filled
					}},
				}},
			},
		}
		changed := defaultPermissions(plan)
		if !changed {
			t.Fatal("expected changed=true from table-level fill even though catalog and db were unchanged")
		}
		tbl := plan.Catalog.Database[0].Table[0]
		if tbl.GrantablePermissions == nil {
			t.Error("expected table GrantablePermissions auto-filled to empty struct")
		}
		if permsToAPI(tbl.GrantablePermissions) != nil {
			t.Error("expected empty table GrantablePermissions")
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
						Permissions: &Permissions{Select: true},
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
		mock := mockWithCurrentPerms(
			[]lftypes.Permission{lftypes.PermissionSelect, lftypes.PermissionDescribe},
			nil,
		)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					Table: []TablePermModel{{
						Name: types.StringValue("tbl"),
						Permissions: &Permissions{
							Select:   true,
							Describe: true,
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
						Permissions: &Permissions{Select: true},
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
		mock := mockWithCurrentPerms([]lftypes.Permission{lftypes.PermissionSelect}, nil)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					Table: []TablePermModel{{
						Name:        types.StringValue("tbl"),
						Permissions: &Permissions{Select: true},
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
						Permissions: &Permissions{
							Select: true,
							Insert: true,
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
					Permissions: &Permissions{Describe: true},
				}},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("db"),
					Permissions: &Permissions{All: true},
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
		mock := mockWithCurrentPerms([]lftypes.Permission{lftypes.PermissionAll}, nil)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("db"),
					Permissions: &Permissions{All: true},
				}},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("db"),
					Permissions: &Permissions{Describe: true},
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
						Permissions: &Permissions{Select: true},
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
						Permissions:          &Permissions{Select: true},
						GrantablePermissions: &Permissions{Select: true},
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
		mock := mockWithCurrentPerms(
			[]lftypes.Permission{lftypes.PermissionSelect},
			[]lftypes.Permission{lftypes.PermissionSelect},
		)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					Table: []TablePermModel{{
						Name:                 types.StringValue("tbl"),
						Permissions:          &Permissions{Select: true},
						GrantablePermissions: &Permissions{Select: true},
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
						Permissions:          &Permissions{Select: true},
						GrantablePermissions: &Permissions{}, // explicit empty = remove grant option
					}},
				}},
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// AWS requires Permissions to be non-empty in RevokePermissions, so we revoke+regrant:
		// revoke(Permissions=[SELECT], PermissionsWithGrantOption=[SELECT]) then grant(Permissions=[SELECT]).
		rc := findRevokeCall(mock.revokeCalls, isTableResource("db", "tbl"))
		if rc == nil {
			t.Fatal("expected RevokePermissions call to remove grant option")
		}
		if !permsEqual(rc.Permissions, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("Permissions in revoke = %v, want [SELECT]", rc.Permissions)
		}
		if !permsEqual(rc.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("PermissionsWithGrantOption = %v, want [SELECT]", rc.PermissionsWithGrantOption)
		}
		// Regrant of the base permission to restore it after the revoke.
		gc := findGrantCall(mock.grantCalls, isTableResource("db", "tbl"))
		if gc == nil {
			t.Fatal("expected GrantPermissions call to regrant base SELECT after revoke+regrant")
		}
		if !permsEqual(gc.Permissions, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("regrant Permissions = %v, want [SELECT]", gc.Permissions)
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
					Permissions: &Permissions{Describe: true},
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

	t.Run("catalog_and_database_permissions_unchanged_when_only_table_specified", func(t *testing.T) {
		// Simulates the full update path for the scenario where:
		//   - Config has permissions only at the table level (not catalog or database).
		//   - State has DESCRIBE on the catalog and ALTER on the database.
		//   - ModifyPlan (UseStateForUnknown) preserved those state values in the plan.
		//   - Plan therefore has catalog.permissions = {DESCRIBE} and database.permissions = {ALTER},
		//     both equal to state — so updatePermissions must make no API calls at those levels.
		//   - Table permissions changed: state had SELECT, plan now adds INSERT.
		// Only the table changed (SELECT→SELECT+INSERT); catalog and database are unchanged.
		// Mock returns SELECT as the current table state so applyDiff only grants INSERT.
		mock := mockWithCurrentPerms([]lftypes.Permission{lftypes.PermissionSelect}, nil)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID:          types.StringValue(catalogID),
				Permissions: &Permissions{Describe: true},
				Database: []DatabasePermModel{{
					Name:        types.StringValue("db"),
					Permissions: &Permissions{Alter: true},
					Table: []TablePermModel{{
						Name:        types.StringValue("tbl"),
						Permissions: &Permissions{Select: true},
					}},
				}},
			},
		}
		// Plan mirrors state at catalog/database (state-loaded via UseStateForUnknown);
		// table gains INSERT.
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID:          types.StringValue(catalogID),
				Permissions: &Permissions{Describe: true}, // unchanged from state
				Database: []DatabasePermModel{{
					Name:        types.StringValue("db"),
					Permissions: &Permissions{Alter: true}, // unchanged from state
					Table: []TablePermModel{{
						Name: types.StringValue("tbl"),
						Permissions: &Permissions{
							Select: true,
							Insert: true, // new
						},
					}},
				}},
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Catalog: no revoke or grant.
		for _, rc := range mock.revokeCalls {
			if rc.Resource.Catalog != nil {
				t.Errorf("unexpected revoke on catalog: %+v", rc)
			}
		}
		for _, gc := range mock.grantCalls {
			if gc.Resource.Catalog != nil {
				t.Errorf("unexpected grant on catalog: %+v", gc)
			}
		}
		// Database: no revoke or grant.
		for _, rc := range mock.revokeCalls {
			if rc.Resource.Database != nil {
				t.Errorf("unexpected revoke on database: %+v", rc)
			}
		}
		for _, gc := range mock.grantCalls {
			if gc.Resource.Database != nil {
				t.Errorf("unexpected grant on database: %+v", gc)
			}
		}
		// Table: INSERT granted, SELECT untouched (no grant call for it).
		gc := findGrantCall(mock.grantCalls, isTableResource("db", "tbl"))
		if gc == nil {
			t.Fatal("expected grant call for table to add INSERT")
		}
		if !permsEqual(gc.Permissions, []lftypes.Permission{lftypes.PermissionInsert}) {
			t.Errorf("table granted = %v, want [INSERT]", gc.Permissions)
		}
		// No revokes on the table (SELECT remains).
		if rc := findRevokeCall(mock.revokeCalls, isTableResource("db", "tbl")); rc != nil {
			t.Errorf("unexpected table revoke: %+v", rc)
		}
	})

	t.Run("removed_database_not_revoked", func(t *testing.T) {
		// State has a database; plan does not → skip (nil plan = leave unchanged).
		// Revocation is the user's responsibility via a separate Terraform resource removal.
		mock := mockLFClientWithPerms()
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("old_db"),
					Permissions: &Permissions{Describe: true},
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
		if rc := findRevokeCall(mock.revokeCalls, isDatabaseResource("old_db")); rc != nil {
			t.Errorf("unexpected revoke for removed database: %+v", rc)
		}
	})

	// ── "only permissions set" semantics ─────────────────────────────────────────

	t.Run("catalog_only_permissions_set_revokes_existing_grant_option", func(t *testing.T) {
		// State: DESCRIBE regular + DESCRIBE grantable.
		// Plan: DESCRIBE regular only (grantable_permissions absent).
		// Expected: grant option revoked, regular permission untouched.
		mock := mockWithCurrentPerms(
			[]lftypes.Permission{lftypes.PermissionDescribe},
			[]lftypes.Permission{lftypes.PermissionDescribe},
		)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:                   types.StringValue(catalogID),
				Permissions:          &Permissions{Describe: true},
				GrantablePermissions: &Permissions{Describe: true},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:          types.StringValue(catalogID),
				Permissions: &Permissions{Describe: true},
				// GrantablePermissions absent → treated as empty
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// AWS requires Permissions non-empty in RevokePermissions → revoke+regrant.
		rc := findRevokeCall(mock.revokeCalls, isCatalogResource)
		if rc == nil {
			t.Fatal("expected revoke call on catalog to remove grant option")
		}
		if !permsEqual(rc.Permissions, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("Permissions in revoke = %v, want [DESCRIBE]", rc.Permissions)
		}
		if !permsEqual(rc.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("grant option revoked = %v, want [DESCRIBE]", rc.PermissionsWithGrantOption)
		}
		gc := findGrantCall(mock.grantCalls, isCatalogResource)
		if gc == nil {
			t.Fatal("expected regrant call on catalog to restore base DESCRIBE")
		}
		if !permsEqual(gc.Permissions, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("regrant Permissions = %v, want [DESCRIBE]", gc.Permissions)
		}
	})

	t.Run("database_only_permissions_set_revokes_existing_grant_option", func(t *testing.T) {
		// State: ALTER regular + ALTER grantable.
		// Plan: ALTER regular only (grantable_permissions absent).
		// Expected: grant option revoked, regular permission untouched.
		mock := mockWithCurrentPerms(
			[]lftypes.Permission{lftypes.PermissionAlter},
			[]lftypes.Permission{lftypes.PermissionAlter},
		)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:                 types.StringValue("db"),
					Permissions:          &Permissions{Alter: true},
					GrantablePermissions: &Permissions{Alter: true},
				}},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("db"),
					Permissions: &Permissions{Alter: true},
					// GrantablePermissions absent → treated as empty
				}},
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// AWS requires Permissions non-empty in RevokePermissions → revoke+regrant.
		rc := findRevokeCall(mock.revokeCalls, isDatabaseResource("db"))
		if rc == nil {
			t.Fatal("expected revoke call on database to remove grant option")
		}
		if !permsEqual(rc.Permissions, []lftypes.Permission{lftypes.PermissionAlter}) {
			t.Errorf("Permissions in revoke = %v, want [ALTER]", rc.Permissions)
		}
		if !permsEqual(rc.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionAlter}) {
			t.Errorf("grant option revoked = %v, want [ALTER]", rc.PermissionsWithGrantOption)
		}
		gc := findGrantCall(mock.grantCalls, isDatabaseResource("db"))
		if gc == nil {
			t.Fatal("expected regrant call on database to restore base ALTER")
		}
		if !permsEqual(gc.Permissions, []lftypes.Permission{lftypes.PermissionAlter}) {
			t.Errorf("regrant Permissions = %v, want [ALTER]", gc.Permissions)
		}
	})

	t.Run("catalog_neither_set_no_api_calls", func(t *testing.T) {
		// State: DESCRIBE regular + DESCRIBE grantable.
		// Plan: catalog block present but neither permissions nor grantable_permissions set
		// (UseStateForUnknown loaded state values → plan == state, no diff).
		mock := &mockLFClient{}
		perms := &Permissions{Describe: true}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:                   types.StringValue(catalogID),
				Permissions:          perms,
				GrantablePermissions: perms,
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				// Neither permissions nor grantable_permissions set in plan.
				// Outer guard (plan.Permissions != nil || plan.GrantablePermissions != nil)
				// is false → block is skipped entirely.
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.grantCalls) != 0 || len(mock.revokeCalls) != 0 {
			t.Errorf("expected no API calls; got %d grant(s), %d revoke(s)",
				len(mock.grantCalls), len(mock.revokeCalls))
		}
	})

	t.Run("database_neither_set_no_api_calls", func(t *testing.T) {
		// State: DESCRIBE regular + DESCRIBE grantable on database.
		// Plan: database block present but neither permissions nor grantable_permissions set.
		// Expected: no diff, no API calls.
		mock := &mockLFClient{}
		perms := &Permissions{Describe: true}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:                 types.StringValue("db"),
					Permissions:          perms,
					GrantablePermissions: perms,
				}},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("db"),
					// Neither permissions nor grantable_permissions set.
				}},
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.grantCalls) != 0 || len(mock.revokeCalls) != 0 {
			t.Errorf("expected no API calls; got %d grant(s), %d revoke(s)",
				len(mock.grantCalls), len(mock.revokeCalls))
		}
	})

	t.Run("database_grantable_dropped_when_only_permissions_set", func(t *testing.T) {
		// State: database has permissions=[DESCRIBE] and grantable=[DESCRIBE].
		// Plan: permissions=[DESCRIBE], grantable_permissions=&{} (empty — auto-filled by syncPermissions).
		// Expected: grantable DESCRIBE is revoked; regular DESCRIBE is untouched.
		mock := mockWithCurrentPerms(
			[]lftypes.Permission{lftypes.PermissionDescribe},
			[]lftypes.Permission{lftypes.PermissionDescribe},
		)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:                 types.StringValue("db"),
					Permissions:          &Permissions{Describe: true},
					GrantablePermissions: &Permissions{Describe: true},
				}},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:                 types.StringValue("db"),
					Permissions:          &Permissions{Describe: true},
					GrantablePermissions: &Permissions{}, // empty: auto-filled by syncPermissions
				}},
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// AWS requires Permissions non-empty in RevokePermissions → revoke+regrant.
		rc := findRevokeCall(mock.revokeCalls, isDatabaseResource("db"))
		if rc == nil {
			t.Fatal("expected RevokePermissions call to remove database grantable DESCRIBE")
		}
		if !permsEqual(rc.Permissions, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("Permissions in revoke = %v, want [DESCRIBE]", rc.Permissions)
		}
		if !permsEqual(rc.PermissionsWithGrantOption, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("PermissionsWithGrantOption = %v, want [DESCRIBE]", rc.PermissionsWithGrantOption)
		}
		gc := findGrantCall(mock.grantCalls, isDatabaseResource("db"))
		if gc == nil {
			t.Fatal("expected regrant call to restore base DESCRIBE after revoke+regrant")
		}
		if !permsEqual(gc.Permissions, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("regrant Permissions = %v, want [DESCRIBE]", gc.Permissions)
		}
	})

	t.Run("grant_option_revoke_non_super_user_error_propagated", func(t *testing.T) {
		// State: SELECT regular + SELECT grantable.
		// Plan: SELECT regular only (removing grant option).
		// The keepRevokeG revoke (revoke SELECT+grant_option while keeping SELECT) returns a
		// non-SUPER_USER error. Unlike the SUPER_USER case, this must not be swallowed.
		mock := &mockLFClient{
			listResult: []lftypes.PrincipalResourcePermissions{{
				Permissions:                []lftypes.Permission{lftypes.PermissionSelect},
				PermissionsWithGrantOption: []lftypes.Permission{lftypes.PermissionSelect},
			}},
			revokeErr: fmt.Errorf("access denied"),
		}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:                 types.StringValue("db"),
					Permissions:          &Permissions{Select: true},
					GrantablePermissions: &Permissions{Select: true},
				}},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("db"),
					Permissions: &Permissions{Select: true},
					// GrantablePermissions absent → removing grant option
				}},
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err == nil {
			t.Fatal("expected error from non-SUPER_USER revoke failure, got nil")
		}
	})

	t.Run("nil_plan_catalog_no_api_calls", func(t *testing.T) {
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:          types.StringValue(catalogID),
				Permissions: &Permissions{Describe: true},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			// Catalog nil
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.grantCalls) != 0 || len(mock.revokeCalls) != 0 {
			t.Errorf("expected no API calls when plan catalog is nil; got %d grant(s), %d revoke(s)",
				len(mock.grantCalls), len(mock.revokeCalls))
		}
	})

	t.Run("new_table_in_plan_fully_granted", func(t *testing.T) {
		// Table present in plan but absent from state → grant its permissions.
		mock := mockWithCurrentPerms(nil, nil)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("analytics"),
				}},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("analytics"),
					Table: []TablePermModel{{
						Name:        types.StringValue("events"),
						Permissions: &Permissions{Select: true},
					}},
				}},
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		call := findGrantCall(mock.grantCalls, isTableResource("analytics", "events"))
		if call == nil {
			t.Fatal("expected GrantPermissions call for new table")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("grant permissions = %v, want [SELECT]", call.Permissions)
		}
	})

	t.Run("removed_table_not_revoked", func(t *testing.T) {
		// Table present in state but absent from plan → not revoked by updatePermissions
		// (caller must handle removal explicitly if needed).
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("analytics"),
					Table: []TablePermModel{{
						Name:        types.StringValue("events"),
						Permissions: &Permissions{Select: true},
					}},
				}},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("analytics"),
					// Table absent from plan
				}},
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if call := findRevokeCall(mock.revokeCalls, isTableResource("analytics", "events")); call != nil {
			t.Errorf("expected no revoke for table removed from plan; got %+v", call)
		}
	})

	t.Run("wildcard_added_granted", func(t *testing.T) {
		// Plan adds a wildcard where state had none.
		mock := mockWithCurrentPerms(nil, nil)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("analytics"),
				}},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:     types.StringValue("analytics"),
					Wildcard: &TablePermModel{Permissions: &Permissions{Select: true}},
				}},
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		call := findGrantCall(mock.grantCalls, isWildcardResource("analytics"))
		if call == nil {
			t.Fatal("expected GrantPermissions call for new wildcard")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionSelect}) {
			t.Errorf("grant permissions = %v, want [SELECT]", call.Permissions)
		}
	})

	t.Run("wildcard_removed_not_revoked", func(t *testing.T) {
		// Wildcard present in state but absent from plan → not revoked by updatePermissions.
		mock := &mockLFClient{}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:     types.StringValue("analytics"),
					Wildcard: &TablePermModel{Permissions: &Permissions{Select: true}},
				}},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("analytics"),
					// Wildcard absent from plan
				}},
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if call := findRevokeCall(mock.revokeCalls, isWildcardResource("analytics")); call != nil {
			t.Errorf("expected no revoke for wildcard removed from plan; got %+v", call)
		}
	})

	t.Run("wildcard_permissions_changed", func(t *testing.T) {
		// Wildcard in both state and plan but permissions differ → updated.
		mock := mockWithCurrentPerms(
			[]lftypes.Permission{lftypes.PermissionSelect},
			nil,
		)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:     types.StringValue("analytics"),
					Wildcard: &TablePermModel{Permissions: &Permissions{Select: true}},
				}},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("analytics"),
					Wildcard: &TablePermModel{
						Permissions: &Permissions{Select: true, Insert: true},
					},
				}},
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		call := findGrantCall(mock.grantCalls, isWildcardResource("analytics"))
		if call == nil {
			t.Fatal("expected GrantPermissions call for changed wildcard")
		}
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionInsert}) {
			t.Errorf("grant permissions = %v, want [INSERT] (the added permission)", call.Permissions)
		}
	})

	t.Run("table_permissions_changed_others_untouched", func(t *testing.T) {
		// Two tables: one changes, one is unchanged. Only the changed table gets an API call.
		mock := &mockLFClient{
			listFn: func(_ int, input *lakeformation.ListPermissionsInput) ([]lftypes.PrincipalResourcePermissions, error) {
				if input.Resource.Table != nil && aws.ToString(input.Resource.Table.Name) == "events" {
					return []lftypes.PrincipalResourcePermissions{
						{Permissions: []lftypes.Permission{lftypes.PermissionSelect}},
					}, nil
				}
				// "logs" table: SELECT already held, matches plan
				return []lftypes.PrincipalResourcePermissions{
					{Permissions: []lftypes.Permission{lftypes.PermissionDescribe}},
				}, nil
			},
		}
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("analytics"),
					Table: []TablePermModel{
						{Name: types.StringValue("events"), Permissions: &Permissions{Select: true}},
						{Name: types.StringValue("logs"), Permissions: &Permissions{Describe: true}},
					},
				}},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name: types.StringValue("analytics"),
					Table: []TablePermModel{
						// events: adding INSERT
						{Name: types.StringValue("events"), Permissions: &Permissions{Select: true, Insert: true}},
						// logs: unchanged
						{Name: types.StringValue("logs"), Permissions: &Permissions{Describe: true}},
					},
				}},
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if call := findGrantCall(mock.grantCalls, isTableResource("analytics", "events")); call == nil {
			t.Fatal("expected grant call for events table")
		} else if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionInsert}) {
			t.Errorf("events grant = %v, want [INSERT]", call.Permissions)
		}
		if call := findGrantCall(mock.grantCalls, isTableResource("analytics", "logs")); call != nil {
			t.Errorf("expected no grant call for unchanged logs table; got %+v", call)
		}
		if call := findRevokeCall(mock.revokeCalls, isTableResource("analytics", "logs")); call != nil {
			t.Errorf("expected no revoke call for unchanged logs table; got %+v", call)
		}
	})

	t.Run("new_database_with_tables_fully_granted", func(t *testing.T) {
		// New database (absent from state) with nested tables → database and all tables granted.
		mock := mockWithCurrentPerms(nil, nil)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog:   &CatalogPermModel{ID: types.StringValue(catalogID)},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("newdb"),
					Permissions: &Permissions{Describe: true},
					Table: []TablePermModel{
						{Name: types.StringValue("t1"), Permissions: &Permissions{Select: true}},
						{Name: types.StringValue("t2"), Permissions: &Permissions{Insert: true}},
					},
				}},
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if findGrantCall(mock.grantCalls, isDatabaseResource("newdb")) == nil {
			t.Error("expected grant call for new database")
		}
		if findGrantCall(mock.grantCalls, isTableResource("newdb", "t1")) == nil {
			t.Error("expected grant call for table t1 in new database")
		}
		if findGrantCall(mock.grantCalls, isTableResource("newdb", "t2")) == nil {
			t.Error("expected grant call for table t2 in new database")
		}
	})

	t.Run("aws_drift_update_computes_diff_against_aws_not_state", func(t *testing.T) {
		// State says [SELECT, DESCRIBE]; AWS actually holds only [SELECT] (DESCRIBE drifted).
		// Plan adds INSERT. The grant must be [DESCRIBE, INSERT] (diff against AWS),
		// not just [INSERT] (diff against state).
		mock := mockWithCurrentPerms(
			[]lftypes.Permission{lftypes.PermissionSelect},
			nil,
		)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("analytics"),
					Permissions: &Permissions{Select: true, Describe: true},
				}},
			},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID: types.StringValue(catalogID),
				Database: []DatabasePermModel{{
					Name:        types.StringValue("analytics"),
					Permissions: &Permissions{Select: true, Describe: true, Insert: true},
				}},
			},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		call := findGrantCall(mock.grantCalls, isDatabaseResource("analytics"))
		if call == nil {
			t.Fatal("expected grant call for database")
		}
		// applyDiff computed diff against AWS ([SELECT]), so grant must include DESCRIBE
		// (drifted away) and INSERT (newly added), not just INSERT.
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionDescribe, lftypes.PermissionInsert}) {
			t.Errorf("grant permissions = %v, want [DESCRIBE, INSERT]", call.Permissions)
		}
	})
}

func TestModifyPlan(t *testing.T) {
	ctx := context.Background()
	const principal = "arn:aws:iam::123456789012:role/role"
	const catalogID = "123456789012"

	buildPlan := func(r *LakeFormationPermissionsResource, model *LakeFormationPermissionsResourceModel) tfsdk.Plan {
		var schemaResp resource.SchemaResponse
		r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
		planType := schemaResp.Schema.Type().TerraformType(ctx)
		p := tfsdk.Plan{
			Schema: schemaResp.Schema,
			Raw:    tftypes.NewValue(planType, nil),
		}
		if diags := p.Set(ctx, model); diags.HasError() {
			panic(fmt.Sprintf("buildPlan: %v", diags))
		}
		return p
	}

	getModel := func(p tfsdk.Plan) *LakeFormationPermissionsResourceModel {
		var m LakeFormationPermissionsResourceModel
		if diags := p.Get(ctx, &m); diags.HasError() {
			panic(fmt.Sprintf("getModel: %v", diags))
		}
		return &m
	}

	callModifyPlan := func(r *LakeFormationPermissionsResource, plan tfsdk.Plan) resource.ModifyPlanResponse {
		resp := resource.ModifyPlanResponse{Plan: plan}
		r.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: plan}, &resp)
		return resp
	}

	t.Run("destroy_plan_no_op", func(t *testing.T) {
		r := &LakeFormationPermissionsResource{awsCfg: aws.Config{Region: "us-east-1"}}
		var schemaResp resource.SchemaResponse
		r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
		planType := schemaResp.Schema.Type().TerraformType(ctx)
		destroyPlan := tfsdk.Plan{
			Schema: schemaResp.Schema,
			Raw:    tftypes.NewValue(planType, nil), // null Raw = destroy
		}
		resp := resource.ModifyPlanResponse{Plan: destroyPlan}
		r.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: destroyPlan}, &resp)
		if resp.Diagnostics.HasError() {
			t.Errorf("unexpected error on destroy plan: %v", resp.Diagnostics)
		}
	})

	t.Run("region_null_resolved_from_provider", func(t *testing.T) {
		r := &LakeFormationPermissionsResource{awsCfg: aws.Config{Region: "eu-west-1"}}
		plan := buildPlan(r, &LakeFormationPermissionsResourceModel{
			Region:    types.StringNull(),
			Principal: types.StringValue(principal),
		})
		resp := callModifyPlan(r, plan)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected error: %v", resp.Diagnostics)
		}
		if got := getModel(resp.Plan).Region.ValueString(); got != "eu-west-1" {
			t.Errorf("region = %q, want eu-west-1", got)
		}
	})

	t.Run("region_explicit_unchanged", func(t *testing.T) {
		r := &LakeFormationPermissionsResource{awsCfg: aws.Config{Region: "eu-west-1"}}
		plan := buildPlan(r, &LakeFormationPermissionsResourceModel{
			Region:    types.StringValue("ap-southeast-1"),
			Principal: types.StringValue(principal),
		})
		resp := callModifyPlan(r, plan)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected error: %v", resp.Diagnostics)
		}
		if got := getModel(resp.Plan).Region.ValueString(); got != "ap-southeast-1" {
			t.Errorf("region = %q, want ap-southeast-1", got)
		}
	})

	t.Run("region_error_when_not_configured", func(t *testing.T) {
		r := &LakeFormationPermissionsResource{awsCfg: aws.Config{}}
		plan := buildPlan(r, &LakeFormationPermissionsResourceModel{
			Region:    types.StringNull(),
			Principal: types.StringValue(principal),
		})
		resp := callModifyPlan(r, plan)
		if !resp.Diagnostics.HasError() {
			t.Error("expected error when no region is configured")
		}
	})

	t.Run("grantable_only_fills_permissions", func(t *testing.T) {
		r := &LakeFormationPermissionsResource{awsCfg: aws.Config{Region: "us-east-1"}}
		plan := buildPlan(r, &LakeFormationPermissionsResourceModel{
			Region:    types.StringValue("us-east-1"),
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:                   types.StringValue(catalogID),
				GrantablePermissions: &Permissions{Describe: true},
			},
		})
		resp := callModifyPlan(r, plan)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected error: %v", resp.Diagnostics)
		}
		got := getModel(resp.Plan)
		if got.Catalog == nil {
			t.Fatal("expected catalog in plan")
		}
		if got.Catalog.Permissions == nil {
			t.Fatal("expected catalog.permissions to be filled from grantable_permissions")
		}
		if !got.Catalog.Permissions.Describe {
			t.Error("expected catalog.permissions.describe=true")
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

	t.Run("resource_region_unknown_falls_through_to_provider", func(t *testing.T) {
		r := &LakeFormationPermissionsResource{awsCfg: aws.Config{Region: "us-west-2"}}
		got, err := r.resolveRegion(types.StringUnknown())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "us-west-2" {
			t.Errorf("got %q, want us-west-2", got)
		}
	})

	t.Run("resource_region_empty_string_falls_through_to_provider", func(t *testing.T) {
		r := &LakeFormationPermissionsResource{awsCfg: aws.Config{Region: "us-west-2"}}
		got, err := r.resolveRegion(types.StringValue(""))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "us-west-2" {
			t.Errorf("got %q, want us-west-2", got)
		}
	})
}

// ── Concurrency retry ─────────────────────────────────────────────────────────

func TestConcurrencyRetry(t *testing.T) {
	ctx := context.Background()
	principal := "arn:aws:iam::123456789012:role/role"
	concErr := &lftypes.ConcurrentModificationException{Message: aws.String("concurrent modification")}
	catalogRes := &lftypes.Resource{Catalog: &lftypes.CatalogResource{}}

	// Disable sleep so tests run instantly.
	orig := lfPermsSleepFn
	lfPermsSleepFn = func(_ context.Context, _ int) error { return nil }
	defer func() { lfPermsSleepFn = orig }()

	t.Run("grant_retries_on_concurrency_error_then_succeeds", func(t *testing.T) {
		mock := &mockLFClient{
			grantFn: func(idx int, _ *lakeformation.GrantPermissionsInput) error {
				if idx == 0 {
					return concErr
				}
				return nil
			},
		}
		err := applyResource(ctx, mock, principal, catalogRes,
			[]lftypes.Permission{lftypes.PermissionDescribe}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.grantCalls) != 2 {
			t.Errorf("expected 2 grant calls (1 fail + 1 retry), got %d", len(mock.grantCalls))
		}
	})

	t.Run("grant_skipped_if_already_applied_after_refresh", func(t *testing.T) {
		// First grant returns concurrency error; re-read before retry shows permission
		// already granted so applyDiff produces no-op and grant is not retried.
		listCalls := 0
		mock := &mockLFClient{
			grantFn: func(idx int, _ *lakeformation.GrantPermissionsInput) error {
				if idx == 0 {
					return concErr
				}
				return nil
			},
			listFn: func(call int, _ *lakeformation.ListPermissionsInput) ([]lftypes.PrincipalResourcePermissions, error) {
				listCalls++
				if listCalls == 1 {
					return nil, nil // first read: nothing granted yet
				}
				// second read (before retry): already granted by the concurrent process
				return []lftypes.PrincipalResourcePermissions{
					{Permissions: []lftypes.Permission{lftypes.PermissionDescribe}},
				}, nil
			},
		}
		err := applyResource(ctx, mock, principal, catalogRes,
			[]lftypes.Permission{lftypes.PermissionDescribe}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.grantCalls) != 1 {
			t.Errorf("expected 1 grant call (the failing one), got %d; retry read showed permission already applied", len(mock.grantCalls))
		}
		if listCalls != 2 {
			t.Errorf("expected 2 list calls (initial read + retry read), got %d", listCalls)
		}
	})

	t.Run("revoke_retries_on_concurrency_error_then_succeeds", func(t *testing.T) {
		mock := &mockLFClient{
			listResult: []lftypes.PrincipalResourcePermissions{
				{Permissions: []lftypes.Permission{lftypes.PermissionDescribe}},
			},
			revokeFn: func(idx int, _ *lakeformation.RevokePermissionsInput) error {
				if idx == 0 {
					return concErr
				}
				return nil
			},
		}
		// plan wants nothing → reads DESCRIBE from AWS → revokes it
		err := applyResource(ctx, mock, principal, catalogRes, nil, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.revokeCalls) != 2 {
			t.Errorf("expected 2 revoke calls (1 fail + 1 retry), got %d", len(mock.revokeCalls))
		}
	})

	t.Run("revoke_skipped_if_already_revoked_after_refresh", func(t *testing.T) {
		listCall := 0
		mock := &mockLFClient{
			revokeFn: func(idx int, _ *lakeformation.RevokePermissionsInput) error {
				if idx == 0 {
					return concErr
				}
				return nil
			},
			listFn: func(call int, _ *lakeformation.ListPermissionsInput) ([]lftypes.PrincipalResourcePermissions, error) {
				listCall++
				if listCall == 1 {
					// Initial read: permission present
					return []lftypes.PrincipalResourcePermissions{
						{Permissions: []lftypes.Permission{lftypes.PermissionDescribe}},
					}, nil
				}
				// Retry read: already revoked by concurrent process
				return nil, nil
			},
		}
		// plan wants nothing → reads DESCRIBE → revokes (fails) → retry reads nothing → no-op
		err := applyResource(ctx, mock, principal, catalogRes, nil, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.revokeCalls) != 1 {
			t.Errorf("expected 1 revoke call (the failing one), got %d; retry read showed permission already revoked", len(mock.revokeCalls))
		}
	})

	t.Run("non_concurrency_error_not_retried", func(t *testing.T) {
		otherErr := fmt.Errorf("AccessDeniedException")
		mock := &mockLFClient{
			grantFn: func(_ int, _ *lakeformation.GrantPermissionsInput) error {
				return otherErr
			},
		}
		err := applyResource(ctx, mock, principal, catalogRes,
			[]lftypes.Permission{lftypes.PermissionDescribe}, nil)
		if err == nil {
			t.Fatal("expected error to propagate")
		}
		if len(mock.grantCalls) != 1 {
			t.Errorf("expected exactly 1 grant call (no retry), got %d", len(mock.grantCalls))
		}
	})

	t.Run("exhausts_retries_and_returns_error", func(t *testing.T) {
		mock := &mockLFClient{
			grantFn: func(_ int, _ *lakeformation.GrantPermissionsInput) error {
				return concErr
			},
		}
		err := applyResource(ctx, mock, principal, catalogRes,
			[]lftypes.Permission{lftypes.PermissionDescribe}, nil)
		if err == nil {
			t.Fatal("expected error after exhausting retries")
		}
		if !isConcurrencyErr(err) {
			t.Errorf("expected ConcurrentModificationException, got %v", err)
		}
		if len(mock.grantCalls) != lfPermsMaxRetries+1 {
			t.Errorf("expected %d grant calls, got %d", lfPermsMaxRetries+1, len(mock.grantCalls))
		}
	})

	t.Run("create_grant_retries_via_grantAll", func(t *testing.T) {
		mock := &mockLFClient{
			grantFn: func(idx int, _ *lakeformation.GrantPermissionsInput) error {
				if idx == 0 {
					return concErr
				}
				return nil
			},
		}
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Region:    types.StringValue("us-east-1"),
			Catalog: &CatalogPermModel{
				ID:          types.StringValue("123456789012"),
				Permissions: &Permissions{Describe: true},
			},
		}
		if err := grantAll(ctx, mock, data); err != nil {
			t.Fatalf("grantAll() error: %v", err)
		}
		if len(mock.grantCalls) != 2 {
			t.Errorf("expected 2 grant calls (1 fail + 1 retry), got %d", len(mock.grantCalls))
		}
	})

	t.Run("list_error_propagated", func(t *testing.T) {
		// ListPermissions failing on the first attempt should be returned immediately;
		// no grant or revoke calls should be made.
		mock := &mockLFClient{listErr: fmt.Errorf("network error")}
		err := applyResource(ctx, mock, principal, catalogRes,
			[]lftypes.Permission{lftypes.PermissionDescribe}, nil)
		if err == nil {
			t.Fatal("expected error from ListPermissions failure")
		}
		if len(mock.grantCalls) != 0 {
			t.Errorf("expected no grant calls after list failure; got %d", len(mock.grantCalls))
		}
	})

	t.Run("context_cancelled_during_sleep_returns_concurrency_error", func(t *testing.T) {
		// When lfPermsSleepFn signals cancellation after a concurrency error,
		// applyResource returns the original concurrency error (not the sleep error).
		origSleep := lfPermsSleepFn
		lfPermsSleepFn = func(_ context.Context, _ int) error {
			return fmt.Errorf("context cancelled")
		}
		defer func() { lfPermsSleepFn = origSleep }()

		mock := &mockLFClient{
			grantFn: func(_ int, _ *lakeformation.GrantPermissionsInput) error {
				return concErr
			},
		}
		err := applyResource(ctx, mock, principal, catalogRes,
			[]lftypes.Permission{lftypes.PermissionDescribe}, nil)
		if err == nil {
			t.Fatal("expected error after sleep cancellation")
		}
		if !isConcurrencyErr(err) {
			t.Errorf("expected ConcurrentModificationException (not sleep error), got %v", err)
		}
		if len(mock.grantCalls) != 1 {
			t.Errorf("expected 1 grant call before sleep cancelled, got %d", len(mock.grantCalls))
		}
	})

	t.Run("list_error_on_retry_propagated", func(t *testing.T) {
		// First iteration: list succeeds, applyDiff returns a concurrency error.
		// After sleep, second iteration: list itself fails — that error is returned.
		listCalls := 0
		mock := &mockLFClient{
			grantFn: func(_ int, _ *lakeformation.GrantPermissionsInput) error {
				return concErr
			},
			listFn: func(_ int, _ *lakeformation.ListPermissionsInput) ([]lftypes.PrincipalResourcePermissions, error) {
				listCalls++
				if listCalls == 1 {
					return nil, nil // first list: nothing held, triggers grant attempt
				}
				return nil, fmt.Errorf("network error on retry list")
			},
		}
		err := applyResource(ctx, mock, principal, catalogRes,
			[]lftypes.Permission{lftypes.PermissionDescribe}, nil)
		if err == nil {
			t.Fatal("expected error from list failure on retry")
		}
		if isConcurrencyErr(err) {
			t.Errorf("expected list error, not concurrency error; got %v", err)
		}
		if listCalls != 2 {
			t.Errorf("expected 2 list calls (initial + retry), got %d", listCalls)
		}
	})
}

// ── isConcurrencyErr ─────────────────────────────────────────────────────────

func TestIsConcurrencyErr(t *testing.T) {
	t.Run("nil_returns_false", func(t *testing.T) {
		if isConcurrencyErr(nil) {
			t.Error("expected false for nil error")
		}
	})

	t.Run("non_concurrency_error_returns_false", func(t *testing.T) {
		if isConcurrencyErr(fmt.Errorf("some other error")) {
			t.Error("expected false for non-concurrency error")
		}
	})

	t.Run("concurrency_exception_returns_true", func(t *testing.T) {
		err := &lftypes.ConcurrentModificationException{Message: aws.String("concurrent")}
		if !isConcurrencyErr(err) {
			t.Error("expected true for ConcurrentModificationException")
		}
	})
}

// ── isSuperUserGrantErr ───────────────────────────────────────────────────────

func TestIsSuperUserGrantErr(t *testing.T) {
	t.Run("nil_returns_false", func(t *testing.T) {
		if isSuperUserGrantErr(nil) {
			t.Error("expected false for nil error")
		}
	})

	t.Run("non_matching_error_returns_false", func(t *testing.T) {
		if isSuperUserGrantErr(fmt.Errorf("some other error")) {
			t.Error("expected false for unrelated error")
		}
	})

	t.Run("matching_error_returns_true", func(t *testing.T) {
		err := fmt.Errorf("StatusCode: 400, InvalidInputException: Grant options not allowed for SUPER_USER grant")
		if !isSuperUserGrantErr(err) {
			t.Error("expected true for SUPER_USER grant error")
		}
	})
}

// ── needsUpdate ──────────────────────────────────────────────────────────────

func TestNeedsUpdate(t *testing.T) {
	desc := &Permissions{Describe: true}
	alter := &Permissions{Alter: true}

	t.Run("both_plan_nil_returns_false", func(t *testing.T) {
		if needsUpdate(desc, nil, desc, nil) {
			t.Error("expected false when both plan pointers are nil")
		}
	})

	t.Run("state_and_plan_equal_returns_false", func(t *testing.T) {
		if needsUpdate(desc, desc, nil, nil) {
			t.Error("expected false when state and plan permissions are equal")
		}
	})

	t.Run("perms_differ_returns_true", func(t *testing.T) {
		if !needsUpdate(desc, alter, nil, nil) {
			t.Error("expected true when permissions differ")
		}
	})

	t.Run("grantable_differ_returns_true", func(t *testing.T) {
		if !needsUpdate(desc, desc, desc, alter) {
			t.Error("expected true when grantable permissions differ")
		}
	})

	t.Run("state_nil_plan_non_nil_returns_true", func(t *testing.T) {
		if !needsUpdate(nil, desc, nil, nil) {
			t.Error("expected true when state is nil but plan has permissions")
		}
	})

	t.Run("only_plan_grantable_set_still_checks", func(t *testing.T) {
		// plP is nil but plG is non-nil — the early return does not fire.
		if !needsUpdate(nil, nil, nil, desc) {
			t.Error("expected true when only plan grantable differs from nil state")
		}
	})

	t.Run("plan_nil_state_non_nil_returns_false", func(t *testing.T) {
		// Both plan pointers nil → early return false regardless of state.
		// This is the "database removed from plan" case: needsUpdate returns false
		// so updatePermissions does not touch the resource.
		if needsUpdate(desc, nil, desc, nil) {
			t.Error("expected false when both plan pointers are nil even if state is non-nil")
		}
	})

	t.Run("all_true_equal_returns_false", func(t *testing.T) {
		all := &Permissions{All: true}
		if needsUpdate(all, all, nil, nil) {
			t.Error("expected false when both state and plan have all=true")
		}
	})

	t.Run("all_true_vs_individual_returns_true", func(t *testing.T) {
		// state all=true → [ALL]; plan individual → [DESCRIBE]. Different sets.
		all := &Permissions{All: true}
		if !needsUpdate(all, desc, nil, nil) {
			t.Error("expected true when state is all=true and plan has individual permissions")
		}
	})
}

// ── permSetsEqual ─────────────────────────────────────────────────────────────

func TestPermSetsEqual(t *testing.T) {
	sel := lftypes.PermissionSelect
	desc := lftypes.PermissionDescribe

	t.Run("both_empty_returns_true", func(t *testing.T) {
		if !permSetsEqual(nil, nil) {
			t.Error("expected true for two empty sets")
		}
	})

	t.Run("equal_same_order_returns_true", func(t *testing.T) {
		if !permSetsEqual([]lftypes.Permission{sel, desc}, []lftypes.Permission{sel, desc}) {
			t.Error("expected true for equal sets in the same order")
		}
	})

	t.Run("equal_different_order_returns_true", func(t *testing.T) {
		if !permSetsEqual([]lftypes.Permission{sel, desc}, []lftypes.Permission{desc, sel}) {
			t.Error("expected true for equal sets in different order")
		}
	})

	t.Run("different_lengths_returns_false", func(t *testing.T) {
		if permSetsEqual([]lftypes.Permission{sel}, []lftypes.Permission{sel, desc}) {
			t.Error("expected false for sets of different lengths")
		}
	})

	t.Run("same_length_different_elements_returns_false", func(t *testing.T) {
		if permSetsEqual([]lftypes.Permission{sel}, []lftypes.Permission{desc}) {
			t.Error("expected false for sets with different elements")
		}
	})
}

// ── permUnion ────────────────────────────────────────────────────────────────

func TestPermUnion(t *testing.T) {
	sel := lftypes.PermissionSelect
	desc := lftypes.PermissionDescribe

	t.Run("b_empty_returns_a_unchanged", func(t *testing.T) {
		a := []lftypes.Permission{sel}
		got := permUnion(a, nil)
		if !permSetsEqual(got, []lftypes.Permission{sel}) {
			t.Errorf("got %v, want [SELECT]", got)
		}
	})

	t.Run("a_empty_returns_b_elements", func(t *testing.T) {
		got := permUnion(nil, []lftypes.Permission{sel})
		if !permSetsEqual(got, []lftypes.Permission{sel}) {
			t.Errorf("got %v, want [SELECT]", got)
		}
	})

	t.Run("disjoint_appends_all_of_b", func(t *testing.T) {
		got := permUnion([]lftypes.Permission{sel}, []lftypes.Permission{desc})
		if !permSetsEqual(got, []lftypes.Permission{sel, desc}) {
			t.Errorf("got %v, want [SELECT DESCRIBE]", got)
		}
	})

	t.Run("b_already_in_a_returns_a_unchanged", func(t *testing.T) {
		got := permUnion([]lftypes.Permission{sel, desc}, []lftypes.Permission{sel})
		if !permSetsEqual(got, []lftypes.Permission{sel, desc}) {
			t.Errorf("got %v, want [SELECT DESCRIBE]", got)
		}
	})
}

// ── intersect ────────────────────────────────────────────────────────────────

func TestIntersect(t *testing.T) {
	sel := lftypes.PermissionSelect
	desc := lftypes.PermissionDescribe

	t.Run("a_empty_returns_nil", func(t *testing.T) {
		if intersect(nil, []lftypes.Permission{sel}) != nil {
			t.Error("expected nil when a is empty")
		}
	})

	t.Run("b_empty_returns_nil", func(t *testing.T) {
		if intersect([]lftypes.Permission{sel}, nil) != nil {
			t.Error("expected nil when b is empty")
		}
	})

	t.Run("no_overlap_returns_nil", func(t *testing.T) {
		if intersect([]lftypes.Permission{sel}, []lftypes.Permission{desc}) != nil {
			t.Error("expected nil for disjoint sets")
		}
	})

	t.Run("partial_overlap_returns_common_elements", func(t *testing.T) {
		got := intersect([]lftypes.Permission{sel, desc}, []lftypes.Permission{desc})
		if !permSetsEqual(got, []lftypes.Permission{desc}) {
			t.Errorf("got %v, want [DESCRIBE]", got)
		}
	})

	t.Run("a_subset_of_b_returns_a", func(t *testing.T) {
		got := intersect([]lftypes.Permission{sel}, []lftypes.Permission{sel, desc})
		if !permSetsEqual(got, []lftypes.Permission{sel}) {
			t.Errorf("got %v, want [SELECT]", got)
		}
	})
}

// ── setSubtract ───────────────────────────────────────────────────────────────

func TestSetSubtract(t *testing.T) {
	sel := lftypes.PermissionSelect
	desc := lftypes.PermissionDescribe

	t.Run("a_empty_returns_nil", func(t *testing.T) {
		if setSubtract(nil, []lftypes.Permission{sel}) != nil {
			t.Error("expected nil when a is empty")
		}
	})

	t.Run("b_empty_returns_a", func(t *testing.T) {
		got := setSubtract([]lftypes.Permission{sel, desc}, nil)
		if !permSetsEqual(got, []lftypes.Permission{sel, desc}) {
			t.Errorf("got %v, want [SELECT DESCRIBE]", got)
		}
	})

	t.Run("all_subtracted_returns_nil", func(t *testing.T) {
		got := setSubtract([]lftypes.Permission{sel}, []lftypes.Permission{sel})
		if len(got) != 0 {
			t.Errorf("got %v, want empty", got)
		}
	})

	t.Run("partial_subtraction_returns_remainder", func(t *testing.T) {
		got := setSubtract([]lftypes.Permission{sel, desc}, []lftypes.Permission{sel})
		if !permSetsEqual(got, []lftypes.Permission{desc}) {
			t.Errorf("got %v, want [DESCRIBE]", got)
		}
	})

	t.Run("no_overlap_returns_a", func(t *testing.T) {
		got := setSubtract([]lftypes.Permission{sel}, []lftypes.Permission{desc})
		if !permSetsEqual(got, []lftypes.Permission{sel}) {
			t.Errorf("got %v, want [SELECT]", got)
		}
	})
}

// ── containsPermission ────────────────────────────────────────────────────────

func TestContainsPermission(t *testing.T) {
	sel := lftypes.PermissionSelect
	desc := lftypes.PermissionDescribe

	t.Run("found_returns_true", func(t *testing.T) {
		if !containsPermission([]lftypes.Permission{sel, desc}, sel) {
			t.Error("expected true when permission is present")
		}
	})

	t.Run("not_found_returns_false", func(t *testing.T) {
		if containsPermission([]lftypes.Permission{desc}, sel) {
			t.Error("expected false when permission is absent")
		}
	})

	t.Run("empty_list_returns_false", func(t *testing.T) {
		if containsPermission(nil, sel) {
			t.Error("expected false for empty permission list")
		}
	})
}

// ── resourceLFType ────────────────────────────────────────────────────────────

func TestResourceLFType(t *testing.T) {
	tests := []struct {
		name string
		res  *lftypes.Resource
		want LFResourceType
	}{
		{
			name: "catalog_resource",
			res:  &lftypes.Resource{Catalog: &lftypes.CatalogResource{Id: aws.String("123")}},
			want: LFResourceTypeCatalog,
		},
		{
			name: "database_resource",
			res:  &lftypes.Resource{Database: &lftypes.DatabaseResource{Name: aws.String("db")}},
			want: LFResourceTypeDatabase,
		},
		{
			name: "named_table_resource",
			res:  &lftypes.Resource{Table: &lftypes.TableResource{Name: aws.String("tbl")}},
			want: LFResourceTypeTable,
		},
		{
			name: "wildcard_table_resource",
			res:  &lftypes.Resource{Table: &lftypes.TableResource{TableWildcard: &lftypes.TableWildcard{}}},
			want: LFResourceTypeTable,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resourceLFType(tc.res); got != tc.want {
				t.Errorf("resourceLFType() = %q, want %q", got, tc.want)
			}
		})
	}
}

// ── collapseImplicitAll ───────────────────────────────────────────────────────

func TestCollapseImplicitAll(t *testing.T) {
	allCatalog := []lftypes.Permission{
		lftypes.PermissionAlter, lftypes.PermissionCreateCatalog,
		lftypes.PermissionCreateDatabase, lftypes.PermissionDescribe, lftypes.PermissionDrop,
	}
	allDatabase := []lftypes.Permission{
		lftypes.PermissionAlter, lftypes.PermissionCreateTable,
		lftypes.PermissionDescribe, lftypes.PermissionDrop,
	}
	allTable := []lftypes.Permission{
		lftypes.PermissionAlter, lftypes.PermissionDelete, lftypes.PermissionDescribe,
		lftypes.PermissionDrop, lftypes.PermissionInsert, lftypes.PermissionSelect,
	}

	t.Run("catalog_all_individuals_collapsed", func(t *testing.T) {
		got := collapseImplicitAll(allCatalog, LFResourceTypeCatalog)
		if !permsEqual(got, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("got %v, want [ALL]", got)
		}
	})

	t.Run("catalog_missing_one_not_collapsed", func(t *testing.T) {
		partial := []lftypes.Permission{
			lftypes.PermissionAlter, lftypes.PermissionCreateCatalog,
			lftypes.PermissionCreateDatabase, lftypes.PermissionDescribe,
			// DROP missing
		}
		got := collapseImplicitAll(partial, LFResourceTypeCatalog)
		if permsEqual(got, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Error("expected no collapse when one individual permission is missing")
		}
	})

	t.Run("database_all_individuals_collapsed", func(t *testing.T) {
		got := collapseImplicitAll(allDatabase, LFResourceTypeDatabase)
		if !permsEqual(got, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("got %v, want [ALL]", got)
		}
	})

	t.Run("table_all_individuals_collapsed", func(t *testing.T) {
		got := collapseImplicitAll(allTable, LFResourceTypeTable)
		if !permsEqual(got, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("got %v, want [ALL]", got)
		}
	})

	t.Run("table_missing_one_not_collapsed", func(t *testing.T) {
		partial := []lftypes.Permission{
			lftypes.PermissionAlter, lftypes.PermissionDelete, lftypes.PermissionDescribe,
			lftypes.PermissionDrop, lftypes.PermissionInsert,
			// SELECT missing
		}
		got := collapseImplicitAll(partial, LFResourceTypeTable)
		if permsEqual(got, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Error("expected no collapse when one individual permission is missing")
		}
	})

	t.Run("explicit_all_already_present_unchanged", func(t *testing.T) {
		// If ALL is already in the list, return as-is without checking individuals.
		perms := []lftypes.Permission{lftypes.PermissionAll, lftypes.PermissionDescribe}
		got := collapseImplicitAll(perms, LFResourceTypeCatalog)
		if !permsEqual(got, perms) {
			t.Errorf("got %v, want unchanged %v", got, perms)
		}
	})

	t.Run("empty_not_collapsed", func(t *testing.T) {
		got := collapseImplicitAll(nil, LFResourceTypeCatalog)
		if len(got) != 0 {
			t.Errorf("got %v, want empty", got)
		}
	})
}

// ── listLFPerms ───────────────────────────────────────────────────────────────

func TestListLFPerms(t *testing.T) {
	ctx := context.Background()
	principal := "arn:aws:iam::123456789012:role/role"
	dbRes := &lftypes.Resource{Database: &lftypes.DatabaseResource{Name: aws.String("db")}}

	t.Run("no_entries_returns_nil_nil", func(t *testing.T) {
		mock := &mockLFClient{}
		perms, grantPerms, err := listLFPerms(ctx, mock, principal, dbRes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if perms != nil {
			t.Errorf("perms = %v, want nil", perms)
		}
		if grantPerms != nil {
			t.Errorf("grantPerms = %v, want nil", grantPerms)
		}
	})

	t.Run("single_page_returns_permissions", func(t *testing.T) {
		mock := &mockLFClient{
			listResult: []lftypes.PrincipalResourcePermissions{
				{
					Permissions:                []lftypes.Permission{lftypes.PermissionDescribe, lftypes.PermissionAlter},
					PermissionsWithGrantOption: []lftypes.Permission{lftypes.PermissionDescribe},
				},
			},
		}
		perms, grantPerms, err := listLFPerms(ctx, mock, principal, dbRes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !permsEqual(perms, []lftypes.Permission{lftypes.PermissionDescribe, lftypes.PermissionAlter}) {
			t.Errorf("perms = %v, want [DESCRIBE, ALTER]", perms)
		}
		if !permsEqual(grantPerms, []lftypes.Permission{lftypes.PermissionDescribe}) {
			t.Errorf("grantPerms = %v, want [DESCRIBE]", grantPerms)
		}
	})

	t.Run("multi_page_results_accumulated", func(t *testing.T) {
		mock := &mockLFClient{
			listPages: [][]lftypes.PrincipalResourcePermissions{
				{{Permissions: []lftypes.Permission{lftypes.PermissionDescribe}}},
				{{Permissions: []lftypes.Permission{lftypes.PermissionAlter}}},
			},
		}
		perms, _, err := listLFPerms(ctx, mock, principal, dbRes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !permsEqual(perms, []lftypes.Permission{lftypes.PermissionDescribe, lftypes.PermissionAlter}) {
			t.Errorf("perms = %v, want [DESCRIBE, ALTER]", perms)
		}
		if mock.listCallIdx != 2 {
			t.Errorf("expected 2 ListPermissions calls, got %d", mock.listCallIdx)
		}
	})

	t.Run("error_propagated", func(t *testing.T) {
		mock := &mockLFClient{listErr: fmt.Errorf("api error")}
		_, _, err := listLFPerms(ctx, mock, principal, dbRes)
		if err == nil {
			t.Error("expected error, got nil")
		}
	})
}

// ── listLFPerms (implicit ALL integration) ───────────────────────────────────

func TestListLFPermsCollapseImplicitAll(t *testing.T) {
	ctx := context.Background()
	principal := "arn:aws:iam::123456789012:role/role"
	catalogRes := &lftypes.Resource{Catalog: &lftypes.CatalogResource{Id: aws.String("123456789012")}}
	allCatalogPerms := []lftypes.Permission{
		lftypes.PermissionAlter, lftypes.PermissionCreateCatalog,
		lftypes.PermissionCreateDatabase, lftypes.PermissionDescribe, lftypes.PermissionDrop,
	}

	t.Run("all_individuals_collapsed_to_ALL_in_permissions", func(t *testing.T) {
		mock := &mockLFClient{
			listResult: []lftypes.PrincipalResourcePermissions{
				{Permissions: allCatalogPerms},
			},
		}
		perms, grantPerms, err := listLFPerms(ctx, mock, principal, catalogRes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !permsEqual(perms, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("perms = %v, want [ALL]", perms)
		}
		if len(grantPerms) != 0 {
			t.Errorf("grantPerms = %v, want empty", grantPerms)
		}
	})

	t.Run("all_individuals_collapsed_to_ALL_in_grantable", func(t *testing.T) {
		mock := &mockLFClient{
			listResult: []lftypes.PrincipalResourcePermissions{
				{
					Permissions:                allCatalogPerms,
					PermissionsWithGrantOption: allCatalogPerms,
				},
			},
		}
		perms, grantPerms, err := listLFPerms(ctx, mock, principal, catalogRes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !permsEqual(perms, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("perms = %v, want [ALL]", perms)
		}
		if !permsEqual(grantPerms, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("grantPerms = %v, want [ALL]", grantPerms)
		}
	})

	t.Run("partial_individuals_not_collapsed", func(t *testing.T) {
		partial := []lftypes.Permission{lftypes.PermissionDescribe, lftypes.PermissionAlter}
		mock := &mockLFClient{
			listResult: []lftypes.PrincipalResourcePermissions{
				{Permissions: partial},
			},
		}
		perms, _, err := listLFPerms(ctx, mock, principal, catalogRes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if containsPermission(perms, lftypes.PermissionAll) {
			t.Errorf("perms = %v, should not contain ALL for partial set", perms)
		}
	})
}

// ── implicit ALL end-to-end ───────────────────────────────────────────────────

func TestImplicitAllEndToEnd(t *testing.T) {
	ctx := context.Background()
	principal := "arn:aws:iam::123456789012:role/role"
	const catalogID = "123456789012"
	allCatalogPerms := []lftypes.Permission{
		lftypes.PermissionAlter, lftypes.PermissionCreateCatalog,
		lftypes.PermissionCreateDatabase, lftypes.PermissionDescribe, lftypes.PermissionDrop,
	}

	t.Run("refreshPerms_all_true_with_implicit_all_from_aws", func(t *testing.T) {
		// declared all=true; AWS returns all individuals (not literal ALL).
		// collapseImplicitAll in listLFPerms converts to [ALL], so refreshPerms
		// correctly keeps all=true in state.
		mock := mockWithCurrentPerms(allCatalogPerms, nil)
		data := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog: &CatalogPermModel{
				ID:          types.StringValue(catalogID),
				Permissions: &Permissions{All: true},
			},
		}
		if err := readPermissions(ctx, mock, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if data.Catalog.Permissions == nil || !data.Catalog.Permissions.All {
			t.Error("expected Permissions.All=true after reading implicit ALL from AWS")
		}
	})

	t.Run("update_no_op_when_aws_has_implicit_all_and_plan_has_all_true", func(t *testing.T) {
		// AWS holds all individual catalog permissions; plan has all=true.
		// collapseImplicitAll converts curP to [ALL], which equals planP=[ALL] → no diff.
		mock := mockWithCurrentPerms(allCatalogPerms, nil)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog:   &CatalogPermModel{ID: types.StringValue(catalogID), Permissions: &Permissions{All: true}},
		}
		plan := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog:   &CatalogPermModel{ID: types.StringValue(catalogID), Permissions: &Permissions{All: true}},
		}
		if err := updatePermissions(ctx, mock, state, plan); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.grantCalls) != 0 || len(mock.revokeCalls) != 0 {
			t.Errorf("expected no API calls when implicit ALL == plan ALL; got %d grant(s), %d revoke(s)",
				len(mock.grantCalls), len(mock.revokeCalls))
		}
	})

	t.Run("delete_implicit_all_revokes_each_individual", func(t *testing.T) {
		// AWS holds all individuals; delete must revoke what AWS actually has.
		// After collapse, curP=[ALL] → applyDiff uses ALL branch → revoke [ALL].
		mock := mockWithCurrentPerms(allCatalogPerms, nil)
		state := &LakeFormationPermissionsResourceModel{
			Principal: types.StringValue(principal),
			Catalog:   &CatalogPermModel{ID: types.StringValue(catalogID), Permissions: &Permissions{All: true}},
		}
		if err := deletePermissions(ctx, mock, state); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		call := findRevokeCall(mock.revokeCalls, isCatalogResource)
		if call == nil {
			t.Fatal("expected RevokePermissions call")
		}
		// After collapse curP=[ALL], applyDiff revokes [ALL] (not the individuals).
		if !permsEqual(call.Permissions, []lftypes.Permission{lftypes.PermissionAll}) {
			t.Errorf("revoke permissions = %v, want [ALL]", call.Permissions)
		}
	})
}

// ── resolveUnknownToNull ──────────────────────────────────────────────────────

func TestResolveUnknownToNull(t *testing.T) {
	ctx := context.Background()
	attrTypes := map[string]attr.Type{"describe": types.BoolType}
	m := resolveUnknownToNull{}

	t.Run("unknown_becomes_null", func(t *testing.T) {
		req := planmodifier.ObjectRequest{PlanValue: types.ObjectUnknown(attrTypes)}
		resp := &planmodifier.ObjectResponse{PlanValue: types.ObjectUnknown(attrTypes)}
		m.PlanModifyObject(ctx, req, resp)
		if !resp.PlanValue.IsNull() {
			t.Errorf("expected null, got unknown=%v null=%v", resp.PlanValue.IsUnknown(), resp.PlanValue.IsNull())
		}
	})

	t.Run("null_unchanged", func(t *testing.T) {
		null := types.ObjectNull(attrTypes)
		req := planmodifier.ObjectRequest{PlanValue: null}
		resp := &planmodifier.ObjectResponse{PlanValue: null}
		m.PlanModifyObject(ctx, req, resp)
		if !resp.PlanValue.IsNull() {
			t.Error("expected null to remain null")
		}
	})

	t.Run("known_unchanged", func(t *testing.T) {
		known, _ := types.ObjectValue(attrTypes, map[string]attr.Value{"describe": types.BoolValue(true)})
		req := planmodifier.ObjectRequest{PlanValue: known}
		resp := &planmodifier.ObjectResponse{PlanValue: known}
		m.PlanModifyObject(ctx, req, resp)
		if resp.PlanValue.IsNull() || resp.PlanValue.IsUnknown() {
			t.Error("expected known value to remain unchanged")
		}
	})
}
