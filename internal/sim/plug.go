package sim

import (
	"context"
	"errors"
	"io"
	"sync"

	"tramline/internal/hue"
)

// Plug is a simulated power switch, with a simulated printer behind it that
// only exists while the plug is on. It never talks to a real bridge.
type Plug struct {
	Options Options

	mu   sync.Mutex
	on   bool
	conn io.Closer
}

// Open connects to the printer, and fails like the real device while it's off.
func (p *Plug) Open() (io.ReadWriteCloser, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.on {
		return nil, errors.New("no such device")
	}
	if p.Options.Card == nil {
		p.Options.Card = NewCard()
	}
	conn := New(p.Options)
	p.conn = conn
	return conn, nil
}

func (p *Plug) Pair(context.Context) error { return nil }

func (p *Plug) Lights(ctx context.Context) ([]hue.Light, error) {
	state, err := p.State(ctx)
	return []hue.Light{state}, err
}

func (p *Plug) State(context.Context) (hue.Light, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return hue.Light{ID: "simulated", Name: "Simulated plug", Archetype: "plug", On: p.on}, nil
}

func (p *Plug) Set(_ context.Context, on bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.on = on
	if !on && p.conn != nil {
		p.conn.Close()
		p.conn = nil
	}
	if !on && p.Options.Card != nil {
		p.Options.Card.PowerLost()
	}
	return nil
}
