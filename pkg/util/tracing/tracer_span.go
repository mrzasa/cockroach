// Copyright 2017 The Cockroach Authors.
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
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
	proto "github.com/gogo/protobuf/proto"
	"github.com/gogo/protobuf/types"
	jaegerjson "github.com/jaegertracing/jaeger/model/json"
	opentracing "github.com/opentracing/opentracing-go"
	otlog "github.com/opentracing/opentracing-go/log"
	"golang.org/x/net/trace"
)

// spanMeta stores span information that is common to span and spanContext.
type spanMeta struct {
	// A probabilistically unique identifier for a [multi-span] trace.
	TraceID uint64

	// A probabilistically unique identifier for a span.
	SpanID uint64
}

type spanContext struct {
	spanMeta

	// Underlying shadow tracer info and context (optional).
	shadowTr  *shadowTracer
	shadowCtx opentracing.SpanContext

	// If set, all spans derived from this context are being recorded.
	recordingType RecordingType
	// span is set if this context corresponds to a local span. If so, pointing
	// back to the span is used for registering child spans with their parent.
	// Children of remote spans act as roots when it comes to recordings - someone
	// is responsible for calling GetRecording() on them and marshaling the
	// recording back to the parent (generally an RPC handler does this).
	span *span

	// The span's associated baggage.
	Baggage map[string]string
}

const (
	// TagPrefix is prefixed to all tags that should be output in SHOW TRACE.
	TagPrefix = "cockroach."
	// StatTagPrefix is prefixed to all stats output in span tags.
	StatTagPrefix = TagPrefix + "stat."
)

// SpanStats are stats that can be added to a span.
type SpanStats interface {
	proto.Message
	// Stats returns the stats that the object represents as a map from stat name
	// to value to be added to span tags. The keys will be prefixed with
	// StatTagPrefix.
	Stats() map[string]string
}

var _ opentracing.SpanContext = &spanContext{}

// ForeachBaggageItem is part of the opentracing.SpanContext interface.
func (sc *spanContext) ForeachBaggageItem(handler func(k, v string) bool) {
	for k, v := range sc.Baggage {
		if !handler(k, v) {
			break
		}
	}
}

// RecordingType is the type of recording that a span might be performing.
type RecordingType int

const (
	// NoRecording means that the span isn't recording.
	NoRecording RecordingType = iota
	// SnowballRecording means that remote child spans (generally opened through
	// RPCs) are also recorded.
	SnowballRecording
	// SingleNodeRecording means that only spans on the current node are recorded.
	SingleNodeRecording
)

type crdbSpan struct {
	spanMeta

	parentSpanID uint64

	tracer *Tracer

	operation string
	startTime time.Time

	// logTags are set to the log tags that were available when this span was
	// created, so that there's no need to eagerly copy all of those log tags into
	// this span's tags. If the span's tags are actually requested, these logTags
	// will be copied out at that point.
	// Note that these tags have not gone through the log tag -> span tag
	// remapping procedure; tagName() needs to be called before exposing each
	// tag's key to a user.
	logTags *logtags.Buffer

	// Atomic flag used to avoid taking the mutex in the hot path.
	recording int32

	mu struct {
		syncutil.Mutex
		// duration is initialized to -1 and set on Finish().
		duration time.Duration

		// recording maintains state once StartRecording() is called.
		recording struct {
			recordingType RecordingType
			recordedLogs  []opentracing.LogRecord
			// children contains the list of child spans started after this span
			// started recording.
			children []*span
			// remoteSpan contains the list of remote child spans manually imported.
			remoteSpans []RecordedSpan
		}

		// tags are only set when recording. These are tags that have been added to
		// this span, and will be appended to the tags in logTags when someone
		// needs to actually observe the total set of tags that is a part of this
		// span.
		// TODO(radu): perhaps we want a recording to capture all the tags (even
		// those that were set before recording started)?
		tags opentracing.Tags

		stats SpanStats

		// The span's associated baggage.
		Baggage map[string]string
	}
}

func (s *crdbSpan) isRecording() bool {
	return atomic.LoadInt32(&s.recording) != 0
}

type otSpan struct {
	// TODO(tbg): see if we can lose the shadowTr here and rely on shadowSpan.Tracer().
	// Probably not - but worth checking.
	// TODO(tbg): consider renaming 'shadow' -> 'ot' or 'external'.
	shadowTr   *shadowTracer
	shadowSpan opentracing.Span
}

type span struct {
	crdbSpan // can be zero
	// x/net/trace.Trace instance; nil if not tracing to x/net/trace.
	netTr trace.Trace
	// Shadow tracer and span; zero if not using a shadow tracer.
	otSpan otSpan
}

// TODO(tbg): remove this. We don't need *span to be an opentracing.Span.
var _ opentracing.Span = &span{}

func (s *span) isBlackHole() bool {
	return !s.crdbSpan.isRecording() && s.netTr == nil && s.otSpan == (otSpan{})
}

/*
TODO(tbg): replace noopSpan with this concept:
func (s *span) isNoop() bool {
	// Special case: trace id zero implies that everything else,
	// with the exception of the tracer, is also zero.
	return s.isBlackHole() && s.crdbSpan.TraceID == 0
}
*/

// IsRecording returns true if the span is recording its events.
func IsRecording(s opentracing.Span) bool {
	if _, noop := s.(*noopSpan); noop {
		return false
	}
	return s.(*span).isRecording()
}

// enableRecording start recording on the span. From now on, log events and child spans
// will be stored.
//
// If parent != nil, the span will be registered as a child of the respective
// parent.
// If separate recording is specified, the child is not registered with the
// parent. Thus, the parent's recording will not include this child.
func (s *span) enableRecording(parent *span, recType RecordingType, separateRecording bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	atomic.StoreInt32(&s.recording, 1)
	s.mu.recording.recordingType = recType
	if parent != nil && !separateRecording {
		parent.addChild(s)
	}
	if recType == SnowballRecording {
		s.setBaggageItemLocked(Snowball, "1")
	}
	// Clear any previously recorded info. This is needed by SQL SessionTracing,
	// who likes to start and stop recording repeatedly on the same span, and
	// collect the (separate) recordings every time.
	s.mu.recording.recordedLogs = nil
	s.mu.recording.children = nil
	s.mu.recording.remoteSpans = nil
}

// StartRecording enables recording on the span. Events from this point forward
// are recorded; also, all direct and indirect child spans started from now on
// will be part of the same recording.
//
// Recording is not supported by noop spans; to ensure a real span is always
// created, use the Recordable option to StartSpan.
//
// If recording was already started on this span (either directly or because a
// parent span is recording), the old recording is lost.
func StartRecording(os opentracing.Span, recType RecordingType) {
	if recType == NoRecording {
		panic("StartRecording called with NoRecording")
	}
	if _, noop := os.(*noopSpan); noop {
		panic("StartRecording called on NoopSpan; use the Recordable option for StartSpan")
	}

	// If we're already recording (perhaps because the parent was recording when
	// this span was created), there's nothing to do.
	if sp := os.(*span); !sp.isRecording() {
		sp.enableRecording(nil /* parent */, recType, false /* separateRecording */)
	}
}

// StopRecording disables recording on this span. Child spans that were created
// since recording was started will continue to record until they finish.
//
// Calling this after StartRecording is not required; the recording will go away
// when all the spans finish.
//
// StopRecording() can be called on a Finish()ed span.
func StopRecording(os opentracing.Span) {
	os.(*span).disableRecording()
}

func (s *span) disableRecording() {
	s.mu.Lock()
	atomic.StoreInt32(&s.recording, 0)
	// We test the duration as a way to check if the span has been finished. If it
	// has, we don't want to do the call below as it might crash (at least if
	// there's a netTr).
	if (s.mu.duration == -1) && (s.mu.recording.recordingType == SnowballRecording) {
		// Clear the Snowball baggage item, assuming that it was set by
		// enableRecording().
		s.setBaggageItemLocked(Snowball, "")
	}
	s.mu.Unlock()
}

// IsRecordable returns true if {Start,Stop}Recording() can be called on this
// span.
//
// In other words, this tests if the span is our custom type, and not a noopSpan
// or anything else.
func IsRecordable(os opentracing.Span) bool {
	_, isCockroachSpan := os.(*span)
	return isCockroachSpan
}

// Recording represents a group of RecordedSpans, as returned by GetRecording.
// Spans are sorted by StartTime.
type Recording []RecordedSpan

// GetRecording retrieves the current recording, if the span has recording
// enabled. This can be called while spans that are part of the recording are
// still open; it can run concurrently with operations on those spans.
func GetRecording(os opentracing.Span) Recording {
	if _, noop := os.(*noopSpan); noop {
		return nil
	}
	s := os.(*span)
	if !s.isRecording() {
		return nil
	}
	s.mu.Lock()
	// The capacity here is approximate since we don't know how many grandchildren
	// there are.
	result := make(Recording, 0, 1+len(s.mu.recording.children)+len(s.mu.recording.remoteSpans))
	// Shallow-copy the children so we can process them without the lock.
	children := s.mu.recording.children
	result = append(result, s.getRecordingLocked())
	result = append(result, s.mu.recording.remoteSpans...)
	s.mu.Unlock()

	for _, child := range children {
		result = append(result, GetRecording(child)...)
	}

	// Sort the spans by StartTime, except the first span (the root of this
	// recording) which stays in place.
	toSort := result[1:]
	sort.Slice(toSort, func(i, j int) bool {
		return toSort[i].StartTime.Before(toSort[j].StartTime)
	})
	return result
}

type traceLogData struct {
	opentracing.LogRecord
	depth int
	// timeSincePrev represents the duration since the previous log line (previous in the
	// set of log lines that this is part of). This is always computed relative to a log line
	// from the same span, except for start of span in which case the duration is computed relative
	// to the last log in the parent occurring before this start. For example:
	// start span A
	// log 1           // duration relative to "start span A"
	//   start span B  // duration relative to "log 1"
	//   log 2  			 // duration relative to "start span B"
	// log 3  				 // duration relative to "log 1"
	timeSincePrev time.Duration
}

// String formats the given spans for human consumption, showing the
// relationship using nesting and times as both relative to the previous event
// and cumulative.
//
// Child spans are inserted into the parent at the point of the child's
// StartTime; see the diagram on generateSessionTraceVTable() for the ordering
// of messages.
//
// Each log line show the time since the beginning of the trace
// and since the previous log line. Span starts are shown with special "===
// <operation>" lines. For a span start, the time since the relative log line
// can be negative when the span start follows a message from the parent that
// was generated after the child span started (or even after the child
// finished).
//
// TODO(andrei): this should be unified with
// SessionTracing.generateSessionTraceVTable().
func (r Recording) String() string {
	if len(r) == 0 {
		return "<empty recording>"
	}

	var buf strings.Builder
	start := r[0].StartTime
	writeLogs := func(logs []traceLogData) {
		for _, entry := range logs {
			fmt.Fprintf(&buf, "% 10.3fms % 10.3fms%s",
				1000*entry.Timestamp.Sub(start).Seconds(),
				1000*entry.timeSincePrev.Seconds(),
				strings.Repeat("    ", entry.depth+1))
			for i, f := range entry.Fields {
				if i != 0 {
					buf.WriteByte(' ')
				}
				fmt.Fprintf(&buf, "%s:%v", f.Key(), f.Value())
			}
			buf.WriteByte('\n')
		}
	}

	logs := r.visitSpan(r[0], 0 /* depth */)
	writeLogs(logs)

	// Check if there's any orphan spans (spans for which the parent is missing).
	// This shouldn't happen, but we're protecting against incomplete traces. For
	// example, ingesting of remote spans through DistSQL is complex. Orphan spans
	// would not be reflected in the output string at all without this.
	orphans := r.OrphanSpans()
	if len(orphans) > 0 {
		// This shouldn't happen.
		buf.WriteString("orphan spans (trace is missing spans):\n")
		for _, o := range orphans {
			logs := r.visitSpan(o, 0 /* depth */)
			writeLogs(logs)
		}
	}
	return buf.String()
}

// OrphanSpans returns the spans with parents missing from the recording.
func (r Recording) OrphanSpans() []RecordedSpan {
	spanIDs := make(map[uint64]struct{})
	for _, sp := range r {
		spanIDs[sp.SpanID] = struct{}{}
	}

	var orphans []RecordedSpan
	for i, sp := range r {
		if i == 0 {
			// The first span can be a root span. Note that any other root span will
			// be considered an orphan.
			continue
		}
		if _, ok := spanIDs[sp.ParentSpanID]; !ok {
			orphans = append(orphans, sp)
		}
	}
	return orphans
}

// FindLogMessage returns the first log message in the recording that matches
// the given regexp. The bool return value is true if such a message is found.
func (r Recording) FindLogMessage(pattern string) (string, bool) {
	re := regexp.MustCompile(pattern)
	for _, sp := range r {
		for _, l := range sp.Logs {
			msg := l.Msg()
			if re.MatchString(msg) {
				return msg, true
			}
		}
	}
	return "", false
}

// FindSpan returns the span with the given operation. The bool retval is false
// if the span is not found.
func (r Recording) FindSpan(operation string) (RecordedSpan, bool) {
	for _, sp := range r {
		if sp.Operation == operation {
			return sp, true
		}
	}
	return RecordedSpan{}, false
}

// visitSpan returns the log messages for sp, and all of sp's children.
//
// All messages from a span are kept together. Sibling spans are ordered within
// the parent in their start order.
func (r Recording) visitSpan(sp RecordedSpan, depth int) []traceLogData {
	ownLogs := make([]traceLogData, 0, len(sp.Logs)+1)

	conv := func(l opentracing.LogRecord, ref time.Time) traceLogData {
		var timeSincePrev time.Duration
		if ref != (time.Time{}) {
			timeSincePrev = l.Timestamp.Sub(ref)
		}
		return traceLogData{
			LogRecord:     l,
			depth:         depth,
			timeSincePrev: timeSincePrev,
		}
	}

	// Add a log line representing the start of the span.
	lr := opentracing.LogRecord{
		Timestamp: sp.StartTime,
		Fields:    []otlog.Field{otlog.String("=== operation", sp.Operation)},
	}
	if len(sp.Tags) > 0 {
		tags := make([]string, 0, len(sp.Tags))
		for k := range sp.Tags {
			tags = append(tags, k)
		}
		sort.Strings(tags)
		for _, k := range tags {
			lr.Fields = append(lr.Fields, otlog.String(k, sp.Tags[k]))
		}
	}
	ownLogs = append(ownLogs, conv(
		lr,
		// ref - this entries timeSincePrev will be computed when we merge it into the parent
		time.Time{}))

	for _, l := range sp.Logs {
		lr := opentracing.LogRecord{
			Timestamp: l.Time,
			Fields:    make([]otlog.Field, len(l.Fields)),
		}
		for i, f := range l.Fields {
			lr.Fields[i] = otlog.String(f.Key, f.Value)
		}
		lastLog := ownLogs[len(ownLogs)-1]
		ownLogs = append(ownLogs, conv(lr, lastLog.Timestamp))
	}

	childSpans := make([][]traceLogData, 0)
	for _, osp := range r {
		if osp.ParentSpanID != sp.SpanID {
			continue
		}
		childSpans = append(childSpans, r.visitSpan(osp, depth+1))
	}

	// Merge ownLogs with childSpans.
	mergedLogs := make([]traceLogData, 0, len(ownLogs))
	timeMax := time.Date(2200, 0, 0, 0, 0, 0, 0, time.UTC)
	i, j := 0, 0
	var lastTimestamp time.Time
	for i < len(ownLogs) || j < len(childSpans) {
		if len(mergedLogs) > 0 {
			lastTimestamp = mergedLogs[len(mergedLogs)-1].Timestamp
		}
		nextLog, nextChild := timeMax, timeMax
		if i < len(ownLogs) {
			nextLog = ownLogs[i].Timestamp
		}
		if j < len(childSpans) {
			nextChild = childSpans[j][0].Timestamp
		}
		if nextLog.After(nextChild) {
			// Fill in timeSincePrev for the first one of the child's entries.
			if lastTimestamp != (time.Time{}) {
				childSpans[j][0].timeSincePrev = childSpans[j][0].Timestamp.Sub(lastTimestamp)
			}
			mergedLogs = append(mergedLogs, childSpans[j]...)
			lastTimestamp = childSpans[j][0].Timestamp
			j++
		} else {
			mergedLogs = append(mergedLogs, ownLogs[i])
			lastTimestamp = ownLogs[i].Timestamp
			i++
		}
	}

	return mergedLogs
}

// ToJaegerJSON returns the trace as a JSON that can be imported into Jaeger for
// visualization.
//
// The format is described here: https://github.com/jaegertracing/jaeger-ui/issues/381#issuecomment-494150826
//
// The statement is passed in so it can be included in the trace.
func (r Recording) ToJaegerJSON(stmt string) (string, error) {
	if len(r) == 0 {
		return "", nil
	}

	cpy := make(Recording, len(r))
	copy(cpy, r)
	r = cpy
	tagsCopy := make(map[string]string)
	for k, v := range r[0].Tags {
		tagsCopy[k] = v
	}
	tagsCopy["statement"] = stmt
	r[0].Tags = tagsCopy

	toJaegerSpanID := func(spanID uint64) jaegerjson.SpanID {
		return jaegerjson.SpanID(strconv.FormatUint(spanID, 10))
	}

	// Each span in Jaeger belongs to a "process" that generated it. Spans
	// belonging to different colors are colored differently in Jaeger. We're
	// going to map our different nodes to different processes.
	processes := make(map[jaegerjson.ProcessID]jaegerjson.Process)
	// getProcessID figures out what "process" a span belongs to. It looks for an
	// "node: <node id>" tag. The processes map is populated with an entry for every
	// node present in the trace.
	getProcessID := func(sp RecordedSpan) jaegerjson.ProcessID {
		node := "unknown node"
		for k, v := range sp.Tags {
			if k == "node" {
				node = fmt.Sprintf("node %s", v)
				break
			}
		}
		pid := jaegerjson.ProcessID(node)
		if _, ok := processes[pid]; !ok {
			processes[pid] = jaegerjson.Process{
				ServiceName: node,
				Tags:        nil,
			}
		}
		return pid
	}

	var t jaegerjson.Trace
	t.TraceID = jaegerjson.TraceID(strconv.FormatUint(r[0].TraceID, 10))
	t.Processes = processes

	for _, sp := range r {
		var s jaegerjson.Span

		s.TraceID = t.TraceID
		s.Duration = uint64(sp.Duration.Microseconds())
		s.StartTime = uint64(sp.StartTime.UnixNano() / 1000)
		s.SpanID = toJaegerSpanID(sp.SpanID)
		s.OperationName = sp.Operation
		s.ProcessID = getProcessID(sp)

		if sp.ParentSpanID != 0 {
			s.References = []jaegerjson.Reference{{
				RefType: jaegerjson.ChildOf,
				TraceID: s.TraceID,
				SpanID:  toJaegerSpanID(sp.ParentSpanID),
			}}
		}

		for k, v := range sp.Tags {
			s.Tags = append(s.Tags, jaegerjson.KeyValue{
				Key:   k,
				Value: v,
				Type:  "STRING",
			})
		}
		for _, l := range sp.Logs {
			jl := jaegerjson.Log{Timestamp: uint64(l.Time.UnixNano() / 1000)}
			for _, field := range l.Fields {
				jl.Fields = append(jl.Fields, jaegerjson.KeyValue{
					Key:   field.Key,
					Value: field.Value,
					Type:  "STRING",
				})
			}
			s.Logs = append(s.Logs, jl)
		}
		t.Spans = append(t.Spans, s)
	}

	data := TraceCollection{
		Data: []jaegerjson.Trace{t},
		// Add a comment that will show-up at the top of the JSON file, is someone opens the file.
		// NOTE: This comment is scarce on newlines because they appear as \n in the
		// generated file doing more harm than good.
		Comment: fmt.Sprintf(`This is a trace for SQL statement: %s
This trace can be imported into Jaeger for visualization. From the Jaeger Search screen, select JSON File.
Jaeger can be started using docker with: docker run -d --name jaeger -p 16686:16686 jaegertracing/all-in-one:1.17
The UI can then be accessed at http://localhost:16686/search`,
			stmt),
	}
	json, err := json.MarshalIndent(data, "" /* prefix */, "\t" /* indent */)
	if err != nil {
		return "", err
	}
	return string(json), nil
}

// TraceCollection is the format accepted by the Jaegar upload feature, as per
// https://github.com/jaegertracing/jaeger-ui/issues/381#issuecomment-494150826
type TraceCollection struct {
	// Comment is a dummy field we use to put instructions on how to load the trace.
	Comment string             `json:"_comment"`
	Data    []jaegerjson.Trace `json:"data"`
}

// ImportRemoteSpans adds RecordedSpan data to the recording of the given span;
// these spans will be part of the result of GetRecording. Used to import
// recorded traces from other nodes.
func ImportRemoteSpans(os opentracing.Span, remoteSpans []RecordedSpan) error {
	s := os.(*span)
	if !s.isRecording() {
		return errors.New("adding Raw Spans to a span that isn't recording")
	}

	// Change the root of the remote recording to be a child of this span. This is
	// usually already the case, except with DistSQL traces where remote
	// processors run in spans that FollowFrom an RPC span that we don't collect.
	remoteSpans[0].ParentSpanID = s.SpanID

	s.mu.Lock()
	s.mu.recording.remoteSpans = append(s.mu.recording.remoteSpans, remoteSpans...)
	s.mu.Unlock()
	return nil
}

// IsBlackHoleSpan returns true if events for this span are just dropped. This
// is the case when tracing is disabled and we're not recording. Tracing clients
// can use this method to figure out if they can short-circuit some
// tracing-related work that would be discarded anyway.
func IsBlackHoleSpan(s opentracing.Span) bool {
	// There are two types of black holes: instances of noopSpan and, when tracing
	// is disabled, real spans that are not recording.
	if _, noop := s.(*noopSpan); noop {
		return true
	}
	sp := s.(*span)
	return sp.isBlackHole()
}

// IsNoopContext returns true if the span context is from a "no-op" span. If
// this is true, any span derived from this context will be a "black hole span".
func IsNoopContext(spanCtx opentracing.SpanContext) bool {
	_, noop := spanCtx.(noopSpanContext)
	return noop
}

// SetSpanStats sets the stats on a span. stats.Stats() will also be added to
// the span tags.
func SetSpanStats(os opentracing.Span, stats SpanStats) {
	s := os.(*span)
	s.mu.Lock()
	s.mu.stats = stats
	for name, value := range stats.Stats() {
		s.setTagInner(StatTagPrefix+name, value, true /* locked */)
	}
	s.mu.Unlock()
}

// Finish is part of the opentracing.Span interface.
func (s *span) Finish() {
	s.FinishWithOptions(opentracing.FinishOptions{})
}

// FinishWithOptions is part of the opentracing.Span interface.
func (s *span) FinishWithOptions(opts opentracing.FinishOptions) {
	finishTime := opts.FinishTime
	if finishTime.IsZero() {
		finishTime = time.Now()
	}
	s.mu.Lock()
	s.mu.duration = finishTime.Sub(s.startTime)
	s.mu.Unlock()
	if s.otSpan.shadowTr != nil {
		s.otSpan.shadowSpan.Finish()
	}
	if s.netTr != nil {
		s.netTr.Finish()
	}
}

// Context is part of the opentracing.Span interface.
//
// TODO(andrei, radu): Should this return noopSpanContext for a Recordable span
// that's not currently recording? That might save work and allocations when
// creating child spans.
func (s *span) Context() opentracing.SpanContext {
	s.mu.Lock()
	defer s.mu.Unlock()
	baggageCopy := make(map[string]string, len(s.mu.Baggage))
	for k, v := range s.mu.Baggage {
		baggageCopy[k] = v
	}
	sc := &spanContext{
		spanMeta: s.spanMeta,
		span:     s,
		Baggage:  baggageCopy,
	}
	if s.otSpan.shadowTr != nil {
		sc.shadowTr = s.otSpan.shadowTr
		sc.shadowCtx = s.otSpan.shadowSpan.Context()
	}

	if s.isRecording() {
		sc.recordingType = s.mu.recording.recordingType
	}
	return sc
}

// SetOperationName is part of the opentracing.Span interface.
func (s *span) SetOperationName(operationName string) opentracing.Span {
	if s.otSpan.shadowTr != nil {
		s.otSpan.shadowSpan.SetOperationName(operationName)
	}
	s.operation = operationName
	return s
}

// SetTag is part of the opentracing.Span interface.
func (s *span) SetTag(key string, value interface{}) opentracing.Span {
	return s.setTagInner(key, value, false /* locked */)
}

func (s *span) setTagInner(key string, value interface{}, locked bool) opentracing.Span {
	if s.otSpan.shadowTr != nil {
		s.otSpan.shadowSpan.SetTag(key, value)
	}
	if s.netTr != nil {
		s.netTr.LazyPrintf("%s:%v", key, value)
	}
	// The internal tags will be used if we start a recording on this span.
	if !locked {
		s.mu.Lock()
	}
	if s.mu.tags == nil {
		s.mu.tags = make(opentracing.Tags)
	}
	s.mu.tags[key] = value
	if !locked {
		s.mu.Unlock()
	}
	return s
}

// LogFields is part of the opentracing.Span interface.
func (s *span) LogFields(fields ...otlog.Field) {
	if s.otSpan.shadowTr != nil {
		s.otSpan.shadowSpan.LogFields(fields...)
	}
	if s.netTr != nil {
		// TODO(radu): when LightStep supports arbitrary fields, we should make
		// the formatting of the message consistent with that. Until then we treat
		// legacy events that just have an "event" key specially.
		if len(fields) == 1 && fields[0].Key() == LogMessageField {
			s.netTr.LazyPrintf("%s", fields[0].Value())
		} else {
			var buf bytes.Buffer
			for i, f := range fields {
				if i > 0 {
					buf.WriteByte(' ')
				}
				fmt.Fprintf(&buf, "%s:%v", f.Key(), f.Value())
			}

			s.netTr.LazyPrintf("%s", buf.String())
		}
	}
	if s.isRecording() {
		s.mu.Lock()
		if len(s.mu.recording.recordedLogs) < maxLogsPerSpan {
			s.mu.recording.recordedLogs = append(s.mu.recording.recordedLogs, opentracing.LogRecord{
				Timestamp: time.Now(),
				Fields:    fields,
			})
		}
		s.mu.Unlock()
	}
}

// LogKV is part of the opentracing.Span interface.
func (s *span) LogKV(alternatingKeyValues ...interface{}) {
	fields, err := otlog.InterleavedKVToFields(alternatingKeyValues...)
	if err != nil {
		s.LogFields(otlog.Error(err), otlog.String("function", "LogKV"))
		return
	}
	s.LogFields(fields...)
}

// SetBaggageItem is part of the opentracing.Span interface.
func (s *span) SetBaggageItem(restrictedKey, value string) opentracing.Span {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setBaggageItemLocked(restrictedKey, value)
}

func (s *span) setBaggageItemLocked(restrictedKey, value string) opentracing.Span {
	if oldVal, ok := s.mu.Baggage[restrictedKey]; ok && oldVal == value {
		// No-op.
		return s
	}
	if s.mu.Baggage == nil {
		s.mu.Baggage = make(map[string]string)
	}
	s.mu.Baggage[restrictedKey] = value

	if s.otSpan.shadowTr != nil {
		s.otSpan.shadowSpan.SetBaggageItem(restrictedKey, value)
	}
	// Also set a tag so it shows up in the Lightstep UI or x/net/trace.
	s.setTagInner(restrictedKey, value, true /* locked */)
	return s
}

// BaggageItem is part of the opentracing.Span interface.
func (s *span) BaggageItem(restrictedKey string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mu.Baggage[restrictedKey]
}

// Tracer is part of the opentracing.Span interface.
func (s *span) Tracer() opentracing.Tracer {
	return s.tracer
}

// LogEvent is part of the opentracing.Span interface. Deprecated.
func (s *span) LogEvent(event string) {
	s.LogFields(otlog.String(LogMessageField, event))
}

// LogEventWithPayload is part of the opentracing.Span interface. Deprecated.
func (s *span) LogEventWithPayload(event string, payload interface{}) {
	s.LogFields(otlog.String(LogMessageField, event), otlog.Object("payload", payload))
}

// Log is part of the opentracing.Span interface. Deprecated.
func (s *span) Log(data opentracing.LogData) {
	panic("unimplemented")
}

// getRecordingLocked returns the span's recording. This does not include
// children.
func (s *span) getRecordingLocked() RecordedSpan {
	rs := RecordedSpan{
		TraceID:      s.TraceID,
		SpanID:       s.SpanID,
		ParentSpanID: s.parentSpanID,
		Operation:    s.operation,
		StartTime:    s.startTime,
		Duration:     s.mu.duration,
	}

	addTag := func(k, v string) {
		if rs.Tags == nil {
			rs.Tags = make(map[string]string)
		}
		rs.Tags[k] = v
	}

	switch rs.Duration {
	case -1:
		// -1 indicates an unfinished span. For a recording it's better to put some
		// duration in it, otherwise tools get confused. For example, we export
		// recordings to Jaeger, and spans with a zero duration don't look nice.
		rs.Duration = timeutil.Now().Sub(rs.StartTime)
		addTag("unfinished", "")
	}

	if s.mu.stats != nil {
		stats, err := types.MarshalAny(s.mu.stats)
		if err != nil {
			panic(err)
		}
		rs.Stats = stats
	}

	if len(s.mu.Baggage) > 0 {
		rs.Baggage = make(map[string]string)
		for k, v := range s.mu.Baggage {
			rs.Baggage[k] = v
		}
	}
	if s.logTags != nil {
		tags := s.logTags.Get()
		for i := range tags {
			tag := &tags[i]
			addTag(tagName(tag.Key()), tag.ValueStr())
		}
	}
	if len(s.mu.tags) > 0 {
		for k, v := range s.mu.tags {
			// We encode the tag values as strings.
			addTag(k, fmt.Sprint(v))
		}
	}
	rs.Logs = make([]LogRecord, len(s.mu.recording.recordedLogs))
	for i, r := range s.mu.recording.recordedLogs {
		rs.Logs[i].Time = r.Timestamp
		rs.Logs[i].Fields = make([]LogRecord_Field, len(r.Fields))
		for j, f := range r.Fields {
			rs.Logs[i].Fields[j] = LogRecord_Field{
				Key:   f.Key(),
				Value: fmt.Sprint(f.Value()),
			}
		}
	}

	return rs
}

func (s *span) addChild(child *span) {
	s.mu.Lock()
	s.mu.recording.children = append(s.mu.recording.children, child)
	s.mu.Unlock()
}

type noopSpanContext struct{}

var _ opentracing.SpanContext = noopSpanContext{}

func (n noopSpanContext) ForeachBaggageItem(handler func(k, v string) bool) {}

type noopSpan struct {
	tracer *Tracer
}

var _ opentracing.Span = &noopSpan{}

func (n *noopSpan) Context() opentracing.SpanContext                       { return noopSpanContext{} }
func (n *noopSpan) BaggageItem(key string) string                          { return "" }
func (n *noopSpan) SetTag(key string, value interface{}) opentracing.Span  { return n }
func (n *noopSpan) Finish()                                                {}
func (n *noopSpan) FinishWithOptions(opts opentracing.FinishOptions)       {}
func (n *noopSpan) SetOperationName(operationName string) opentracing.Span { return n }
func (n *noopSpan) Tracer() opentracing.Tracer                             { return n.tracer }
func (n *noopSpan) LogFields(fields ...otlog.Field)                        {}
func (n *noopSpan) LogKV(keyVals ...interface{})                           {}
func (n *noopSpan) LogEvent(event string)                                  {}
func (n *noopSpan) LogEventWithPayload(event string, payload interface{})  {}
func (n *noopSpan) Log(data opentracing.LogData)                           {}

func (n *noopSpan) SetBaggageItem(key, val string) opentracing.Span {
	if key == Snowball {
		panic("attempting to set Snowball on a noop span; use the Recordable option to StartSpan")
	}
	return n
}
