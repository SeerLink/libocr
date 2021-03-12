package managed

import (
	"context"

	"github.com/SeerLink/libocr/offchainreporting/internal/config"
	"github.com/SeerLink/libocr/offchainreporting/types"
	"github.com/SeerLink/libocr/subprocesses"
)

// RunManagedBootstrapNode runs a "managed" bootstrap node. It handles
// configuration updates on the contract.
func RunManagedBootstrapNode(
	ctx context.Context,

	bootstrapperFactory types.BootstrapperFactory,
	bootstrappers []string,
	contractConfigTracker types.ContractConfigTracker,
	database types.Database,
	localConfig types.LocalConfig,
	logger types.Logger,
) {
	mb := managedBootstrapNodeState{
		ctx: ctx,

		bootstrapperFactory: bootstrapperFactory,
		bootstrappers:       bootstrappers,
		configTracker:       contractConfigTracker,
		database:            database,
		localConfig:         localConfig,
		logger:              logger,
	}
	mb.run()
}

type managedBootstrapNodeState struct {
	ctx context.Context

	bootstrappers       []string
	bootstrapperFactory types.BootstrapperFactory
	configTracker       types.ContractConfigTracker
	database            types.Database
	localConfig         types.LocalConfig
	logger              types.Logger

	bootstrapper types.Bootstrapper
	config       config.PublicConfig
}

func (mb *managedBootstrapNodeState) run() {
	var subprocesses subprocesses.Subprocesses

	// Restore config from database, so that we can run even if the ethereum node
	// isn't working.
	{
		var cc *types.ContractConfig
		ok := subprocesses.BlockForAtMost(
			mb.ctx,
			mb.localConfig.DatabaseTimeout,
			func(ctx context.Context) {
				cc = loadConfigFromDatabase(ctx, mb.database, mb.logger)
			},
		)
		if !ok {
			mb.logger.Error("ManagedBootstrapNode: database timed out while attempting to restore configuration", types.LogFields{
				"timeout": mb.localConfig.DatabaseTimeout,
			})
		} else if cc != nil {
			mb.configChanged(*cc)
		}
	}

	chNewConfig := make(chan types.ContractConfig, 5)
	subprocesses.Go(func() {
		TrackConfig(mb.ctx, mb.configTracker, mb.config.ConfigDigest, mb.localConfig, mb.logger, chNewConfig)
	})

	for {
		select {
		case cc := <-chNewConfig:
			mb.logger.Info("ManagedBootstrapNode: Switching between configs", types.LogFields{
				"oldConfigDigest": mb.config.ConfigDigest.Hex(),
				"newConfigDigest": cc.ConfigDigest.Hex(),
			})
			mb.configChanged(cc)
		case <-mb.ctx.Done():
			mb.logger.Debug("ManagedBootstrapNode: winding down ", nil)
			mb.closeBootstrapper()
			subprocesses.Wait()
			mb.logger.Debug("ManagedBootstrapNode: exiting", nil)
			return
		}
	}
}

func (mb *managedBootstrapNodeState) closeBootstrapper() {
	if mb.bootstrapper != nil {
		err := mb.bootstrapper.Close()
		// there's not much we can do apart from logging the error and praying
		if err != nil {
			mb.logger.Error("ManagedBootstrapNode: error while closing bootstrapper", types.LogFields{
				"error": err,
			})
		}
		mb.bootstrapper = nil
	}
}

func (mb *managedBootstrapNodeState) configChanged(cc types.ContractConfig) {
	// Cease any operation from earlier configs
	mb.closeBootstrapper()

	var err error
	mb.config, err = config.PublicConfigFromContractConfig(cc)
	if err != nil {
		mb.logger.Error("ManagedBootstrapNode: error while decoding ContractConfig", types.LogFields{
			"error": err,
		})
		return
	}

	peerIDs := []string{}
	for _, pcKey := range mb.config.OracleIdentities {
		peerIDs = append(peerIDs, pcKey.PeerID)
	}

	bootstrapper, err := mb.bootstrapperFactory.MakeBootstrapper(mb.config.ConfigDigest, peerIDs, mb.bootstrappers, mb.config.F)
	if err != nil {
		mb.logger.Error("ManagedBootstrapNode: error during MakeBootstrapper", types.LogFields{
			"error":         err,
			"configDigest":  mb.config.ConfigDigest,
			"peerIDs":       peerIDs,
			"bootstrappers": mb.bootstrappers,
		})
		return
	}
	err = bootstrapper.Start()
	if err != nil {
		mb.logger.Error("ManagedBootstrapNode: error during bootstrapper.Start()", types.LogFields{
			"error":        err,
			"configDigest": mb.config.ConfigDigest,
		})
		return
	}

	mb.bootstrapper = bootstrapper

	childCtx, childCancel := context.WithTimeout(mb.ctx, mb.localConfig.DatabaseTimeout)
	defer childCancel()
	if err := mb.database.WriteConfig(childCtx, cc); err != nil {
		mb.logger.Error("ManagedBootstrapNode: error writing new config to database", types.LogFields{
			"config": cc,
			"error":  err,
		})
		// We can keep running even without storing the config in the database
	}
}
