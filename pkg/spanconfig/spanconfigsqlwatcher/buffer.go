// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package spanconfigsqlwatcher

import (
	"context"
	"sort"

	"github.com/cockroachdb/cockroach/pkg/kv/kvclient/rangefeed/rangefeedbuffer"
	"github.com/cockroachdb/cockroach/pkg/spanconfig"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/errors"
)

// buffer is a helper struct for the SQLWatcher. It buffers events generated by
// the SQLWatcher's rangefeeds over system.zones and system.descriptors. It is
// safe for concurrent use.
//
// The buffer tracks frontier timestamps for both these rangefeeds as well. It
// maintains the notion of the combined frontier timestamp computed as the
// minimum of the two. This is used when flushing the buffer periodically.
type buffer struct {
	mu struct {
		syncutil.Mutex

		// rangefeed.Buffer stores spanconfigsqlwatcher.Events.
		buffer *rangefeedbuffer.Buffer

		// rangefeedFrontiers tracks the frontier timestamps of individual
		// rangefeeds established by the SQLWatcher.
		rangefeedFrontiers [numRangefeeds]hlc.Timestamp
	}
}

// event is the unit produced by the rangefeeds the SQLWatcher establishes over
// system.zones and system.descriptors. It implements the rangefeedbuffer.Event
// interface.
type event struct {
	// timestamp at which the event was generated by the rangefeed.
	timestamp hlc.Timestamp

	// update captures information about the descriptor or zone that the
	// SQLWatcher has observed change.
	update spanconfig.DescriptorUpdate
}

// Timestamp implements the rangefeedbuffer.Event interface.
func (e event) Timestamp() hlc.Timestamp {
	return e.timestamp
}

// rangefeedKind is used to identify the distinct rangefeeds {descriptors,
// zones} established by the SQLWatcher.
type rangefeedKind int

const (
	zonesRangefeed rangefeedKind = iota
	descriptorsRangefeed

	// numRangefeeds should be listed last.
	numRangefeeds int = iota
)

// newBuffer constructs and returns a new buffer.
func newBuffer(limit int) *buffer {
	rangefeedBuffer := rangefeedbuffer.New(limit)
	eventBuffer := &buffer{}
	eventBuffer.mu.buffer = rangefeedBuffer
	return eventBuffer
}

// advance advances the frontier for the given rangefeed.
func (b *buffer) advance(rangefeed rangefeedKind, timestamp hlc.Timestamp) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.mu.rangefeedFrontiers[rangefeed].Forward(timestamp)
}

// add records the given event in the buffer.
func (b *buffer) add(ev event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mu.buffer.Add(ev)
}

// flushEvents computes the combined frontier timestamp of the buffer and
// returns  a list of relevant events which were buffered up to that timestamp.
func (b *buffer) flushEvents(
	ctx context.Context,
) (updates []rangefeedbuffer.Event, combinedFrontierTS hlc.Timestamp) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// First we determine the checkpoint timestamp, which is the minimum
	// checkpoint timestamp of all event types.
	combinedFrontierTS = hlc.MaxTimestamp
	for _, ts := range b.mu.rangefeedFrontiers {
		combinedFrontierTS.Backward(ts)
	}

	return b.mu.buffer.Flush(ctx, combinedFrontierTS), combinedFrontierTS
}

// flush computes the combined frontier timestamp of the buffer and returns a
// list of unique spanconfig.DescriptorUpdates below this timestamp. The
// combined frontier timestamp is also returned.
func (b *buffer) flush(
	ctx context.Context,
) (updates []spanconfig.DescriptorUpdate, _ hlc.Timestamp, _ error) {
	events, combinedFrontierTS := b.flushEvents(ctx)
	sort.Slice(events, func(i, j int) bool {
		ei, ej := events[i].(event), events[j].(event)
		if ei.update.ID == ej.update.ID {
			return ei.timestamp.Less(ej.timestamp)
		}
		return ei.update.ID < ej.update.ID
	})
	for i, ev := range events {
		if i == 0 || events[i-1].(event).update.ID != ev.(event).update.ID {
			updates = append(updates, ev.(event).update)
			continue
		}
		descType, err := combine(updates[len(updates)-1].DescriptorType, ev.(event).update.DescriptorType)
		if err != nil {
			return nil, hlc.Timestamp{}, err
		}
		updates[len(updates)-1].DescriptorType = descType
	}
	return updates, combinedFrontierTS, nil
}

// combine takes two catalog.DescriptorTypes and combines them according to the
// following semantics:
// - Any can combine with any concrete descriptor type (including itself).
// Concrete descriptor types are {Table,Database,Schema,Type} descriptor types.
// - Concrete descriptor types can combine with themselves.
// - A concrete descriptor type cannot combine with another concrete descriptor
// type.
func combine(d1 catalog.DescriptorType, d2 catalog.DescriptorType) (catalog.DescriptorType, error) {
	if d1 == d2 {
		return d1, nil
	}
	if d1 == catalog.Any {
		return d2, nil
	}
	if d2 == catalog.Any {
		return d1, nil
	}
	return catalog.Any, errors.AssertionFailedf("cannot combine %s and %s", d1, d2)
}
