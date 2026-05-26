//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// JaegerEndpoints holds the mapped endpoints for the Jaeger container.
type JaegerEndpoints struct {
	OTLPEndpoint  string // host:port for OTLP HTTP (4318)
	QueryEndpoint string // http://host:port for Jaeger Query API (16686)
}

// StartJaeger starts a Jaeger all-in-one container and returns the mapped
// endpoints. The container is automatically terminated when the test finishes.
func StartJaeger(t *testing.T) JaegerEndpoints {
	t.Helper()
	ctx := context.Background()

	ctr, err := testcontainers.Run(ctx, "jaegertracing/all-in-one:latest",
		testcontainers.WithExposedPorts("4318/tcp", "16686/tcp"),
		testcontainers.WithEnv(map[string]string{
			"COLLECTOR_OTLP_ENABLED": "true",
		}),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/").WithPort("16686/tcp").WithStartupTimeout(30*time.Second),
		),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start jaeger container: %v", err)
	}

	otlp, err := ctr.PortEndpoint(ctx, "4318", "")
	if err != nil {
		t.Fatalf("get OTLP endpoint: %v", err)
	}

	query, err := ctr.PortEndpoint(ctx, "16686", "http")
	if err != nil {
		t.Fatalf("get query endpoint: %v", err)
	}

	return JaegerEndpoints{
		OTLPEndpoint:  otlp,
		QueryEndpoint: query,
	}
}

// Trace is a minimal representation of a Jaeger trace.
type Trace struct {
	TraceID string
	Spans   []Span
}

// Span is a minimal representation of a Jaeger span.
type Span struct {
	OperationName string
	Tags          map[string]string
}

// QueryTraces polls Jaeger's HTTP API until traces for the given service
// appear or the timeout expires. Returns all matching traces.
func QueryTraces(t *testing.T, queryEndpoint, service string, timeout time.Duration) []Trace {
	t.Helper()

	apiURL := fmt.Sprintf("%s/api/traces?service=%s&limit=100", queryEndpoint, url.QueryEscape(service))
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		resp, err := http.Get(apiURL)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}

		var result jaegerAPIResponse
		decErr := json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if decErr != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}

		if len(result.Data) > 0 {
			return convertTraces(result.Data)
		}
		time.Sleep(200 * time.Millisecond)
	}

	t.Fatalf("no traces found for service %q within %v", service, timeout)
	return nil
}

// SpansByName returns all spans with the given operation name across all traces.
func SpansByName(traces []Trace, name string) []Span {
	var out []Span
	for _, tr := range traces {
		for _, sp := range tr.Spans {
			if sp.OperationName == name {
				out = append(out, sp)
			}
		}
	}
	return out
}

// Jaeger API JSON structures — minimal subset for test assertions.

type jaegerAPIResponse struct {
	Data []jaegerTrace `json:"data"`
}

type jaegerTrace struct {
	TraceID string       `json:"traceID"`
	Spans   []jaegerSpan `json:"spans"`
}

type jaegerSpan struct {
	OperationName string      `json:"operationName"`
	Tags          []jaegerTag `json:"tags"`
}

type jaegerTag struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

func convertTraces(raw []jaegerTrace) []Trace {
	traces := make([]Trace, len(raw))
	for i, rt := range raw {
		spans := make([]Span, len(rt.Spans))
		for j, rs := range rt.Spans {
			tags := make(map[string]string, len(rs.Tags))
			for _, tag := range rs.Tags {
				tags[tag.Key] = fmt.Sprintf("%v", tag.Value)
			}
			spans[j] = Span{
				OperationName: rs.OperationName,
				Tags:          tags,
			}
		}
		traces[i] = Trace{
			TraceID: rt.TraceID,
			Spans:   spans,
		}
	}
	return traces
}
