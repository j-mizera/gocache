package handler

import (
	"fmt"
	"path/filepath"
	"strings"

	"gocache/pkg/command"
	"gocache/pkg/logger"
	"gocache/pkg/persistence"
	"gocache/pkg/resp"
)

func HandleSnapshot(cmdCtx *command.Context) command.Result {
	executeFn := func() interface{} {
		if err := persistence.SaveSnapshot(cmdCtx.SnapshotFile, cmdCtx.Cache); err != nil {
			return err
		}
		return "OK"
	}
	res := command.Dispatch(cmdCtx, executeFn)
	if res.Err != nil {
		logger.Error().Err(res.Err).Msg("snapshot command failed")
	}
	return res
}

func HandleLoadSnapshot(cmdCtx *command.Context) command.Result {
	requested := cmdCtx.Args[0]

	// Restrict LOAD_SNAPSHOT to files within the configured snapshot directory
	// to prevent arbitrary file reads via path traversal. Authenticated clients
	// must not be able to read /etc/passwd or other files readable by the
	// server user.
	filename, err := sanitizeSnapshotPath(cmdCtx.SnapshotFile, requested)
	if err != nil {
		return command.Result{Value: resp.MarshalError("ERR " + err.Error())}
	}

	executeFn := func() interface{} {
		if err := persistence.LoadSnapshot(filename, cmdCtx.Cache); err != nil {
			return err
		}
		return "OK"
	}
	res := command.Dispatch(cmdCtx, executeFn)
	if res.Err != nil {
		logger.Error().Err(res.Err).Str("file", filename).Msg("loadsnapshot command failed")
	}
	return res
}

// sanitizeSnapshotPath returns an absolute path guaranteed to be within the
// directory of baseSnapshotFile. It rejects absolute paths, parent-directory
// traversal, and any path that escapes the base directory after cleaning.
func sanitizeSnapshotPath(baseSnapshotFile, requested string) (string, error) {
	if baseSnapshotFile == "" {
		return "", fmt.Errorf("snapshots disabled: no snapshot_file configured")
	}
	if requested == "" {
		return "", fmt.Errorf("empty filename")
	}
	// Reject absolute paths and explicit traversal up-front.
	if filepath.IsAbs(requested) || strings.Contains(requested, "..") {
		return "", fmt.Errorf("path not allowed")
	}

	baseDir, err := filepath.Abs(filepath.Dir(baseSnapshotFile))
	if err != nil {
		return "", fmt.Errorf("resolve base dir: %w", err)
	}
	// filepath.Base on requested strips any remaining separators, leaving only
	// a file name. Combined with the absolute-path/".." rejection above this
	// guarantees the result stays in baseDir.
	final := filepath.Join(baseDir, filepath.Base(requested))

	// Defense in depth: verify the result is within baseDir after cleaning.
	cleaned := filepath.Clean(final)
	if !strings.HasPrefix(cleaned, baseDir+string(filepath.Separator)) && cleaned != baseDir {
		return "", fmt.Errorf("path not allowed")
	}
	return cleaned, nil
}
