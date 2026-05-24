//go:build aof

package aof

import (
	"context"
	"fmt"
	"strings"

	apicommand "gocache/api/command"
	apiconfig "gocache/api/config"
	"gocache/api/logger"
	apipersistence "gocache/api/persistence"
)

func init() {
	apipersistence.RegisterProvider(&provider{})
}

const (
	keyFile  = "file"
	keyFsync = "fsync"

	defaultFile  = "appendonly.aof"
	defaultFsync = "everysec"
)

type provider struct {
	src  *AOFSource
	sink *AOFSink
}

func (provider) Name() string { return "aof" }

func (p *provider) Build(cfg apiconfig.PluginConfig, store apipersistence.CacheStore) (*apipersistence.Backend, error) {
	cfg.SetDefault(keyFile, defaultFile)
	cfg.SetDefault(keyFsync, defaultFsync)

	file := cfg.GetString(keyFile)
	if file == "" {
		return nil, fmt.Errorf("aof: %q is required", keyFile)
	}

	policy := parseFsync(cfg.GetString(keyFsync))

	sink, err := NewSink(file, policy)
	if err != nil {
		return nil, err
	}
	p.src = NewSource(file)
	p.sink = sink

	logger.InfoNoCtx().
		Str("plugin", "aof").Str("file", file).Str("fsync", policy.String()).
		Msg("aof: configured")

	cmds := p.commands(store)

	return &apipersistence.Backend{
		Source:   p.src,
		Sink:     sink,
		Commands: func(_ apipersistence.PersistenceAPI) []apipersistence.Command { return cmds },
		OnReload: p,
	}, nil
}

func (p *provider) commands(store apipersistence.CacheStore) []apipersistence.Command {
	sink := p.sink
	return []apipersistence.Command{
		{
			Name: "BGREWRITEAOF",
			Fn: func(_ context.Context, _ []string) (any, error) {
				go func() {
					if err := Rewrite(context.Background(), store, sink); err != nil {
						logger.ErrorNoCtx().Err(err).Msg("BGREWRITEAOF failed")
					}
				}()
				return "Background append only file rewriting started", nil
			},
			Spec: apicommand.Spec{Min: 0, Max: 0, KeyArgIndex: -1},
		},
	}
}

func (p *provider) OnConfigReload(cfg apiconfig.PluginConfig) {
	if p.sink == nil {
		return
	}
	if newFsync := cfg.GetString(keyFsync); newFsync != "" {
		p.sink.SetFsyncPolicy(parseFsync(newFsync))
	}
	if newFile := cfg.GetString(keyFile); newFile != "" {
		p.src.SetPath(newFile)
	}
}

func parseFsync(s string) apipersistence.FsyncPolicy {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "always":
		return apipersistence.FsyncAlways
	case "no":
		return apipersistence.FsyncNo
	default:
		return apipersistence.FsyncEverySec
	}
}
