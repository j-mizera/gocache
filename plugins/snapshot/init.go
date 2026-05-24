package snapshot

import (
	"context"
	"fmt"

	apicommand "gocache/api/command"
	apiconfig "gocache/api/config"
	"gocache/api/logger"
	apipersistence "gocache/api/persistence"
)

func init() {
	apipersistence.RegisterProvider(&provider{})
}

const keyFile = "file"
const defaultFile = "snapshot.dat"

type provider struct {
	src  *Source
	snap *Snapshotter
}

func (provider) Name() string { return "snapshot" }

func (p *provider) Build(cfg apiconfig.PluginConfig, _ apipersistence.CacheStore) (*apipersistence.Backend, error) {
	cfg.SetDefault(keyFile, defaultFile)
	cfg.BindEnv(keyFile, "GOCACHE_SNAPSHOT_FILE")
	file := cfg.GetString(keyFile)
	if file == "" {
		return nil, fmt.Errorf("snapshot: %q is required", keyFile)
	}

	p.src = NewSource(file)
	p.snap = NewSnapshotter(file)

	logger.InfoNoCtx().Str("plugin", "snapshot").Str("file", file).Msg("snapshot: configured")
	return &apipersistence.Backend{
		Source:      p.src,
		Snapshotter: p.snap,
		Commands:    p.commands,
		OnReload:    p,
	}, nil
}

func (p *provider) commands(api apipersistence.PersistenceAPI) []apipersistence.Command {
	save := func(ctx context.Context, _ []string) (any, error) {
		if err := api.Snapshot(ctx); err != nil {
			logger.Error(ctx).Err(err).Msg("snapshot command failed")
			return nil, err
		}
		return "OK", nil
	}

	bgsave := func(_ context.Context, _ []string) (any, error) {
		go func() {
			if err := api.Snapshot(context.Background()); err != nil {
				logger.ErrorNoCtx().Err(err).Msg("BGSAVE failed")
			}
		}()
		return "Background saving started", nil
	}

	lastsave := func(_ context.Context, _ []string) (any, error) {
		return api.LastSaveUnix(), nil
	}

	return []apipersistence.Command{
		{Name: "SNAPSHOT", Fn: save, Spec: apicommand.Spec{Min: 0, Max: 0, MultiKey: true}},
		{Name: "SAVE", Fn: save, Spec: apicommand.Spec{Min: 0, Max: 0, MultiKey: true}},
		{Name: "BGSAVE", Fn: bgsave, Spec: apicommand.Spec{Min: 0, Max: 0, KeyArgIndex: -1}},
		{Name: "LASTSAVE", Fn: lastsave, Spec: apicommand.Spec{Min: 0, Max: 0, ReadOnly: true, KeyArgIndex: -1}},
	}
}

func (p *provider) OnConfigReload(cfg apiconfig.PluginConfig) {
	if p.src == nil || p.snap == nil {
		return
	}
	newFile := cfg.GetString(keyFile)
	if newFile == "" {
		return
	}
	p.src.SetFilename(newFile)
	p.snap.SetFilename(newFile)
	logger.InfoNoCtx().Str("plugin", "snapshot").Str("file", newFile).Msg("snapshot: filename updated via hot reload")
}
