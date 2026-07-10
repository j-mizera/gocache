package benchsuite

import "fmt"

// TransportKind names the transport under benchmark.
type TransportKind string

const (
	// TransportNetPipe uses net.Pipe for in-process command transport.
	TransportNetPipe TransportKind = "net.Pipe"
	// TransportAFUnix uses an AF_UNIX socket for IPC-shaped command transport.
	TransportAFUnix TransportKind = "AF_UNIX"
)

// BenchmarkDimensions describes one table-driven benchmark case.
type BenchmarkDimensions struct {
	Transport     TransportKind
	Hooks         int
	Plugins       int
	PayloadSize   int
	FanoutDepth   int
	PipelineDepth int
}

// Name returns a stable sub-benchmark name for the dimension case.
func (dimensions BenchmarkDimensions) Name() string {
	return fmt.Sprintf(
		"transport=%s/hooks=%d/plugins=%d/payload=%dB/fanout=%d/pipeline=%d",
		dimensions.Transport,
		dimensions.Hooks,
		dimensions.Plugins,
		dimensions.PayloadSize,
		dimensions.FanoutDepth,
		dimensions.PipelineDepth,
	)
}

// DimensionMatrix is a table of benchmark dimension cases.
type DimensionMatrix []BenchmarkDimensions

// Cases returns a copy of the matrix cases for callers that need inspection.
func (matrix DimensionMatrix) Cases() []BenchmarkDimensions {
	benchmarkCases := make([]BenchmarkDimensions, len(matrix))
	copy(benchmarkCases, matrix)
	return benchmarkCases
}

// DimensionOptions lists per-axis values for Cartesian matrix generation.
type DimensionOptions struct {
	Transports     []TransportKind
	Hooks          []int
	Plugins        []int
	PayloadSizes   []int
	FanoutDepths   []int
	PipelineDepths []int
}

// EnumerateDimensions creates the Cartesian product of all dimension axes.
func EnumerateDimensions(options DimensionOptions) DimensionMatrix {
	matrixSize := len(options.Transports) * len(options.Hooks) * len(options.Plugins) * len(options.PayloadSizes) * len(options.FanoutDepths) * len(options.PipelineDepths)
	if matrixSize == 0 {
		return nil
	}

	benchmarkCases := make(DimensionMatrix, 0, matrixSize)
	for _, transport := range options.Transports {
		for _, hookCount := range options.Hooks {
			for _, pluginCount := range options.Plugins {
				for _, payloadSize := range options.PayloadSizes {
					for _, fanoutDepth := range options.FanoutDepths {
						for _, pipelineDepth := range options.PipelineDepths {
							benchmarkCases = append(benchmarkCases, BenchmarkDimensions{
								Transport:     transport,
								Hooks:         hookCount,
								Plugins:       pluginCount,
								PayloadSize:   payloadSize,
								FanoutDepth:   fanoutDepth,
								PipelineDepth: pipelineDepth,
							})
						}
					}
				}
			}
		}
	}
	return benchmarkCases
}
