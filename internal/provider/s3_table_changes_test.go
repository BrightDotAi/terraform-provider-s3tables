// Copyright BrightAI 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"testing"

	itable "github.com/apache/iceberg-go/table"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func chgField(name, typ string) FieldModel {
	return FieldModel{
		Name:          types.StringValue(name),
		Type:          types.StringValue(typ),
		Required:      types.BoolValue(false),
		DefaultString: types.StringNull(),
		DefaultNumber: types.NumberNull(),
		DefaultBool:   types.BoolNull(),
		Doc:           types.StringValue(""),
	}
}

func chgListField(name string) FieldModel {
	f := chgField(name, "")
	f.Type = types.StringNull()
	f.ListType = &ListTypeModel{
		ID: types.Int64Value(100), ElementType: types.StringValue("string"), Required: types.BoolValue(false),
	}
	return f
}

func chgNames(fields []FieldModel) []string {
	names := make([]string, 0, len(fields))
	for _, f := range fields {
		names = append(names, f.Name.ValueString())
	}
	return names
}

// stagedTable applies the changes with the real iceberg-go update builders
// on a table built from current and returns the resulting table.
func stagedTable(t *testing.T, current []FieldModel, parts []PartitionModel,
	apply func(txn tableTransaction) error,
) (*itable.Table, error) {
	t.Helper()
	tbl := buildTestTable(t, current, parts)
	txn := tbl.NewTransaction()
	if err := apply(&txnAdapter{txn}); err != nil {
		return nil, err
	}
	staged, err := txn.StagedTable()
	if err != nil {
		t.Fatalf("StagedTable: %v", err)
	}
	return staged.Table, nil
}

func schemaNames(tbl *itable.Table) []string {
	var names []string
	for _, f := range tbl.Schema().Fields() {
		names = append(names, f.Name)
	}
	return names
}

// ── pre-commit validation ─────────────────────────────────────────────────────

func TestPlanSchemaChanges_OnlyChangedAttributesAreSent(t *testing.T) {
	t.Run("doc_change_on_primitive_without_default", func(t *testing.T) {
		cur := chgField("a", "string")
		want := cur
		want.Doc = types.StringValue("new doc")
		plan, err := planSchemaChanges([]FieldModel{cur}, []FieldModel{want})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.updates) != 1 {
			t.Fatalf("expected one update, got %d", len(plan.updates))
		}
		u := plan.updates[0].update
		if !u.Doc.Valid || u.Doc.Val != "new doc" {
			t.Errorf("expected doc update, got %+v", u.Doc)
		}
		if u.FieldType.Valid || u.Required.Valid {
			t.Errorf("type and required must not be sent: %+v", u)
		}
		// A nil write default marked valid panics in iceberg-go v0.6.0.
		if u.WriteDefault.Valid {
			t.Errorf("write default must not be sent when it did not change")
		}
	})

	t.Run("doc_change_on_list_column_sends_no_type", func(t *testing.T) {
		cur := chgListField("tags")
		want := cur
		want.Doc = types.StringValue("tags doc")
		plan, err := planSchemaChanges([]FieldModel{cur}, []FieldModel{want})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.updates) != 1 || plan.updates[0].update.FieldType.Valid {
			t.Fatalf("expected a doc-only update, got %+v", plan.updates)
		}
	})

	t.Run("type_promotion_sends_type_only", func(t *testing.T) {
		plan, err := planSchemaChanges(
			[]FieldModel{chgField("n", "int")}, []FieldModel{chgField("n", "long")})
		if err != nil {
			t.Fatal(err)
		}
		u := plan.updates[0].update
		if !u.FieldType.Valid || u.FieldType.Val.String() != "long" {
			t.Errorf("expected promotion to long, got %+v", u.FieldType)
		}
		if u.Doc.Valid || u.Required.Valid || u.WriteDefault.Valid {
			t.Errorf("unchanged attributes must not be sent: %+v", u)
		}
	})

	t.Run("new_default_is_sent", func(t *testing.T) {
		cur := chgField("s", "string")
		want := cur
		want.DefaultString = types.StringValue("x")
		plan, err := planSchemaChanges([]FieldModel{cur}, []FieldModel{want})
		if err != nil {
			t.Fatal(err)
		}
		u := plan.updates[0].update
		if !u.WriteDefault.Valid || u.WriteDefault.Val == nil {
			t.Errorf("expected a write default, got %+v", u.WriteDefault)
		}
	})
}

func TestPlanSchemaChanges_Rejections(t *testing.T) {
	required := func(f FieldModel) FieldModel { f.Required = types.BoolValue(true); return f }

	cases := []struct {
		name    string
		current []FieldModel
		desired []FieldModel
		wantErr string
		wantIs  error
	}{
		{
			name:    "optional_to_required",
			current: []FieldModel{chgField("a", "string")},
			desired: []FieldModel{required(chgField("a", "string"))},
			wantErr: "optional to required",
		},
		{
			name:    "invalid_promotion",
			current: []FieldModel{chgField("a", "long")},
			desired: []FieldModel{chgField("a", "int")},
			wantErr: "not a valid type promotion",
		},
		{
			name:    "required_add_without_default",
			current: nil,
			desired: []FieldModel{required(chgField("a", "string"))},
			wantErr: "without a default value",
		},
		{
			name: "default_removed",
			current: func() []FieldModel {
				f := chgField("a", "string")
				f.DefaultString = types.StringValue("x")
				return []FieldModel{f}
			}(),
			desired: []FieldModel{chgField("a", "string")},
			wantErr: "removing a column default is not supported",
		},
		{
			name:    "primitive_to_list",
			current: []FieldModel{chgField("a", "string")},
			desired: []FieldModel{chgListField("a")},
			wantErr: "from primitive to list",
		},
		{
			name:    "duplicate_names",
			current: nil,
			desired: []FieldModel{chgField("a", "string"), chgField("a", "long")},
			wantErr: "declared more than once",
		},
		{
			name:    "nested_type_changed",
			current: []FieldModel{chgListField("a")},
			desired: func() []FieldModel {
				f := chgListField("a")
				f.ListType = &ListTypeModel{
					ID: types.Int64Value(100), ElementType: types.StringValue("long"), Required: types.BoolValue(false),
				}
				return []FieldModel{f}
			}(),
			wantIs: errNestedTypeUpdate,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := planSchemaChanges(tc.current, tc.desired)
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("error %v is not %v", err, tc.wantIs)
			}
			if tc.wantIs != nil && !isNonPrimitiveUpdateErr(err) {
				t.Errorf("isNonPrimitiveUpdateErr must recognise %v", err)
			}
		})
	}

	t.Run("rejection_stages_nothing", func(t *testing.T) {
		mock := &mockSchemaUpdater{}
		err := ApplySchemaChanges(&mockTransaction{schema: mock},
			[]FieldModel{chgField("keep", "string"), chgField("a", "long")},
			[]FieldModel{chgField("a", "int")})
		if err == nil {
			t.Fatal("expected an error")
		}
		if len(mock.deletedCols)+len(mock.addedCols)+len(mock.updatedCols) != 0 || mock.commitCalled {
			t.Errorf("a rejected change must not stage anything: %+v", mock)
		}
	})
}

// ── ordering ──────────────────────────────────────────────────────────────────

func TestApplySchemaChanges_AddsFollowConfigOrder(t *testing.T) {
	desired := []FieldModel{chgField("id", "long")}
	for i := range 8 {
		desired = append(desired, chgField(fmt.Sprintf("c%d", i), "string"))
	}
	// Go randomises map iteration, so repeat to catch order dependence.
	for range 50 {
		mock := &mockSchemaUpdater{}
		if err := ApplySchemaChanges(&mockTransaction{schema: mock},
			[]FieldModel{chgField("id", "long")}, desired); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, a := range mock.addedCols {
			got = append(got, a.path[0])
		}
		if !slices.Equal(got, chgNames(desired)[1:]) {
			t.Fatalf("adds not in config order: %v", got)
		}
		if len(mock.moves) != 0 {
			t.Fatalf("no moves expected when columns are appended in order, got %v", mock.moves)
		}
	}
}

func TestApplySchemaChanges_MovesOnlyWhenOrderDiffers(t *testing.T) {
	mock := &mockSchemaUpdater{}
	err := ApplySchemaChanges(&mockTransaction{schema: mock},
		[]FieldModel{chgField("a", "string"), chgField("b", "string")},
		[]FieldModel{chgField("a", "string"), chgField("new", "string"), chgField("b", "string")})
	if err != nil {
		t.Fatal(err)
	}
	if len(mock.moves) != 3 || mock.moves[0].relativeTo != nil ||
		mock.moves[1].path[0] != "new" || mock.moves[1].relativeTo[0] != "a" ||
		mock.moves[2].path[0] != "b" || mock.moves[2].relativeTo[0] != "new" {
		t.Errorf("unexpected moves: %+v", mock.moves)
	}

	mock = &mockSchemaUpdater{}
	err = ApplySchemaChanges(&mockTransaction{schema: mock},
		[]FieldModel{chgField("a", "string"), chgField("b", "string")},
		[]FieldModel{chgField("b", "string"), chgField("a", "string")})
	if err != nil {
		t.Fatal(err)
	}
	if !mock.commitCalled || len(mock.moves) != 2 {
		t.Errorf("a pure reorder must commit moves, got %+v", mock)
	}
}

// ── real iceberg-go update builders ───────────────────────────────────────────
//
// These tests run the staged changes through iceberg-go itself (no catalog is
// involved), because the failure modes they guard against live in that library.

func TestApplySchemaChanges_RealBuilder(t *testing.T) {
	t.Run("doc_change_on_column_without_default_does_not_panic", func(t *testing.T) {
		want := chgField("a", "string")
		want.Doc = types.StringValue("new doc")
		tbl, err := stagedTable(t, []FieldModel{chgField("a", "string")}, nil, func(txn tableTransaction) error {
			return ApplySchemaChanges(txn, []FieldModel{chgField("a", "string")}, []FieldModel{want})
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := tbl.Schema().Fields()[0].Doc; got != "new doc" {
			t.Errorf("doc = %q", got)
		}
	})

	t.Run("doc_change_on_list_column", func(t *testing.T) {
		cur := []FieldModel{chgListField("tags")}
		want := chgListField("tags")
		want.Doc = types.StringValue("tags doc")
		tbl, err := stagedTable(t, cur, nil, func(txn tableTransaction) error {
			return ApplySchemaChanges(txn, cur, []FieldModel{want})
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := tbl.Schema().Fields()[0].Doc; got != "tags doc" {
			t.Errorf("doc = %q", got)
		}
	})

	t.Run("type_promotion", func(t *testing.T) {
		cur := []FieldModel{chgField("n", "int")}
		tbl, err := stagedTable(t, cur, nil, func(txn tableTransaction) error {
			return ApplySchemaChanges(txn, cur, []FieldModel{chgField("n", "long")})
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := tbl.Schema().Fields()[0].Type.String(); got != "long" {
			t.Errorf("type = %s", got)
		}
	})

	t.Run("insert_in_middle_matches_config_order", func(t *testing.T) {
		cur := []FieldModel{chgField("a", "string"), chgField("b", "string")}
		want := []FieldModel{chgField("a", "string"), chgField("x", "string"), chgField("b", "string"), chgField("y", "string")}
		tbl, err := stagedTable(t, cur, nil, func(txn tableTransaction) error {
			return ApplySchemaChanges(txn, cur, want)
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := schemaNames(tbl); !slices.Equal(got, chgNames(want)) {
			t.Errorf("column order = %v, want %v", got, chgNames(want))
		}
	})

	t.Run("many_adds_match_config_order", func(t *testing.T) {
		cur := []FieldModel{chgField("id", "long")}
		want := slices.Clone(cur)
		for i := range 10 {
			want = append(want, chgField(fmt.Sprintf("c%d", i), "string"))
		}
		for range 20 {
			tbl, err := stagedTable(t, cur, nil, func(txn tableTransaction) error {
				return ApplySchemaChanges(txn, cur, want)
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := schemaNames(tbl); !slices.Equal(got, chgNames(want)) {
				t.Fatalf("column order = %v, want %v", got, chgNames(want))
			}
		}
	})

	t.Run("reorder_only", func(t *testing.T) {
		cur := []FieldModel{chgField("a", "string"), chgField("b", "string"), chgField("c", "string")}
		want := []FieldModel{chgField("c", "string"), chgField("a", "string"), chgField("b", "string")}
		tbl, err := stagedTable(t, cur, nil, func(txn tableTransaction) error {
			return ApplySchemaChanges(txn, cur, want)
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := schemaNames(tbl); !slices.Equal(got, chgNames(want)) {
			t.Errorf("column order = %v, want %v", got, chgNames(want))
		}
	})

	t.Run("delete_add_and_reorder_together", func(t *testing.T) {
		cur := []FieldModel{chgField("a", "string"), chgField("b", "string"), chgField("c", "string")}
		want := []FieldModel{chgField("new", "long"), chgField("c", "string"), chgField("a", "string")}
		tbl, err := stagedTable(t, cur, nil, func(txn tableTransaction) error {
			return ApplySchemaChanges(txn, cur, want)
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := schemaNames(tbl); !slices.Equal(got, chgNames(want)) {
			t.Errorf("column order = %v, want %v", got, chgNames(want))
		}
	})

	t.Run("revert_to_an_earlier_schema", func(t *testing.T) {
		// Dropping a column that was added earlier recreates schema 0. The
		// update must resolve to the existing schema rather than assume a new ID.
		first := []FieldModel{chgField("a", "string")}
		second := []FieldModel{chgField("a", "string"), chgField("b", "string")}
		tbl := buildTestTable(t, first, nil)
		txn := tbl.NewTransaction()
		if err := ApplySchemaChanges(&txnAdapter{txn}, first, second); err != nil {
			t.Fatal(err)
		}
		if err := ApplySchemaChanges(&txnAdapter{txn}, second, first); err != nil {
			t.Fatal(err)
		}
		staged, err := txn.StagedTable()
		if err != nil {
			t.Fatal(err)
		}
		if got := schemaNames(staged.Table); !slices.Equal(got, []string{"a"}) {
			t.Errorf("column order = %v", got)
		}
	})
}

func TestApplyPartitionChanges_RealBuilder(t *testing.T) {
	fields := []FieldModel{
		chgField("id", "long"), chgField("ts", "timestamptz"), chgField("region", "string"),
	}
	p := func(src, transform, name string) PartitionModel {
		return PartitionModel{
			SourceName: types.StringValue(src), Transform: types.StringValue(transform), Name: types.StringValue(name),
		}
	}

	t.Run("adds_follow_config_order", func(t *testing.T) {
		want := []PartitionModel{p("region", "identity", "region"), p("ts", "day", "day"), p("id", "bucket[8]", "id_bucket")}
		for range 20 {
			tbl, err := stagedTable(t, fields, nil, func(txn tableTransaction) error {
				return ApplyPartitionChanges(txn, nil, want)
			})
			if err != nil {
				t.Fatal(err)
			}
			got := specToPartitionModels(tbl.Spec(), tbl.Schema())
			if !slices.Equal(partitionNames(got), partitionNames(want)) {
				t.Fatalf("partition order = %v, want %v", partitionNames(got), partitionNames(want))
			}
		}
	})

	t.Run("change_transform_of_existing_field", func(t *testing.T) {
		cur := []PartitionModel{p("ts", "year", "ts_part")}
		want := []PartitionModel{p("ts", "month", "ts_part")}
		tbl, err := stagedTable(t, fields, cur, func(txn tableTransaction) error {
			return ApplyPartitionChanges(txn, cur, want)
		})
		if err != nil {
			t.Fatal(err)
		}
		got := specToPartitionModels(tbl.Spec(), tbl.Schema())
		if len(got) != 1 || got[0].Transform.ValueString() != "month" || got[0].Name.ValueString() != "ts_part" {
			t.Errorf("partitions = %+v", got)
		}
	})

	t.Run("duplicate_names_rejected", func(t *testing.T) {
		mock := &mockPartitionUpdater{}
		err := ApplyPartitionChanges(&mockTransaction{partition: mock}, nil,
			[]PartitionModel{p("ts", "day", "x"), p("id", "identity", "x")})
		if err == nil || !strings.Contains(err.Error(), "declared more than once") {
			t.Errorf("expected duplicate error, got %v", err)
		}
	})
}

func partitionNames(ps []PartitionModel) []string {
	var names []string
	for _, p := range ps {
		names = append(names, p.Name.ValueString())
	}
	return names
}

// ── equality ──────────────────────────────────────────────────────────────────

func TestFieldModelsEqual_NumericDefaults(t *testing.T) {
	// A number decoded from Terraform's wire format has 64-bit precision while one
	// built from the catalog's float64 has 53 bits. They are the same number.
	fromWire := new(big.Float).SetInt64(5)
	fromCatalog := big.NewFloat(5)
	if fromWire.Prec() == fromCatalog.Prec() {
		t.Fatal("test precondition: precisions should differ")
	}
	a := chgField("n", "int")
	a.DefaultNumber = types.NumberValue(fromWire)
	b := chgField("n", "int")
	b.DefaultNumber = types.NumberValue(fromCatalog)

	if !fieldModelsEqual(a, b) {
		t.Error("identical numeric defaults must compare equal")
	}
	if !fieldsEqual([]FieldModel{a}, []FieldModel{b}) {
		t.Error("fieldsEqual must compare numeric defaults by value")
	}

	b.DefaultNumber = types.NumberValue(big.NewFloat(6))
	if fieldModelsEqual(a, b) {
		t.Error("different numeric defaults must not compare equal")
	}

	plan, err := planSchemaChanges([]FieldModel{a}, []FieldModel{{
		Name: a.Name, Type: a.Type, Required: a.Required, Doc: a.Doc, DefaultString: a.DefaultString,
		DefaultBool: a.DefaultBool, DefaultNumber: types.NumberValue(big.NewFloat(5)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.empty() {
		t.Errorf("an unchanged numeric default must not produce an update: %+v", plan)
	}
}

func TestAlignPartitionOrder(t *testing.T) {
	p := func(name string) PartitionModel {
		return PartitionModel{
			SourceName: types.StringValue("c"), Transform: types.StringValue("identity"), Name: types.StringValue(name),
		}
	}
	got := []PartitionModel{p("a"), p("b"), p("c")}

	if names := partitionNames(alignPartitionOrder(got, []PartitionModel{p("c"), p("a"), p("b")})); !slices.Equal(names, []string{"c", "a", "b"}) {
		t.Errorf("same set must follow prior order, got %v", names)
	}
	if names := partitionNames(alignPartitionOrder(got, []PartitionModel{p("c"), p("a")})); !slices.Equal(names, []string{"a", "b", "c"}) {
		t.Errorf("a different set must keep the table order, got %v", names)
	}
	if names := partitionNames(alignPartitionOrder(got, nil)); !slices.Equal(names, []string{"a", "b", "c"}) {
		t.Errorf("no prior must keep the table order, got %v", names)
	}
}
