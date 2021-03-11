package managed

import (
	"context"

	"github.com/SeerLink/libocr/offchainreporting/loghelper"
	"github.com/SeerLink/libocr/offchainreporting/types"
)

func loadConfigFromDatabase(ctx context.Context, database types.Database, logger loghelper.LoggerWithContext) *types.ContractConfig {
	cc, err := database.ReadConfig(ctx)
	if err != nil {
		logger.Error("loadConfigFromDatabase: Error during Database.ReadConfig", types.LogFields{
			"error": err,
		})
		return nil
	}

	if cc == nil {
		logger.Info("loadConfigFromDatabase: Database.ReadConfig returned nil, no configuration to restore", nil)
		return nil
	}

	return cc
}
