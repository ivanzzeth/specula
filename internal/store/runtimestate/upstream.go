package runtimestate

import (
	"context"

	"github.com/ivanzzeth/specula/internal/upstream"
)

// protocolBlockPersister adapts a shared BlockStore to upstream.BlockPersister
// for one protocol namespace.
type protocolBlockPersister struct {
	protocol string
	store    BlockStore
}

var _ upstream.BlockPersister = (*protocolBlockPersister)(nil)

// BlockPersisterForProtocol returns a BlockPersister scoped to protocol.
func BlockPersisterForProtocol(store BlockStore, protocol string) upstream.BlockPersister {
	return &protocolBlockPersister{protocol: protocol, store: store}
}

func (p *protocolBlockPersister) Load(ctx context.Context, name string) (upstream.BlockState, error) {
	st, err := p.store.Get(ctx, p.protocol, name)
	if err != nil {
		return upstream.BlockState{}, err
	}
	return upstream.BlockState{Failures: st.Failures, BlockedUntil: st.BlockedUntil}, nil
}

func (p *protocolBlockPersister) Save(ctx context.Context, name string, state upstream.BlockState) error {
	return p.store.Set(ctx, p.protocol, name, BlockState{
		Failures:     state.Failures,
		BlockedUntil: state.BlockedUntil,
	})
}

func (p *protocolBlockPersister) Delete(ctx context.Context, name string) error {
	return p.store.Clear(ctx, p.protocol, name)
}
