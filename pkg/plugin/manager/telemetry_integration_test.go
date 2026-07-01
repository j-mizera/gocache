package manager

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apievents "gocache/api/events"
	gcpc "gocache/api/gcpc/v1"
	apiplugin "gocache/api/plugin"
	commonobs "gocache/commons/observability"
	"gocache/commons/transport"
	serverEvents "gocache/pkg/events"
	"gocache/pkg/plugin"
	serverpkg "gocache/pkg/server"
	"gocache/testkit/common/telemetrytest"
)

func (p *testPlugin) registerWithCommands(name string, scopes []string, commands []*gcpc.CommandDeclV1) *gcpc.RegisterAckV1 {
	p.t.Helper()
	reg := &gcpc.RegisterV1{
		Name:            name,
		Version:         "0.1.0-test",
		Critical:        false,
		RequestedScopes: scopes,
		Commands:        commands,
	}
	env := &gcpc.EnvelopeV1{
		Version: gcpc.ProtocolVersion,
		Payload: &gcpc.EnvelopeV1_Register{Register: reg},
	}
	if err := p.conn.Send(env); err != nil {
		p.t.Fatalf("send register: %v", err)
	}
	ackEnv, err := p.conn.Recv()
	if err != nil {
		p.t.Fatalf("recv ack: %v", err)
	}
	ack := ackEnv.GetRegisterAck()
	if ack == nil {
		p.t.Fatal("expected RegisterAck")
	}
	return ack
}

func TestIT_PluginManagerTelemetryEventsReachEventBus(t *testing.T) {
	mgr, eventBus, sockPath := setupManager(t)
	trackerManager := newManagerTelemetryTracker()
	mgr.SetOperationTrackerManager(trackerManager)
	inst, ok := mgr.registry.Get("test-plugin")
	if !ok {
		t.Fatal("test-plugin instance should be registered")
	}
	mgr.startPluginLifecycleOp(inst)

	received := make(chan apievents.Event, 16)
	eventBus.Subscribe("test-plugin-manager-telemetry", []apievents.Type{
		apievents.PluginRegistered,
		apievents.PluginStopped,
		apievents.PluginRegistrationFailed,
		apievents.PluginCommandRegistered,
	}, func(event apievents.Event) {
		received <- event
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	worker := serverpkg.NewOperationTrackerDrainWorker(trackerManager, time.Millisecond)
	worker.SetEmitter(eventBus)
	worker.Start(ctx)
	t.Cleanup(worker.Stop)
	waiter := telemetrytest.NewEventWaiter(t, worker, received)

	p := newTestPlugin(t, sockPath)
	commands := []*gcpc.CommandDeclV1{{Name: "PLUGINPING", Readonly: true}}
	ack := p.registerWithCommands("test-plugin", []string{"read"}, commands)
	if !ack.Accepted {
		t.Fatalf("registration rejected: %s", ack.Reason)
	}

	registered := waiter.Wait("plugin.registered", func(event apievents.Event) bool {
		payload := event.Proto.GetPluginRegistered()
		return event.Proto.Type == string(apievents.PluginRegistered) && payload != nil && payload.Name == "test-plugin" && payload.Version == "0.1.0-test"
	})
	if registered.Proto.OperationId == "" {
		t.Fatal("plugin.registered should carry operation_id")
	}

	commandRegistered := waiter.Wait("plugin.command.registered", func(event apievents.Event) bool {
		payload := event.Proto.GetPluginCommandRegistered()
		return event.Proto.Type == string(apievents.PluginCommandRegistered) && payload != nil && payload.Name == "test-plugin" && payload.Command == "PLUGINPING" && payload.Readonly
	})
	if commandRegistered.Proto.OperationId == "" {
		t.Fatal("plugin.command.registered should carry operation_id")
	}

	p.send(gcpc.NewShutdownAck())
	stopped := waiter.Wait("plugin.stopped", func(event apievents.Event) bool {
		payload := event.Proto.GetPluginStopped()
		return event.Proto.Type == string(apievents.PluginStopped) && payload != nil && payload.Name == "test-plugin" && payload.Reason == "shutdown ack"
	})
	if stopped.Proto.OperationId == "" {
		t.Fatal("plugin.stopped should carry operation_id")
	}

	unknown := newTestPlugin(t, sockPath)
	unknownAck := unknown.registerWithCommands("missing-plugin", []string{"read"}, nil)
	if unknownAck.Accepted {
		t.Fatal("unknown plugin registration should be rejected")
	}
	failed := waiter.Wait("plugin.registration_failed", func(event apievents.Event) bool {
		payload := event.Proto.GetPluginRegistrationFailed()
		return event.Proto.Type == string(apievents.PluginRegistrationFailed) && payload != nil && payload.Name == "missing-plugin" && payload.Error == "unknown plugin"
	})
	if failed.Proto.OperationId == "" {
		t.Fatal("plugin.registration_failed should carry operation_id")
	}
}

func TestIT_PluginManagerTelemetryPublishesStoppedFromProcessShutdown(t *testing.T) {
	eventBus := serverEvents.NewBus()
	trackerManager := newManagerTelemetryTracker()
	pluginDir := t.TempDir()
	sockPath := filepath.Join(t.TempDir(), "plugin.sock")
	pluginName := "dummy-plugin"
	writeDummyPluginWrapper(t, filepath.Join(pluginDir, pluginName))

	mgr := NewManager(plugin.PluginsConfig{
		Enabled:         true,
		Dir:             pluginDir,
		SocketPath:      sockPath,
		HealthInterval:  time.Hour,
		ShutdownTimeout: 2 * time.Second,
		MaxRestarts:     0,
		ConnectTimeout:  5 * time.Second,
	}, []string{"GET", "SET", "PING"}, &mockState{})
	mgr.SetEventBus(eventBus)
	mgr.SetOperationTrackerManager(trackerManager)

	received := make(chan apievents.Event, 16)
	eventBus.Subscribe("test-plugin-process-shutdown-telemetry", []apievents.Type{
		apievents.PluginRegistered,
		apievents.PluginStarted,
		apievents.PluginStopped,
	}, func(event apievents.Event) {
		received <- event
	})

	ctx, cancel := context.WithCancel(context.Background())
	shutdownDone := false
	t.Cleanup(func() {
		cancel()
		if !shutdownDone {
			mgr.Shutdown(time.Second)
		}
	})
	worker := serverpkg.NewOperationTrackerDrainWorker(trackerManager, time.Millisecond)
	worker.SetEmitter(eventBus)
	worker.Start(ctx)
	t.Cleanup(worker.Stop)
	waiter := telemetrytest.NewEventWaiter(t, worker, received)

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("start plugin manager: %v", err)
	}

	started := waiter.Wait("dummy plugin.started", func(event apievents.Event) bool {
		payload := event.Proto.GetPluginStarted()
		return event.Proto.Type == string(apievents.PluginStarted) && payload != nil && payload.Name == pluginName && payload.Pid > 0
	})
	if started.Proto.OperationId == "" {
		t.Fatal("dummy plugin.started should carry operation_id")
	}

	registered := waiter.Wait("dummy plugin.registered", func(event apievents.Event) bool {
		payload := event.Proto.GetPluginRegistered()
		return event.Proto.Type == string(apievents.PluginRegistered) && payload != nil && payload.Name == pluginName && payload.Version == "0.1.0-dummy"
	})
	if registered.Proto.OperationId == "" {
		t.Fatal("dummy plugin.registered should carry operation_id")
	}

	mgr.Shutdown(2 * time.Second)
	shutdownDone = true
	cancel()

	stopped := waiter.Wait("dummy plugin.stopped", func(event apievents.Event) bool {
		payload := event.Proto.GetPluginStopped()
		return event.Proto.Type == string(apievents.PluginStopped) && payload != nil && payload.Name == pluginName && payload.Reason == "shutdown ack"
	})
	if stopped.Proto.OperationId == "" {
		t.Fatal("dummy plugin.stopped should carry operation_id")
	}
}

func TestIT_PluginManagerTelemetryPublishesPluginStartedFromLaunch(t *testing.T) {
	eventBus := serverEvents.NewBus()
	trackerManager := newManagerTelemetryTracker()
	mgr := NewManager(plugin.PluginsConfig{
		SocketPath:     filepath.Join(t.TempDir(), "plugin.sock"),
		HealthInterval: time.Hour,
		MaxRestarts:    0,
	}, nil, &mockState{})
	mgr.SetOperationTrackerManager(trackerManager)

	received := make(chan apievents.Event, 8)
	eventBus.Subscribe("test-plugin-started-telemetry", []apievents.Type{apievents.PluginStarted}, func(event apievents.Event) {
		received <- event
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	worker := serverpkg.NewOperationTrackerDrainWorker(trackerManager, time.Millisecond)
	worker.SetEmitter(eventBus)
	worker.Start(ctx)
	t.Cleanup(worker.Stop)
	waiter := telemetrytest.NewEventWaiter(t, worker, received)

	binPath := filepath.Join(t.TempDir(), "sleep-plugin.sh")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("write plugin fixture: %v", err)
	}
	inst := &PluginInstance{Name: "started-plugin", BinPath: binPath, MaxRestarts: 0}
	inst.setCriticalAtLoad(false)
	mgr.registry.Add(inst)
	mgr.launchPlugin(ctx, inst)
	if inst.Cmd() == nil || inst.Cmd().Process == nil {
		t.Fatal("plugin process should be started")
	}

	started := waiter.Wait("plugin.started", func(event apievents.Event) bool {
		payload := event.Proto.GetPluginStarted()
		return event.Proto.Type == string(apievents.PluginStarted) && payload != nil && payload.Name == "started-plugin" && payload.Pid > 0
	})
	if started.Proto.OperationId == "" {
		t.Fatal("plugin.started should carry operation_id")
	}

	cancel()
	waitForManagerWaitGroup(t, mgr)
}

func newManagerTelemetryTracker() *commonobs.SlotOperationTrackerManager {
	return commonobs.NewSlotOperationTrackerManager(commonobs.SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           32,
		CompletedRingPerShard: 32,
	})
}

const dummyPluginProcessEnv = "GOCACHE_DUMMY_PLUGIN_PROCESS"

func TestHelperProcessDummyPlugin(t *testing.T) {
	if os.Getenv(dummyPluginProcessEnv) != "1" {
		return
	}
	if err := runDummyPluginProcess(); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func writeDummyPluginWrapper(t *testing.T, path string) {
	t.Helper()
	quotedTestBinary := "'" + strings.ReplaceAll(os.Args[0], "'", "'\\''") + "'"
	script := "#!/bin/sh\n" + dummyPluginProcessEnv + "=1 exec " + quotedTestBinary + " -test.run '^TestHelperProcessDummyPlugin$'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write dummy plugin wrapper: %v", err)
	}
}

func runDummyPluginProcess() error {
	sockPath := os.Getenv(apiplugin.EnvSocketPath)
	if sockPath == "" {
		return transport.ErrConnClosed
	}
	raw, err := net.Dial("unix", sockPath)
	if err != nil {
		return err
	}
	conn := transport.NewConn(raw)
	defer conn.Close()
	if err := conn.Send(gcpc.NewRegister("dummy-plugin", "0.1.0-dummy", false, nil, 0)); err != nil {
		return err
	}
	ackEnv, err := conn.Recv()
	if err != nil {
		return err
	}
	ack := ackEnv.GetRegisterAck()
	if ack == nil || !ack.Accepted {
		return transport.ErrConnClosed
	}
	for {
		env, err := conn.Recv()
		if err != nil {
			return err
		}
		if env.GetShutdown() != nil {
			return conn.Send(gcpc.NewShutdownAck())
		}
		if env.GetHealthCheck() != nil {
			if err := conn.Send(gcpc.NewHealthResponse(true, "ok")); err != nil {
				return err
			}
		}
	}
}

func waitForManagerWaitGroup(t *testing.T, mgr *Manager) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		mgr.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("plugin manager goroutines did not stop")
	}
}
