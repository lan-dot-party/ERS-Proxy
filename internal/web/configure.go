package web

import (
	"context"

	"github.com/lan-dot-party/ers-proxy/internal/store"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg"
)

// Configurator changes switch settings (*switchcfg.Manager). It is nil unless
// configure.enabled is set in config.yaml; then the server registers no
// configuration routes and shows no configuration controls.
type Configurator interface {
	Capabilities(ctx context.Context, deviceID int64) (switchcfg.Capabilities, bool)
	Preview(ctx context.Context, deviceID int64, who switchcfg.Actor, c switchcfg.Change) (*switchcfg.Preview, error)
	PreviewByID(id string, who switchcfg.Actor) (*switchcfg.Preview, bool)
	Apply(ctx context.Context, previewID, hash string, ack bool, who switchcfg.Actor) (*store.ConfigChange, error)
	Confirm(ctx context.Context, id int64, who switchcfg.Actor) (*store.ConfigChange, error)
	Revert(ctx context.Context, id int64, who switchcfg.Actor) (*store.ConfigChange, error)
}

var _ Configurator = (*switchcfg.Manager)(nil)
