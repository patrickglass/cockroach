// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package scmutationexec

import (
	"context"
	"sort"

	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/dbdesc"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/schemadesc"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/seqexpr"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/typedesc"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scexec/descriptorutils"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scop"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/screl"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/log/eventpb"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/errors"
)

// CatalogReader describes catalog read operations as required by the mutation
// visitor.
type CatalogReader interface {
	// MustReadImmutableDescriptor reads a descriptor from the catalog by ID.
	MustReadImmutableDescriptor(ctx context.Context, id descpb.ID) (catalog.Descriptor, error)

	// GetFullyQualifiedName gets the fully qualified name from a descriptor ID.
	GetFullyQualifiedName(ctx context.Context, id descpb.ID) (string, error)

	// AddSyntheticDescriptor adds a synthetic descriptor to the reader state.
	// Subsequent calls to MustReadImmutableDescriptor for this ID will return
	// this synthetic descriptor instead of what it would have otherwise returned.
	AddSyntheticDescriptor(desc catalog.Descriptor)

	// RemoveSyntheticDescriptor undoes the effects of AddSyntheticDescriptor.
	RemoveSyntheticDescriptor(id descpb.ID)

	// AddPartitioning adds partitioning information on an index descriptor.
	AddPartitioning(
		tableDesc *tabledesc.Mutable,
		indexDesc *descpb.IndexDescriptor,
		partitionFields []string,
		listPartition []*scpb.ListPartition,
		rangePartition []*scpb.RangePartitions,
		allowedNewColumnNames []tree.Name,
		allowImplicitPartitioning bool,
	) error
}

// MutationVisitorStateUpdater is the interface for updating the visitor state.
type MutationVisitorStateUpdater interface {

	// CheckOutDescriptor reads a descriptor from the catalog by ID and marks it
	// as undergoing a change.
	CheckOutDescriptor(ctx context.Context, id descpb.ID) (catalog.MutableDescriptor, error)

	// AddDrainedName marks a namespace entry as being drained.
	AddDrainedName(id descpb.ID, nameInfo descpb.NameInfo)

	// AddNewGCJobForDescriptor enqueues a GC job for the given descriptor.
	AddNewGCJobForDescriptor(descriptor catalog.Descriptor)
}

// EventLogWriter encapsulates operations for generating
// event log entries.
type EventLogWriter interface {
	AddDropEvent(
		ctx context.Context,
		descID descpb.ID,
		metadata *scpb.ElementMetadata,
		event eventpb.EventPayload,
	) error
}

// NewMutationVisitor creates a new scop.MutationVisitor.
func NewMutationVisitor(
	cr CatalogReader, s MutationVisitorStateUpdater, ev EventLogWriter,
) scop.MutationVisitor {
	return &visitor{
		cr: cr,
		s:  s,
		ev: ev,
	}
}

type visitor struct {
	cr CatalogReader
	s  MutationVisitorStateUpdater
	ev EventLogWriter
}

func (m *visitor) checkOutTable(ctx context.Context, id descpb.ID) (*tabledesc.Mutable, error) {
	desc, err := m.s.CheckOutDescriptor(ctx, id)
	if err != nil {
		return nil, err
	}
	mut, ok := desc.(*tabledesc.Mutable)
	if !ok {
		return nil, catalog.WrapTableDescRefErr(id, catalog.NewDescriptorTypeError(desc))
	}
	return mut, nil
}

func (m *visitor) checkOutDatabase(ctx context.Context, id descpb.ID) (*dbdesc.Mutable, error) {
	desc, err := m.s.CheckOutDescriptor(ctx, id)
	if err != nil {
		return nil, err
	}
	mut, ok := desc.(*dbdesc.Mutable)
	if !ok {
		return nil, catalog.WrapDatabaseDescRefErr(id, catalog.NewDescriptorTypeError(desc))
	}
	return mut, nil
}

// Stop the linter from complaining.
var _ = ((*visitor)(nil)).checkOutDatabase

func (m *visitor) checkOutSchema(ctx context.Context, id descpb.ID) (*schemadesc.Mutable, error) {
	desc, err := m.s.CheckOutDescriptor(ctx, id)
	if err != nil {
		return nil, err
	}
	mut, ok := desc.(*schemadesc.Mutable)
	if !ok {
		return nil, catalog.WrapSchemaDescRefErr(id, catalog.NewDescriptorTypeError(desc))
	}
	return mut, nil
}

// Stop the linter from complaining.
var _ = ((*visitor)(nil)).checkOutSchema

func (m *visitor) checkOutType(ctx context.Context, id descpb.ID) (*typedesc.Mutable, error) {
	desc, err := m.s.CheckOutDescriptor(ctx, id)
	if err != nil {
		return nil, err
	}
	mut, ok := desc.(*typedesc.Mutable)
	if !ok {
		return nil, catalog.WrapTypeDescRefErr(id, catalog.NewDescriptorTypeError(desc))
	}
	return mut, nil
}

func (m *visitor) MakeAddedColumnDeleteAndWriteOnly(
	ctx context.Context, op scop.MakeAddedColumnDeleteAndWriteOnly,
) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	return mutationStateChange(
		ctx,
		tbl,
		descriptorutils.MakeColumnIDMutationSelector(op.ColumnID),
		descpb.DescriptorMutation_DELETE_ONLY,
		descpb.DescriptorMutation_DELETE_AND_WRITE_ONLY,
	)
}

func (m *visitor) UpdateRelationDeps(ctx context.Context, op scop.UpdateRelationDeps) error {
	// TODO(fqazi): Only implemented for sequences.
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	// Determine all the dependencies for this descriptor.
	dependedOnBy := make([]descpb.TableDescriptor_Reference, len(tbl.DependedOnBy))
	addDependency := func(dep descpb.TableDescriptor_Reference) {
		for _, existingDep := range dependedOnBy {
			if dep.Equal(existingDep) {
				return
			}
			dependedOnBy = append(dependedOnBy, dep)
		}
	}
	for _, col := range tbl.Columns {
		sequenceRefByID := true
		// Parse the default expression to determine
		// if all references are by ID.
		if col.DefaultExpr != nil && len(col.UsesSequenceIds) > 0 {
			expr, err := parser.ParseExpr(*col.DefaultExpr)
			if err != nil {
				return err
			}
			usedSequences, err := seqexpr.GetUsedSequences(expr)
			if err != nil {
				return err
			}
			if len(usedSequences) > 0 {
				sequenceRefByID = usedSequences[0].IsByID()
			}
		}
		for _, seqID := range col.UsesSequenceIds {
			addDependency(descpb.TableDescriptor_Reference{
				ID:        seqID,
				ColumnIDs: []descpb.ColumnID{col.ID},
				ByID:      sequenceRefByID,
			})
		}
	}
	tbl.DependedOnBy = dependedOnBy
	return nil
}

func (m *visitor) RemoveColumnDefaultExpression(
	ctx context.Context, op scop.RemoveColumnDefaultExpression,
) error {
	// Remove the descriptors namespaces as the last stage
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	column, err := tbl.FindColumnWithID(op.ColumnID)
	if err != nil {
		return err
	}

	// Clean up the default expression and the sequence ID's
	column.ColumnDesc().DefaultExpr = nil
	column.ColumnDesc().UsesSequenceIds = nil
	return nil
}

func (m *visitor) AddTypeBackRef(ctx context.Context, op scop.AddTypeBackRef) error {
	typ, err := m.checkOutType(ctx, op.TypeID)
	if err != nil {
		return err
	}
	typ.AddReferencingDescriptorID(op.DescID)
	// Sanity: Validate that a back reference exists by now.
	desc, err := m.cr.MustReadImmutableDescriptor(ctx, op.DescID)
	if err != nil {
		return err
	}
	refDescIDs, err := desc.GetReferencedDescIDs()
	if err != nil {
		return err
	}
	if !refDescIDs.Contains(op.TypeID) {
		return errors.AssertionFailedf("Back reference for type %d is missing inside descriptor %d.",
			op.TypeID, op.DescID)
	}
	return nil
}

func (m *visitor) RemoveRelationDependedOnBy(
	ctx context.Context, op scop.RemoveRelationDependedOnBy,
) error {
	// Remove the descriptors namespaces as the last stage
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	for depIdx, dependedOnBy := range tbl.DependedOnBy {
		if dependedOnBy.ID == op.DependedOnBy {
			tbl.DependedOnBy = append(tbl.DependedOnBy[:depIdx], tbl.DependedOnBy[depIdx+1:]...)
			break
		}
	}
	if len(tbl.DependedOnBy) == 0 {
		tbl.DependedOnBy = nil
	}
	return nil
}

func (m *visitor) RemoveSequenceOwnedBy(ctx context.Context, op scop.RemoveSequenceOwnedBy) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	tbl.GetSequenceOpts().SequenceOwner.OwnerTableID = descpb.InvalidID
	tbl.GetSequenceOpts().SequenceOwner.OwnerColumnID = 0
	return nil
}

func (m *visitor) RemoveTypeBackRef(ctx context.Context, op scop.RemoveTypeBackRef) error {
	typ, err := m.checkOutType(ctx, op.TypeID)
	if err != nil {
		return err
	}
	typ.RemoveReferencingDescriptorID(op.DescID)
	return nil
}

func (m *visitor) CreateGcJobForDescriptor(
	ctx context.Context, op scop.CreateGcJobForDescriptor,
) error {
	desc, err := m.cr.MustReadImmutableDescriptor(ctx, op.DescID)
	if err != nil {
		return err
	}
	m.s.AddNewGCJobForDescriptor(desc)
	return nil
}

func (m *visitor) MarkDescriptorAsDropped(
	ctx context.Context, op scop.MarkDescriptorAsDropped,
) error {
	// Before we can mutate the descriptor, get rid of any synthetic descriptor.
	m.cr.RemoveSyntheticDescriptor(op.DescID)
	desc, err := m.s.CheckOutDescriptor(ctx, op.DescID)
	if err != nil {
		return err
	}
	desc.SetDropped()
	return nil
}

func (m *visitor) MarkDescriptorAsDroppedSynthetically(
	ctx context.Context, op scop.MarkDescriptorAsDroppedSynthetically,
) error {
	desc, err := m.cr.MustReadImmutableDescriptor(ctx, op.DescID)
	if err != nil {
		return err
	}
	mut := desc.NewBuilder().BuildCreatedMutable()
	mut.SetDropped()
	m.cr.AddSyntheticDescriptor(mut)
	return nil
}

func (m *visitor) DrainDescriptorName(ctx context.Context, op scop.DrainDescriptorName) error {
	descriptor, err := m.cr.MustReadImmutableDescriptor(ctx, op.TableID)
	if err != nil {
		return err
	}
	// Queue up names for draining.
	nameDetails := descpb.NameInfo{
		ParentID:       descriptor.GetParentID(),
		ParentSchemaID: descriptor.GetParentSchemaID(),
		Name:           descriptor.GetName()}
	m.s.AddDrainedName(descriptor.GetID(), nameDetails)
	return nil
}

func (m *visitor) MakeColumnPublic(ctx context.Context, op scop.MakeColumnPublic) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	mut, err := removeMutation(
		ctx,
		tbl,
		descriptorutils.MakeColumnIDMutationSelector(op.ColumnID),
		descpb.DescriptorMutation_DELETE_AND_WRITE_ONLY,
	)
	if err != nil {
		return err
	}
	// TODO(ajwerner): Should the op just have the column descriptor? What's the
	// type hydration status here? Cloning is going to blow away hydration. Is
	// that okay?
	tbl.Columns = append(tbl.Columns,
		*(protoutil.Clone(mut.GetColumn())).(*descpb.ColumnDescriptor))
	return nil
}

func (m *visitor) MakeDroppedNonPrimaryIndexDeleteAndWriteOnly(
	ctx context.Context, op scop.MakeDroppedNonPrimaryIndexDeleteAndWriteOnly,
) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	var idx descpb.IndexDescriptor
	for i := range tbl.Indexes {
		if tbl.Indexes[i].ID != op.IndexID {
			continue
		}
		idx = tbl.Indexes[i]
		tbl.Indexes = append(tbl.Indexes[:i], tbl.Indexes[i+1:]...)
		break
	}
	if len(tbl.Indexes) == 0 {
		tbl.Indexes = nil
	}
	if idx.ID == 0 {
		return errors.AssertionFailedf("failed to find index %d in descriptor %v",
			op.IndexID, tbl)
	}
	return tbl.AddIndexMutation(&idx, descpb.DescriptorMutation_DROP)
}

func (m *visitor) MakeDroppedColumnDeleteAndWriteOnly(
	ctx context.Context, op scop.MakeDroppedColumnDeleteAndWriteOnly,
) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	var col descpb.ColumnDescriptor
	for i := range tbl.Columns {
		if tbl.Columns[i].ID != op.ColumnID {
			continue
		}
		col = tbl.Columns[i]
		tbl.Columns = append(tbl.Columns[:i], tbl.Columns[i+1:]...)
		break
	}
	if len(tbl.Columns) == 0 {
		tbl.Columns = nil
	}
	if col.ID == 0 {
		return errors.AssertionFailedf("failed to find column %d in %v", col.ID, tbl)
	}
	tbl.AddColumnMutation(&col, descpb.DescriptorMutation_DROP)
	return nil
}

func (m *visitor) MakeDroppedColumnDeleteOnly(
	ctx context.Context, op scop.MakeDroppedColumnDeleteOnly,
) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	return mutationStateChange(
		ctx,
		tbl,
		descriptorutils.MakeColumnIDMutationSelector(op.ColumnID),
		descpb.DescriptorMutation_DELETE_AND_WRITE_ONLY,
		descpb.DescriptorMutation_DELETE_ONLY,
	)
}

func (m *visitor) MakeColumnAbsent(ctx context.Context, op scop.MakeColumnAbsent) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	mut, err := removeMutation(
		ctx,
		tbl,
		descriptorutils.MakeColumnIDMutationSelector(op.ColumnID),
		descpb.DescriptorMutation_DELETE_ONLY,
	)
	if err != nil {
		return err
	}
	col := mut.GetColumn()
	tbl.RemoveColumnFromFamilyAndPrimaryIndex(col.ID)
	return nil
}

func (m *visitor) MakeAddedIndexDeleteAndWriteOnly(
	ctx context.Context, op scop.MakeAddedIndexDeleteAndWriteOnly,
) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	return mutationStateChange(
		ctx,
		tbl,
		descriptorutils.MakeIndexIDMutationSelector(op.IndexID),
		descpb.DescriptorMutation_DELETE_ONLY,
		descpb.DescriptorMutation_DELETE_AND_WRITE_ONLY,
	)
}

func (m *visitor) MakeAddedColumnDeleteOnly(
	ctx context.Context, op scop.MakeAddedColumnDeleteOnly,
) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	// TODO(ajwerner): deal with ordering the indexes or sanity checking this
	// or what-not.
	if op.Column.ID >= tbl.NextColumnID {
		tbl.NextColumnID = op.Column.ID + 1
	}
	if !op.Column.IsComputed() ||
		!op.Column.Virtual {
		var foundFamily bool
		for i := range tbl.Families {
			fam := &tbl.Families[i]
			if foundFamily = fam.ID == op.FamilyID; foundFamily {
				fam.ColumnIDs = append(fam.ColumnIDs, op.Column.ID)
				fam.ColumnNames = append(fam.ColumnNames, op.Column.Name)
				break
			}
		}
		// Only create column families for non-computed columns
		if !foundFamily {
			tbl.Families = append(tbl.Families, descpb.ColumnFamilyDescriptor{
				Name:        op.FamilyName,
				ID:          op.FamilyID,
				ColumnNames: []string{op.Column.Name},
				ColumnIDs:   []descpb.ColumnID{op.Column.ID},
			})
			sort.Slice(tbl.Families, func(i, j int) bool {
				return tbl.Families[i].ID < tbl.Families[j].ID
			})
			if tbl.NextFamilyID <= op.FamilyID {
				tbl.NextFamilyID = op.FamilyID + 1
			}
		}
	}
	tbl.AddColumnMutation(&op.Column, descpb.DescriptorMutation_ADD)
	return nil
}

func (m *visitor) MakeDroppedIndexDeleteOnly(
	ctx context.Context, op scop.MakeDroppedIndexDeleteOnly,
) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	return mutationStateChange(
		ctx,
		tbl,
		descriptorutils.MakeIndexIDMutationSelector(op.IndexID),
		descpb.DescriptorMutation_DELETE_AND_WRITE_ONLY,
		descpb.DescriptorMutation_DELETE_ONLY,
	)
}

func (m *visitor) MakeDroppedPrimaryIndexDeleteAndWriteOnly(
	ctx context.Context, op scop.MakeDroppedPrimaryIndexDeleteAndWriteOnly,
) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	if tbl.PrimaryIndex.ID != op.IndexID {
		return errors.AssertionFailedf("index being dropped (%d) does not match existing primary index (%d).", op.IndexID, tbl.PrimaryIndex.ID)
	}
	idx := protoutil.Clone(&tbl.PrimaryIndex).(*descpb.IndexDescriptor)
	return tbl.AddIndexMutation(idx, descpb.DescriptorMutation_DROP)
}

func (m *visitor) MakeAddedIndexDeleteOnly(
	ctx context.Context, op scop.MakeAddedIndexDeleteOnly,
) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	// TODO(ajwerner): deal with ordering the indexes or sanity checking this
	// or what-not.
	if op.IndexID >= tbl.NextIndexID {
		tbl.NextIndexID = op.IndexID + 1
	}
	// Resolve column names
	colNames, err := columnNamesFromIDs(tbl, op.KeyColumnIDs)
	if err != nil {
		return err
	}
	storeColNames, err := columnNamesFromIDs(tbl, op.StoreColumnIDs)
	if err != nil {
		return err
	}
	// Setup the index descriptor type.
	indexType := descpb.IndexDescriptor_FORWARD
	if op.Inverted {
		indexType = descpb.IndexDescriptor_INVERTED
	}
	// Setup the encoding type.
	encodingType := descpb.PrimaryIndexEncoding
	indexVersion := descpb.PrimaryIndexWithStoredColumnsVersion
	if op.SecondaryIndex {
		encodingType = descpb.SecondaryIndexEncoding
		indexVersion = descpb.StrictIndexColumnIDGuaranteesVersion
	}
	// Create an index descriptor from the the operation.
	idx := &descpb.IndexDescriptor{
		Name:                op.IndexName,
		ID:                  op.IndexID,
		Unique:              op.Unique,
		Version:             indexVersion,
		KeyColumnNames:      colNames,
		KeyColumnIDs:        op.KeyColumnIDs,
		StoreColumnIDs:      op.StoreColumnIDs,
		StoreColumnNames:    storeColNames,
		KeyColumnDirections: op.KeyColumnDirections,
		Type:                indexType,
		KeySuffixColumnIDs:  op.KeySuffixColumnIDs,
		CompositeColumnIDs:  op.CompositeColumnIDs,
		CreatedExplicitly:   true,
		EncodingType:        encodingType,
	}
	if idx.Name == "" {
		name, err := tabledesc.BuildIndexName(tbl, idx)
		if err != nil {
			return err
		}
		idx.Name = name
	}
	if op.ShardedDescriptor != nil {
		idx.Sharded = *op.ShardedDescriptor
	}
	return tbl.AddIndexMutation(idx, descpb.DescriptorMutation_ADD)
}

func (m *visitor) AddCheckConstraint(ctx context.Context, op scop.AddCheckConstraint) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	ck := &descpb.TableDescriptor_CheckConstraint{
		Expr:      op.Expr,
		Name:      op.Name,
		ColumnIDs: op.ColumnIDs,
		Hidden:    op.Hidden,
	}
	if op.Unvalidated {
		ck.Validity = descpb.ConstraintValidity_Unvalidated
	} else {
		ck.Validity = descpb.ConstraintValidity_Validating
	}
	tbl.Checks = append(tbl.Checks, ck)
	return nil
}

func (m *visitor) MakeAddedSecondaryIndexPublic(
	ctx context.Context, op scop.MakeAddedSecondaryIndexPublic,
) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}

	for idx, idxMutation := range tbl.GetMutations() {
		if idxMutation.GetIndex() != nil &&
			idxMutation.GetIndex().ID == op.IndexID {
			err := tbl.MakeMutationComplete(idxMutation)
			if err != nil {
				return err
			}
			tbl.Mutations = append(tbl.Mutations[:idx], tbl.Mutations[idx+1:]...)
			break
		}
	}
	if len(tbl.Mutations) == 0 {
		tbl.Mutations = nil
	}
	return nil
}

func (m *visitor) MakeAddedPrimaryIndexPublic(
	ctx context.Context, op scop.MakeAddedPrimaryIndexPublic,
) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	index, err := tbl.FindIndexWithID(op.IndexID)
	if err != nil {
		return err
	}
	indexDesc := index.IndexDescDeepCopy()
	if _, err := removeMutation(
		ctx,
		tbl,
		descriptorutils.MakeIndexIDMutationSelector(op.IndexID),
		descpb.DescriptorMutation_DELETE_AND_WRITE_ONLY,
	); err != nil {
		return err
	}
	tbl.PrimaryIndex = indexDesc
	return nil
}

func (m *visitor) MakeIndexAbsent(ctx context.Context, op scop.MakeIndexAbsent) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	_, err = removeMutation(ctx,
		tbl,
		descriptorutils.MakeIndexIDMutationSelector(op.IndexID),
		descpb.DescriptorMutation_DELETE_ONLY,
	)
	return err
}

func (m *visitor) AddColumnFamily(ctx context.Context, op scop.AddColumnFamily) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	tbl.AddFamily(op.Family)
	if op.Family.ID >= tbl.NextFamilyID {
		tbl.NextFamilyID = op.Family.ID + 1
	}
	return nil
}

func (m *visitor) DropForeignKeyRef(ctx context.Context, op scop.DropForeignKeyRef) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	fks := tbl.TableDesc().OutboundFKs
	if !op.Outbound {
		fks = tbl.TableDesc().InboundFKs
	}
	newFks := make([]descpb.ForeignKeyConstraint, 0, len(fks)-1)
	for _, fk := range fks {
		if op.Outbound && (fk.OriginTableID != op.TableID ||
			op.Name != fk.Name) {
			newFks = append(newFks, fk)
		} else if !op.Outbound && (fk.ReferencedTableID != op.TableID ||
			op.Name != fk.Name) {
			newFks = append(newFks, fk)
		}
	}
	if op.Outbound {
		tbl.TableDesc().OutboundFKs = newFks
	} else {
		tbl.TableDesc().InboundFKs = newFks
	}
	return nil
}

func (m *visitor) LogEvent(ctx context.Context, op scop.LogEvent) error {
	descID := screl.GetDescID(op.Element.Element())
	fullName, err := m.cr.GetFullyQualifiedName(ctx, descID)
	if err != nil {
		return err
	}
	if op.Direction == scpb.Target_DROP {
		switch op.Element.GetValue().(type) {
		case *scpb.Table:
			return m.ev.AddDropEvent(ctx, op.DescID, &op.Metadata,
				&eventpb.DropTable{
					TableName: fullName,
				},
			)
		case *scpb.View:
			return m.ev.AddDropEvent(ctx, op.DescID, &op.Metadata,
				&eventpb.DropView{
					ViewName: fullName,
				},
			)
		case *scpb.Sequence:
			return m.ev.AddDropEvent(ctx, op.DescID, &op.Metadata,
				&eventpb.DropSequence{
					SequenceName: fullName,
				},
			)
		case *scpb.Database:
			return m.ev.AddDropEvent(ctx, op.DescID, &op.Metadata,
				&eventpb.DropDatabase{
					DatabaseName: fullName,
				},
			)
		case *scpb.Schema:
			return m.ev.AddDropEvent(ctx, op.DescID, &op.Metadata,
				&eventpb.DropSchema{
					SchemaName: fullName,
				},
			)
		default:
			panic("unknown element type")
		}
	} else if op.Direction == scpb.Target_ADD {
		switch element := op.Element.GetValue().(type) {
		case *scpb.Column:
			table, err := m.checkOutTable(ctx, op.DescID)
			if err != nil {
				return err
			}
			mutation, err := descriptorutils.FindMutation(table,
				descriptorutils.MakeColumnIDMutationSelector(element.Column.ID))
			if err != nil {
				return err
			}
			return m.ev.AddDropEvent(ctx, op.DescID, &op.Metadata,
				&eventpb.AlterTable{
					TableName:  fullName,
					MutationID: uint32(mutation.MutationID()),
				})
		default:
			panic("unknown element type")
		}
	}
	return nil
}

func (m *visitor) AddIndexPartitionInfo(ctx context.Context, op scop.AddIndexPartitionInfo) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	index, err := tbl.FindIndexWithID(op.IndexID)
	if err != nil {
		return err
	}
	return m.cr.AddPartitioning(tbl, index.IndexDesc(), op.PartitionFields, op.ListPartitions, op.RangePartitions, nil, true)
}

var _ scop.MutationVisitor = (*visitor)(nil)
