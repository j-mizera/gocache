package protocol

import (
	gcpc "gocache/proto/gcpc/v1"
	"testing"
	"time"
)

func TestNewRegister(t *testing.T) {
	env := NewRegister("auth", "1.0.0", true, nil, 0)
	if env.Id == 0 {
		t.Error("expected non-zero ID")
	}
	reg := env.GetRegister()
	if reg == nil {
		t.Fatal("expected Register payload")
	}
	if reg.Name != "auth" || reg.Version != "1.0.0" || !reg.Critical {
		t.Errorf("unexpected: %+v", reg)
	}
}

func TestNewRegisterAck(t *testing.T) {
	env := NewRegisterAck(true, "", nil)
	ack := env.GetRegisterAck()
	if ack == nil {
		t.Fatal("expected RegisterAck payload")
	}
	if !ack.Accepted {
		t.Error("expected accepted=true")
	}

	env2 := NewRegisterAck(false, "unknown", nil)
	ack2 := env2.GetRegisterAck()
	if ack2.Accepted {
		t.Error("expected accepted=false")
	}
	if ack2.Reason != "unknown" {
		t.Errorf("expected reason 'unknown', got %q", ack2.Reason)
	}
}

func TestNewHealthCheck(t *testing.T) {
	env := NewHealthCheck()
	hc := env.GetHealthCheck()
	if hc == nil {
		t.Fatal("expected HealthCheck payload")
	}
	if hc.Timestamp == 0 {
		t.Error("expected non-zero timestamp")
	}
}

func TestNewHealthResponse(t *testing.T) {
	env := NewHealthResponse(true, "all good")
	hr := env.GetHealthResponse()
	if hr == nil {
		t.Fatal("expected HealthResponse payload")
	}
	if !hr.Ok || hr.Status != "all good" {
		t.Errorf("unexpected: %+v", hr)
	}
}

func TestNewShutdown(t *testing.T) {
	deadline := time.Now().Add(5 * time.Second)
	env := NewShutdown(deadline)
	sd := env.GetShutdown()
	if sd == nil {
		t.Fatal("expected Shutdown payload")
	}
	if sd.DeadlineNs != uint64(deadline.UnixNano()) {
		t.Errorf("deadline mismatch: got %d, want %d", sd.DeadlineNs, deadline.UnixNano())
	}
}

func TestNewShutdownAck(t *testing.T) {
	env := NewShutdownAck()
	sa := env.GetShutdownAck()
	if sa == nil {
		t.Fatal("expected ShutdownAck payload")
	}
}

func TestIDAutoIncrements(t *testing.T) {
	env1 := NewHealthCheck()
	env2 := NewHealthCheck()
	if env2.Id <= env1.Id {
		t.Errorf("expected IDs to increment: %d <= %d", env2.Id, env1.Id)
	}
}

func TestAllPayloadTypes(t *testing.T) {
	tests := []struct {
		name string
		env  *gcpc.EnvelopeV1
	}{
		{"Register", NewRegister("x", "1", false, nil, 0)},
		{"RegisterAck", NewRegisterAck(true, "", nil)},
		{"HealthCheck", NewHealthCheck()},
		{"HealthResponse", NewHealthResponse(true, "")},
		{"Shutdown", NewShutdown(time.Now())},
		{"ShutdownAck", NewShutdownAck()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env.Payload == nil {
				t.Error("payload is nil")
			}
			if tt.env.Id == 0 {
				t.Error("ID is zero")
			}
		})
	}
}
