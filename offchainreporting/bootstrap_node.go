package offchainreporting

import (
	"context"

	"github.com/pkg/errors"
	"github.com/SeerLink/libocr/offchainreporting/internal/managed"
	"github.com/SeerLink/libocr/offchainreporting/types"
	"github.com/SeerLink/libocr/subprocesses"
	"golang.org/x/sync/semaphore"
)

type BootstrapNodeArgs struct {
	BootstrapperFactory   types.BootstrapperFactory
	Bootstrappers         []string
	ContractConfigTracker types.ContractConfigTracker
	Database              types.Database
	LocalConfig           types.LocalConfig
	Logger                types.Logger
	MonitoringEndpoint    types.MonitoringEndpoint
}

// BootstrapNode connects to a particular feed and listens for config changes,
// but does not participate in the protocol. It merely acts as a bootstrap node
// for the DHT
type BootstrapNode struct {
	bootstrapArgs BootstrapNodeArgs

	// Indicates whether the BootstrapNode has been started, in a thread-safe way
	started *semaphore.Weighted

	// subprocesses tracks completion of all go routines on BootstrapNode.Close()
	subprocesses subprocesses.Subprocesses

	// cancel sends a cancel message to all subprocesses, via a context.Context
	cancel context.CancelFunc
}

func NewBootstrapNode(args BootstrapNodeArgs) (*BootstrapNode, error) {
	if err := SanityCheckLocalConfig(args.LocalConfig); err != nil {
		return nil, errors.Wrapf(err,
			"bad local config while creating bootstrap node")
	}
	return &BootstrapNode{
		bootstrapArgs: args,
		started:       semaphore.NewWeighted(1),
	}, nil
}

// Start spins up a BootstrapNode. Panics if called more than once.
func (b *BootstrapNode) Start() error {
	b.failIfAlreadyStarted()

	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	b.subprocesses.Go(func() {
		defer cancel()
		managed.RunManagedBootstrapNode(
			ctx,

			b.bootstrapArgs.BootstrapperFactory,
			b.bootstrapArgs.Bootstrappers,
			b.bootstrapArgs.ContractConfigTracker,
			b.bootstrapArgs.Database,
			b.bootstrapArgs.LocalConfig,
			b.bootstrapArgs.Logger,
		)
	})
	return nil
}

// Close shuts down a BootstrapNode. Can safely be called multiple times.
func (b *BootstrapNode) Close() error {
	if b.cancel != nil {
		b.cancel()
	}
	// Wait for all subprocesses to shut down, before shutting down other resources.
	// (Wouldn't want anything to panic from attempting to use a closed resource.)
	b.subprocesses.Wait()
	return nil
}

func (b *BootstrapNode) failIfAlreadyStarted() {
	if !b.started.TryAcquire(1) {
		panic("can only start a BootstrapNode once")
	}
}
