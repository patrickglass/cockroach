// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package tracing

import (
	"strings"

	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/cockroach/pkg/util/tracing/tracingpb"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
	"golang.org/x/net/trace"
)

type spanInner struct {
	tracer *Tracer // never nil

	// Internal trace Span; nil if not tracing to crdb.
	// When not-nil, allocated together with the surrounding Span for
	// performance.
	crdb *crdbSpan
	// x/net/trace.Trace instance; nil if not tracing to x/net/trace.
	netTr trace.Trace
	// otelSpan is the "shadow span" created for reporting to the OpenTelemetry
	// tracer (if an otel tracer was configured).
	otelSpan oteltrace.Span

	// sterile is set if this span does not want to have children spans. In that
	// case, trying to create a child span will result in the would-be child being
	// a root span. This is useful for span corresponding to long-running
	// operations that don't want to be associated with derived operations.
	sterile bool
}

func (s *spanInner) TraceID() tracingpb.TraceID {
	if s.isNoop() {
		return 0
	}
	return s.crdb.TraceID()
}

func (s *spanInner) isNoop() bool {
	return s.crdb == nil && s.netTr == nil && s.otelSpan == nil
}

func (s *spanInner) isSterile() bool {
	return s.sterile
}

func (s *spanInner) RecordingType() RecordingType {
	return s.crdb.recordingType()
}

func (s *spanInner) SetVerbose(to bool) {
	if s.isNoop() {
		panic(errors.AssertionFailedf("SetVerbose called on NoopSpan; use the WithForceRealSpan option for StartSpan"))
	}
	s.crdb.SetVerbose(to)
}

func (s *spanInner) GetRecording(recType RecordingType) Recording {
	if s.isNoop() {
		return nil
	}
	return s.crdb.GetRecording(recType)
}

func (s *spanInner) ImportRemoteSpans(remoteSpans []tracingpb.RecordedSpan) {
	s.crdb.recordFinishedChildren(remoteSpans)
}

func (s *spanInner) Finish() {
	if s == nil {
		return
	}
	if s.isNoop() {
		return
	}

	if !s.crdb.finish() {
		// The span was already finished. External spans and net/trace are not
		// always forgiving about spans getting finished twice, but it may happen so
		// let's be resilient to it.
		return
	}

	if s.otelSpan != nil {
		s.otelSpan.End()
	}
	if s.netTr != nil {
		s.netTr.Finish()
	}
}

func (s *spanInner) Meta() SpanMeta {
	var traceID tracingpb.TraceID
	var spanID tracingpb.SpanID
	var recordingType RecordingType
	var sterile bool

	if s.crdb != nil {
		traceID, spanID = s.crdb.traceID, s.crdb.spanID
		recordingType = s.crdb.mu.recording.recordingType.load()
		sterile = s.isSterile()
	}

	var otelCtx oteltrace.SpanContext
	if s.otelSpan != nil {
		otelCtx = s.otelSpan.SpanContext()
	}

	if traceID == 0 &&
		spanID == 0 &&
		!otelCtx.TraceID().IsValid() &&
		recordingType == 0 &&
		!sterile {
		return SpanMeta{}
	}
	return SpanMeta{
		traceID:       traceID,
		spanID:        spanID,
		otelCtx:       otelCtx,
		recordingType: recordingType,
		sterile:       sterile,
	}
}

func (s *spanInner) SetOperationName(operationName string) *spanInner {
	if s.isNoop() {
		return s
	}
	if s.otelSpan != nil {
		s.otelSpan.SetName(operationName)
	}
	s.crdb.mu.Lock()
	s.crdb.mu.operation = operationName
	s.crdb.mu.Unlock()
	return s
}

func (s *spanInner) SetTag(key string, value attribute.Value) *spanInner {
	if s.isNoop() {
		return s
	}
	return s.setTagInner(key, value, false /* locked */)
}

func (s *spanInner) setTagInner(key string, value attribute.Value, locked bool) *spanInner {
	if s.otelSpan != nil {
		s.otelSpan.SetAttributes(attribute.KeyValue{
			Key:   attribute.Key(key),
			Value: value,
		})
	}
	if s.netTr != nil {
		s.netTr.LazyPrintf("%s:%v", key, value)
	}
	// The internal tags will be used if we start a recording on this Span.
	if !locked {
		s.crdb.mu.Lock()
		defer s.crdb.mu.Unlock()
	}
	s.crdb.setTagLocked(key, value)
	return s
}

func (s *spanInner) RecordStructured(item Structured) {
	if s.isNoop() {
		return
	}
	s.crdb.recordStructured(item)
	if s.hasVerboseSink() {
		// NB: TrimSpace avoids the trailing whitespace generated by the
		// protobuf stringers.
		s.Record(strings.TrimSpace(item.String()))
	}
}

func (s *spanInner) Record(msg string) {
	s.Recordf("%s", msg)
}

func (s *spanInner) Recordf(format string, args ...interface{}) {
	if !s.hasVerboseSink() {
		return
	}
	str := redact.Sprintf(format, args...)
	if s.otelSpan != nil {
		// TODO(obs-inf): depending on the situation it may be more appropriate to
		// redact the string here.
		// See:
		// https://github.com/cockroachdb/cockroach/issues/58610#issuecomment-926093901
		s.otelSpan.AddEvent(str.StripMarkers(), oteltrace.WithTimestamp(timeutil.Now()))
	}
	if s.netTr != nil {
		s.netTr.LazyPrintf(format, args)
	}
	s.crdb.record(str)
}

// hasVerboseSink returns false if there is no reason to even evaluate Record
// because the result wouldn't be used for anything.
func (s *spanInner) hasVerboseSink() bool {
	if s.netTr == nil && s.otelSpan == nil && s.RecordingType() != RecordingVerbose {
		return false
	}
	return true
}

// Tracer exports the tracer this span was created using.
func (s *spanInner) Tracer() *Tracer {
	return s.tracer
}
