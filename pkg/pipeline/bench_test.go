package pipeline

import (
	"context"
	"testing"

	apiEvents "gocache/api/events"
	"gocache/pkg/clientctx"
	serverEvents "gocache/pkg/events"
)

type benchCommandMetricsRecorder struct{}

func (benchCommandMetricsRecorder) HasCommandMetricsSink() bool { return true }

func (benchCommandMetricsRecorder) RecordCommand(string, uint64, bool) {}

func BenchmarkEvaluateSinks(b *testing.B) {
	b.Run("no_sink", func(b *testing.B) {
		eval, e, _ := newTestPipeline()
		defer e.Stop()
		ctx := clientctx.New()
		b.ResetTimer()
		for b.Loop() {
			_ = eval.Evaluate(context.Background(), ctx, "PING", nil)
		}
	})
	b.Run("log_event_only", func(b *testing.B) {
		eval, e, _ := newTestPipeline()
		defer e.Stop()
		bus := serverEvents.NewBusWithCapacity(0)
		bus.Subscribe("logs", []apiEvents.Type{apiEvents.LogEntry}, func(apiEvents.Event) {})
		eval.SetEmitter(bus)
		ctx := clientctx.New()
		b.ResetTimer()
		for b.Loop() {
			_ = eval.Evaluate(context.Background(), ctx, "PING", nil)
		}
	})
	b.Run("command_metrics_only", func(b *testing.B) {
		eval, e, _ := newTestPipeline()
		defer e.Stop()
		eval.SetCommandMetricsRecorder(benchCommandMetricsRecorder{})
		ctx := clientctx.New()
		b.ResetTimer()
		for b.Loop() {
			_ = eval.Evaluate(context.Background(), ctx, "PING", nil)
		}
	})
	b.Run("event_completed_only", func(b *testing.B) {
		eval, e, _ := newTestPipeline()
		defer e.Stop()
		bus := serverEvents.NewBusWithCapacity(0)
		bus.Subscribe("metrics", []apiEvents.Type{apiEvents.CommandCompleted}, func(apiEvents.Event) {})
		eval.SetEmitter(bus)
		ctx := clientctx.New()
		b.ResetTimer()
		for b.Loop() {
			_ = eval.Evaluate(context.Background(), ctx, "PING", nil)
		}
	})
	b.Run("full_events", func(b *testing.B) {
		eval, e, _ := newTestPipeline()
		defer e.Stop()
		bus := serverEvents.NewBusWithCapacity(0)
		bus.Subscribe("all", []apiEvents.Type{
			apiEvents.OperationStarted,
			apiEvents.CommandStarted,
			apiEvents.CommandCompleted,
			apiEvents.OperationCompleted,
		}, func(apiEvents.Event) {})
		eval.SetEmitter(bus)
		ctx := clientctx.New()
		b.ResetTimer()
		for b.Loop() {
			_ = eval.Evaluate(context.Background(), ctx, "PING", nil)
		}
	})
	b.Run("hook_only", func(b *testing.B) {
		eval, e, _ := newTestPipeline()
		defer e.Stop()
		eval.SetHookExecutor(&mockHookExecutor{hasAny: true})
		ctx := clientctx.New()
		b.ResetTimer()
		for b.Loop() {
			_ = eval.Evaluate(context.Background(), ctx, "PING", nil)
		}
	})
	b.Run("operation_hook_only", func(b *testing.B) {
		eval, e, _ := newTestPipeline()
		defer e.Stop()
		eval.SetOpHookExecutor(&mockOpHookExecutor{hasAny: true})
		ctx := clientctx.New()
		b.ResetTimer()
		for b.Loop() {
			_ = eval.Evaluate(context.Background(), ctx, "PING", nil)
		}
	})
}
