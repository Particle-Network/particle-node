// SPDX-License-Identifier: BUSL-1.1
//
// Copyright (C) 2023, Berachain Foundation. All rights reserved.
// Use of this software is govered by the Business Source License included
// in the LICENSE file of this repository and at www.mariadb.com/bsl11.
//
// ANY USE OF THE LICENSED WORK IN VIOLATION OF THIS LICENSE WILL AUTOMATICALLY
// TERMINATE YOUR RIGHTS UNDER THIS LICENSE FOR THE CURRENT AND ALL OTHER
// VERSIONS OF THE LICENSED WORK.
//
// THIS LICENSE DOES NOT GRANT YOU ANY RIGHT IN ANY TRADEMARK OR LOGO OF
// LICENSOR OR ITS AFFILIATES (PROVIDED THAT YOU MAY USE A TRADEMARK OR LOGO OF
// LICENSOR AS EXPRESSLY REQUIRED BY THIS LICENSE).
//
// TO THE EXTENT PERMITTED BY APPLICABLE LAW, THE LICENSED WORK IS PROVIDED ON
// AN “AS IS” BASIS. LICENSOR HEREBY DISCLAIMS ALL WARRANTIES AND CONDITIONS,
// EXPRESS OR IMPLIED, INCLUDING (WITHOUT LIMITATION) WARRANTIES OF
// MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE, NON-INFRINGEMENT, AND
// TITLE.

package runtime

import (
	"context"
	"time"

	cosmoslog "cosmossdk.io/log"
	storetypes "cosmossdk.io/store/types"

	libtx "github.com/berachain/polaris/cosmos/lib/tx"
	antelib "github.com/berachain/polaris/cosmos/runtime/ante"
	"github.com/berachain/polaris/cosmos/runtime/chain"
	"github.com/berachain/polaris/cosmos/runtime/comet"
	"github.com/berachain/polaris/cosmos/runtime/miner"
	"github.com/berachain/polaris/cosmos/runtime/txpool"
	evmkeeper "github.com/berachain/polaris/cosmos/x/evm/keeper"
	evmtypes "github.com/berachain/polaris/cosmos/x/evm/types"
	"github.com/berachain/polaris/eth"
	"github.com/berachain/polaris/eth/consensus"
	"github.com/berachain/polaris/eth/core"
	"github.com/berachain/polaris/eth/node"

	abci "github.com/cometbft/cometbft/abci/types"

	"github.com/cosmos/cosmos-sdk/client"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/mempool"

	"github.com/ethereum/go-ethereum/beacon/engine"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	ethlog "github.com/ethereum/go-ethereum/log"
)

// EVMKeeper is an interface that defines the methods needed for the EVM setup.
type EVMKeeper interface {
	// Setup initializes the EVM keeper.
	Setup(evmkeeper.WrappedBlockchain) error
	SetLatestQueryContext(context.Context) error
}

// CosmosApp is an interface that defines the methods needed for the Cosmos setup.
type CosmosApp interface {
	SetPrepareProposal(sdk.PrepareProposalHandler)
	SetProcessProposal(sdk.ProcessProposalHandler)
	SetMempool(mempool.Mempool)
	SetAnteHandler(sdk.AnteHandler)
	TxDecode(txBz []byte) (sdk.Tx, error)
	BeginBlocker(sdk.Context) (sdk.BeginBlock, error)
	PreBlocker(sdk.Context, *abci.RequestFinalizeBlock) (*sdk.ResponsePreBlock, error)
}

// Polaris is a struct that wraps the Polaris struct from the polar package.
// It also includes wrapped versions of the Geth Miner and TxPool.
type Polaris struct {
	*eth.ExecutionLayer
	// WrappedMiner is a wrapped version of the Miner component.
	WrappedMiner *miner.Miner
	// WrappedTxPool is a wrapped version of the Mempool component.
	WrappedTxPool *txpool.Mempool
	// WrappedBlockchain is a wrapped version of the Blockchain component.
	WrappedBlockchain *chain.WrappedBlockchain
	// logger is the underlying logger supplied by the sdk.
	logger cosmoslog.Logger
}

// New creates a new Polaris runtime from the provided dependencies.
func New(
	cfg *eth.Config,
	logger cosmoslog.Logger,
	host core.PolarisHostChain,
	engine consensus.Engine,
) *Polaris {
	var err error
	p := &Polaris{
		logger: logger,
	}

	p.ExecutionLayer, err = eth.New(
		"geth", cfg, host, engine, cfg.Node.AllowUnprotectedTxs,
		ethlog.NewLogger(newEthHandler(logger)),
	)
	if err != nil {
		panic(err)
	}

	p.WrappedTxPool = txpool.New(p.Backend().Blockchain(), p.Backend().TxPool(), cfg.Polar.LegacyTxPool.Lifetime)

	return p
}

// Build is a function that sets up the Polaris struct.
// It takes a BaseApp and an EVMKeeper as arguments.
// It returns an error if the setup fails.
func (p *Polaris) Build(
	app CosmosApp, cosmHandler sdk.AnteHandler, ek EVMKeeper, allowedValMsgs map[string]sdk.Msg,
) error {
	// Wrap the geth miner and txpool with the cosmos miner and txpool.
	p.WrappedMiner = miner.New(p.Backend().Miner(), app, ek, allowedValMsgs)
	p.WrappedBlockchain = chain.New(p.Backend().Blockchain(), app)

	app.SetMempool(p.WrappedTxPool)
	app.SetPrepareProposal(p.WrappedMiner.PrepareProposal)
	app.SetProcessProposal(p.WrappedBlockchain.ProcessProposal)

	if err := ek.Setup(p.WrappedBlockchain); err != nil {
		return err
	}

	app.SetAnteHandler(
		antelib.NewAnteHandler(p.WrappedTxPool, cosmHandler).AnteHandler(),
	)

	return nil
}

// SetupServices initializes and registers the services with Polaris.
// It takes a client context as an argument and returns an error if the setup fails.
func (p *Polaris) SetupServices(clientCtx client.Context) error {
	// Initialize the miner with a new execution payload serializer.
	p.WrappedMiner.Init(libtx.NewSerializer[*engine.ExecutionPayloadEnvelope](
		clientCtx.TxConfig, evmtypes.WrapPayload))

	// Initialize the txpool with a new transaction serializer.
	p.WrappedTxPool.Init(p.logger, clientCtx, libtx.NewSerializer[*ethtypes.Transaction](
		clientCtx.TxConfig, evmtypes.WrapTx))

	// Register services with Polaris.
	p.RegisterLifecycles([]node.Lifecycle{
		p.WrappedTxPool,
	})

	// Register the sync status provider with Polaris.
	p.RegisterSyncStatusProvider(comet.NewSyncProvider(clientCtx))

	// Start the services. TODO: move to place race condition is solved.
	return p.StartServices()
}

// RegisterLifecycles is a function that allows for the application to register lifecycles with
// the evm networking stack. It takes a client context and a slice of node.Lifecycle
// as arguments.
func (p *Polaris) RegisterLifecycles(lcs []node.Lifecycle) {
	// Register the services with polaris.
	for _, lc := range lcs {
		p.ExecutionLayer.RegisterLifecycle(lc)
	}
}

// StartServices starts the services of the Polaris struct.
func (p *Polaris) StartServices() error {
	go func() {
		// TODO: these values are sensitive due to a race condition in the json-rpc ports opening.
		// If the JSON-RPC opens before the first block is committed, hive tests will start failing.
		// This needs to be fixed before mainnet as its ghetto af. If the block time is too long
		// and this sleep is too short, it will cause hive tests to error out.
		time.Sleep(5 * time.Second) //nolint:gomnd // as explained above.
		if err := p.ExecutionLayer.Start(); err != nil {
			panic(err)
		}
	}()

	return nil
}

// LoadLastState is a function that loads the last state of the Polaris struct.
// It takes a CommitMultiStore and an appHeight as arguments.
// It returns an error if the loading fails.
// TODO: is incomplete in the blockchain object.
func (p *Polaris) LoadLastState(cms storetypes.CommitMultiStore, appHeight uint64) error {
	cmsCtx := sdk.Context{}.
		WithMultiStore(cms).
		WithGasMeter(storetypes.NewInfiniteGasMeter()).
		WithBlockGasMeter(storetypes.NewInfiniteGasMeter()).WithEventManager(sdk.NewEventManager())
	return p.Backend().Blockchain().LoadLastState(cmsCtx, appHeight)
}
