package benchsuite

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const gitRevParseTimeout = 5 * time.Second

// BaselineProvenance records enough environment detail to compare benchmark runs.
type BaselineProvenance struct {
	CommitSHA    string `json:"commit_sha"`
	GoVersion    string `json:"go_version"`
	Date         string `json:"date"`
	GOARCH       string `json:"goarch"`
	NumCPU       int    `json:"num_cpu"`
	Hardware     string `json:"hardware"`
	SuiteVersion string `json:"suite_version"` // benchmark suite version (v1-pre-aof)
	SuiteScope   string `json:"suite_scope"`   // scope of version numbers (fire-and-forget = send-cost only)
}

// CaptureBaselineProvenance captures current commit and runtime provenance.
func CaptureBaselineProvenance(ctx context.Context, repoRoot string) (BaselineProvenance, error) {
	commitSHA, commitErr := captureCommitSHA(ctx, repoRoot)
	if commitErr != nil {
		return BaselineProvenance{}, commitErr
	}

	goarch := runtime.GOARCH
	numCPU := runtime.NumCPU()
	return BaselineProvenance{
		CommitSHA:    commitSHA,
		GoVersion:    runtime.Version(),
		Date:         time.Now().UTC().Format(time.RFC3339),
		GOARCH:       goarch,
		NumCPU:       numCPU,
		Hardware:     fmt.Sprintf("%s/%dCPU", goarch, numCPU),
		SuiteVersion: SuiteVersion,
		SuiteScope:   SuiteScope,
	}, nil
}

// LockBaseline writes baseline provenance under bench/results and returns the file path.
func LockBaseline(ctx context.Context, repoRoot string) (string, BaselineProvenance, error) {
	provenance, provenanceErr := CaptureBaselineProvenance(ctx, repoRoot)
	if provenanceErr != nil {
		return "", BaselineProvenance{}, provenanceErr
	}

	resultsDir := filepath.Join(repoRoot, "bench", "results")
	if mkdirErr := os.MkdirAll(resultsDir, 0o755); mkdirErr != nil {
		return "", BaselineProvenance{}, fmt.Errorf("create baseline results directory: %w", mkdirErr)
	}

	filenameDate := strings.NewReplacer(":", "", "-", "").Replace(provenance.Date)
	baselinePath := filepath.Join(resultsDir, "baseline-"+filenameDate+".json")
	encodedProvenance, marshalErr := json.MarshalIndent(provenance, "", "  ")
	if marshalErr != nil {
		return "", BaselineProvenance{}, fmt.Errorf("encode baseline provenance: %w", marshalErr)
	}
	encodedProvenance = append(encodedProvenance, '\n')
	if writeErr := os.WriteFile(baselinePath, encodedProvenance, 0o644); writeErr != nil {
		return "", BaselineProvenance{}, fmt.Errorf("write baseline provenance: %w", writeErr)
	}
	return baselinePath, provenance, nil
}

func captureCommitSHA(ctx context.Context, repoRoot string) (string, error) {
	gitCtx, cancel := context.WithTimeout(ctx, gitRevParseTimeout)
	defer cancel()

	gitCommand := exec.CommandContext(gitCtx, "git", "-C", repoRoot, "rev-parse", "HEAD")
	gitCommand.Stdin = nil
	commitBytes, commandErr := gitCommand.Output()
	if gitCtx.Err() != nil {
		return "", fmt.Errorf("capture git commit SHA: %w", gitCtx.Err())
	}
	if commandErr != nil {
		return "", fmt.Errorf("capture git commit SHA: %w", commandErr)
	}
	commitSHA := strings.TrimSpace(string(commitBytes))
	if commitSHA == "" {
		return "", fmt.Errorf("capture git commit SHA: empty output")
	}
	return commitSHA, nil
}
