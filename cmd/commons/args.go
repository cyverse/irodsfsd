package commons

import (
	"fmt"

	"github.com/cyverse/irodsfsd/commons"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func SetCommonFlags(command *cobra.Command) {
	command.PersistentFlags().BoolP("debug", "d", false, "Enable debug mode")
	command.PersistentFlags().StringP("config", "c", "", "Set config file (yaml)")
}

func ProcessCommonFlags(command *cobra.Command) (*commons.Config, error) {
	logger := log.WithFields(log.Fields{})

	debug, err := command.Root().PersistentFlags().GetBool("debug")
	if err != nil {
		return nil, err
	}

	if debug {
		log.SetLevel(log.DebugLevel)
	}

	readConfig := false
	var config *commons.Config

	configPath, err := command.Root().PersistentFlags().GetString("config")
	if err != nil {
		return nil, err
	}
	if len(configPath) > 0 {
		serverConfig, err := commons.NewConfigFromFile(commons.NewDefaultConfig(), configPath)
		if err != nil {
			logger.Error(err)
			return nil, err
		}

		config = serverConfig
		readConfig = true
	}

	// default config
	if !readConfig {
		config = commons.NewDefaultConfig()
	}

	// prioritize command-line flag over config files
	if debug {
		log.SetLevel(log.DebugLevel)
		config.Debug = true
	}

	if config.Debug {
		log.SetLevel(log.DebugLevel)
	}

	err = config.Validate()
	if err != nil {
		logger.Error(err)
		return nil, err
	}

	return config, nil
}

func PrintVersion(command *cobra.Command) error {
	info, err := commons.GetVersionJSON()
	if err != nil {
		return err
	}

	fmt.Fprintln(command.OutOrStdout(), info)
	return nil
}
