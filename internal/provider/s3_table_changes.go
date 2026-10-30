// Copyright BrightAI 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"errors"
	"fmt"
	"slices"

	iceberg "github.com/apache/iceberg-go"
	itable "github.com/apache/iceberg-go/table"
)

// Table updates are done in two steps: first the complete set of changes is
// computed and validated without touching the transaction (planSchemaChanges,
// planPartitionChanges), then the validated plan is staged on the transaction
// and committed once. A change that Iceberg cannot express is therefore
// reported before anything is staged, with an error that names the column.
//
// The staged changes are sent to the catalog as a single CommitTable call. The
// iceberg-go update builders attach the optimistic-concurrency requirements
// (assert-table-uuid, assert-current-schema-id, assert-last-assigned-partition-id)
// so a concurrent writer makes the commit fail instead of being overwritten.

// errNestedTypeUpdate is returned when a list, map or struct column differs from
// the table in a way that cannot be applied in place.
var errNestedTypeUpdate = errors.New("cannot update field type for non-primitive type")

// columnAdd is a validated AddColumn operation.
type columnAdd struct {
	name     string
	typ      iceberg.Type
	doc      string
	required bool
	def      iceberg.Literal
}

// columnUpdate is a validated UpdateColumn operation. Only the attributes that
// actually changed are marked valid in update.
type columnUpdate struct {
	name   string
	update itable.ColumnUpdate
}

// schemaChangePlan is the validated difference between the table's current
// columns and the desired ones.
type schemaChangePlan struct {
	deletes []string
	updates []columnUpdate
	adds    []columnAdd
	// order is the desired top-level column order. It is only set when the
	// order the table would end up with differs from it.
	order []string
}

func (p *schemaChangePlan) empty() bool {
	return len(p.deletes) == 0 && len(p.updates) == 0 && len(p.adds) == 0 && p.order == nil
}

// planSchemaChanges diffs current against desired, both in column order, and
// validates every resulting operation.
func planSchemaChanges(current, desired []FieldModel) (*schemaChangePlan, error) {
	currentByName := make(map[string]FieldModel, len(current))
	for _, f := range current {
		currentByName[f.Name.ValueString()] = f
	}
	desiredNames := make(map[string]struct{}, len(desired))
	for _, f := range desired {
		name := f.Name.ValueString()
		if _, dup := desiredNames[name]; dup {
			return nil, fmt.Errorf("field %q is declared more than once", name)
		}
		desiredNames[name] = struct{}{}
	}

	plan := &schemaChangePlan{}

	// Columns the table would end up with, in order: surviving columns keep their
	// position and added columns are appended.
	resulting := make([]string, 0, len(desired))
	for _, f := range current {
		name := f.Name.ValueString()
		if _, keep := desiredNames[name]; keep {
			resulting = append(resulting, name)
		} else {
			plan.deletes = append(plan.deletes, name)
		}
	}

	for _, pf := range desired {
		name := pf.Name.ValueString()
		cf, exists := currentByName[name]
		if !exists {
			add, err := planColumnAdd(pf)
			if err != nil {
				return nil, err
			}
			plan.adds = append(plan.adds, *add)
			resulting = append(resulting, name)
			continue
		}
		if fieldModelsEqual(cf, pf) {
			continue
		}
		upd, err := planColumnUpdate(cf, pf)
		if err != nil {
			return nil, err
		}
		if upd != nil {
			plan.updates = append(plan.updates, *upd)
		}
	}

	desiredOrder := make([]string, 0, len(desired))
	for _, f := range desired {
		desiredOrder = append(desiredOrder, f.Name.ValueString())
	}
	if !slices.Equal(resulting, desiredOrder) {
		plan.order = desiredOrder
	}
	return plan, nil
}

// fieldDefaultLiteral returns the write default of f as an Iceberg literal, or
// nil when no default is configured.
func fieldDefaultLiteral(f FieldModel) (iceberg.Literal, error) {
	if f.Type.IsNull() || f.Type.IsUnknown() {
		return nil, nil
	}
	dv, err := f.getFieldDefault()
	if err != nil {
		return nil, err
	}
	return anyToIcebergLit(f.Type.ValueString(), dv)
}

func planColumnAdd(pf FieldModel) (*columnAdd, error) {
	name := pf.Name.ValueString()
	typ, err := fieldModelToIcebergType(pf)
	if err != nil {
		return nil, fmt.Errorf("field %q: %w", name, err)
	}
	def, err := fieldDefaultLiteral(pf)
	if err != nil {
		return nil, fmt.Errorf("field %q: %w", name, err)
	}
	if pf.Required.ValueBool() && def == nil {
		return nil, fmt.Errorf(
			"field %q: a required column cannot be added to an existing table without a default value,"+
				" because existing rows would have no value for it. Add a default_* attribute"+
				" (requires format_version 3) or declare the column as not required", name)
	}
	return &columnAdd{
		name:     name,
		typ:      typ,
		doc:      pf.Doc.ValueString(),
		required: pf.Required.ValueBool(),
		def:      def,
	}, nil
}

// fieldKind classifies a field as "primitive", "list", "map" or "struct".
func fieldKind(f FieldModel) string {
	switch {
	case f.ListType != nil:
		return "list"
	case f.MapType != nil:
		return "map"
	case f.StructType != nil:
		return "struct"
	default:
		return "primitive"
	}
}

// planColumnUpdate validates the change from cf to pf and returns an update that
// carries only the attributes that differ. It returns nil when nothing needs to
// be sent to the catalog.
//
// Sending only the changed attributes matters: iceberg-go rejects any update
// that carries a field type for a list, map or struct column, and v0.6.0
// panics on an update that carries a nil write default.
func planColumnUpdate(cf, pf FieldModel) (*columnUpdate, error) {
	name := pf.Name.ValueString()
	var update itable.ColumnUpdate

	if fieldKind(cf) != fieldKind(pf) {
		return nil, fmt.Errorf(
			"field %q: cannot change a column from %s to %s in place; remove the field and add it again"+
				" (the column's data is lost)", name, fieldKind(cf), fieldKind(pf))
	}

	if fieldKind(pf) != "primitive" {
		if !nestedTypesEqual(cf, pf) {
			return nil, fmt.Errorf("%w: %s", errNestedTypeUpdate, name)
		}
	} else if !cf.Type.Equal(pf.Type) {
		curType, err := parseIcebergType(cf.Type.ValueString())
		if err != nil {
			return nil, fmt.Errorf("field %q: current type: %w", name, err)
		}
		newType, err := parseIcebergType(pf.Type.ValueString())
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", name, err)
		}
		if !curType.Equals(newType) {
			curPrim, curOK := curType.(iceberg.PrimitiveType)
			newPrim, newOK := newType.(iceberg.PrimitiveType)
			if !curOK || !newOK {
				return nil, fmt.Errorf("field %q: unsupported type change from %s to %s", name, curType, newType)
			}
			if _, err := iceberg.PromoteType(curPrim, newPrim); err != nil {
				return nil, fmt.Errorf(
					"field %q: %s is not a valid type promotion from %s; Iceberg only allows int to long,"+
						" float to double and widening decimal precision. Remove the field and add it again"+
						" to use a different type (the column's data is lost): %w",
					name, newType, curType, err)
			}
			update.FieldType = iceberg.Optional[iceberg.Type]{Valid: true, Val: newType}
		}
	}

	if !cf.Required.Equal(pf.Required) {
		if !cf.Required.ValueBool() && pf.Required.ValueBool() {
			return nil, fmt.Errorf(
				"field %q: changing a column from optional to required is not a valid Iceberg schema change"+
					" because existing rows may contain nulls", name)
		}
		update.Required = iceberg.Optional[bool]{Valid: true, Val: pf.Required.ValueBool()}
	}

	if !cf.Doc.Equal(pf.Doc) {
		update.Doc = iceberg.Optional[string]{Valid: true, Val: pf.Doc.ValueString()}
	}

	if !cf.DefaultString.Equal(pf.DefaultString) ||
		!cf.DefaultNumber.Equal(pf.DefaultNumber) ||
		!cf.DefaultBool.Equal(pf.DefaultBool) {
		def, err := fieldDefaultLiteral(pf)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", name, err)
		}
		if def == nil {
			return nil, fmt.Errorf(
				"field %q: removing a column default is not supported. Keep the default_* attribute,"+
					" or remove the field and add it again (the column's data is lost)", name)
		}
		update.WriteDefault = iceberg.Optional[iceberg.Literal]{Valid: true, Val: def}
	}

	if !update.FieldType.Valid && !update.Required.Valid && !update.Doc.Valid && !update.WriteDefault.Valid {
		return nil, nil
	}
	return &columnUpdate{name: name, update: update}, nil
}

// nestedTypesEqual compares the list, map or struct definition of two fields of
// the same kind, including nested IDs.
func nestedTypesEqual(a, b FieldModel) bool {
	switch {
	case a.ListType != nil:
		return a.ListType.ID.Equal(b.ListType.ID) &&
			a.ListType.ElementType.Equal(b.ListType.ElementType) &&
			a.ListType.Required.Equal(b.ListType.Required)
	case a.MapType != nil:
		return a.MapType.KeyID.Equal(b.MapType.KeyID) &&
			a.MapType.ValueID.Equal(b.MapType.ValueID) &&
			a.MapType.KeyType.Equal(b.MapType.KeyType) &&
			a.MapType.ValueType.Equal(b.MapType.ValueType) &&
			a.MapType.Required.Equal(b.MapType.Required)
	case a.StructType != nil:
		return slices.EqualFunc(a.StructType.Fields, b.StructType.Fields,
			func(x, y StructSubFieldModel) bool {
				return x.ID.Equal(y.ID) && x.Name.Equal(y.Name) && x.Type.Equal(y.Type) &&
					x.Required.Equal(y.Required) && x.Doc.Equal(y.Doc)
			})
	}
	return true
}

// ApplySchemaChanges computes the difference between the table's current fields
// and the planned fields, validates it, and stages it on the transaction.
//
// Columns are matched by name; a rename is a delete plus an add. Operations are
// applied in a fixed order (deletes, updates, adds, then moves) and added
// columns follow the order of the configuration, so the result does not depend
// on map iteration order. When the resulting column order would differ from the
// configuration the columns are moved into configuration order, which is a
// metadata-only change.
func ApplySchemaChanges(txn tableTransaction, currentFields, planFields []FieldModel) error {
	plan, err := planSchemaChanges(currentFields, planFields)
	if err != nil {
		return err
	}
	if plan.empty() {
		return nil
	}

	updater := txn.UpdateSchema(true, false)
	for _, name := range plan.deletes {
		updater.DeleteColumn([]string{name})
	}
	for _, u := range plan.updates {
		updater.UpdateColumn([]string{u.name}, u.update)
	}
	for _, a := range plan.adds {
		updater.AddColumn([]string{a.name}, a.typ, a.doc, a.required, a.def)
	}
	for i, name := range plan.order {
		if i == 0 {
			updater.MoveFirst([]string{name})
		} else {
			updater.MoveAfter([]string{name}, []string{plan.order[i-1]})
		}
	}
	return updater.Commit()
}

// partitionChangePlan is the validated difference between the table's current
// partition fields and the desired ones.
type partitionChangePlan struct {
	removes []string
	adds    []partitionAdd
}

type partitionAdd struct {
	sourceName string
	transform  iceberg.Transform
	name       string
}

// planPartitionChanges diffs current against desired, both in order. A partition
// field whose source column or transform changed is removed and added again
// under the same name.
func planPartitionChanges(current, desired []PartitionModel) (*partitionChangePlan, error) {
	currentByName := make(map[string]PartitionModel, len(current))
	for _, p := range current {
		currentByName[p.Name.ValueString()] = p
	}
	desiredByName := make(map[string]PartitionModel, len(desired))
	for _, p := range desired {
		name := p.Name.ValueString()
		if _, dup := desiredByName[name]; dup {
			return nil, fmt.Errorf("partition %q is declared more than once", name)
		}
		desiredByName[name] = p
	}

	plan := &partitionChangePlan{}
	for _, cp := range current {
		name := cp.Name.ValueString()
		if pp, keep := desiredByName[name]; !keep || cp != pp {
			plan.removes = append(plan.removes, name)
		}
	}
	for _, pp := range desired {
		name := pp.Name.ValueString()
		if cp, exists := currentByName[name]; exists && cp == pp {
			continue
		}
		transform, err := iceberg.ParseTransform(pp.Transform.ValueString())
		if err != nil {
			return nil, fmt.Errorf("partition %q: %w", name, err)
		}
		plan.adds = append(plan.adds, partitionAdd{
			sourceName: pp.SourceName.ValueString(),
			transform:  transform,
			name:       name,
		})
	}
	return plan, nil
}

// ApplyPartitionChanges computes the difference between the table's current
// partition fields and the planned ones, validates it, and stages it on the
// transaction. Fields are removed first, then added in configuration order.
func ApplyPartitionChanges(txn tableTransaction, currentPartitions, planPartitions []PartitionModel) error {
	plan, err := planPartitionChanges(currentPartitions, planPartitions)
	if err != nil {
		return err
	}
	if len(plan.removes) == 0 && len(plan.adds) == 0 {
		return nil
	}

	updater := txn.UpdateSpec(true)
	for _, name := range plan.removes {
		updater.RemoveField(name)
	}
	for _, a := range plan.adds {
		updater.AddField(a.sourceName, a.transform, a.name)
	}
	return updater.Commit()
}

// alignPartitionOrder returns got in the order of prior when both hold the same
// active partition fields. The order of partition fields in an Iceberg spec
// cannot be rearranged, so a configuration that lists them in a different
// order than the table would otherwise show a permanent diff.
func alignPartitionOrder(got, prior []PartitionModel) []PartitionModel {
	if len(prior) == 0 || !partitionsMatch(prior, got) {
		return got
	}
	byName := make(map[string]PartitionModel, len(got))
	for _, p := range got {
		byName[p.Name.ValueString()] = p
	}
	aligned := make([]PartitionModel, 0, len(got))
	for _, p := range prior {
		aligned = append(aligned, byName[p.Name.ValueString()])
	}
	return aligned
}
