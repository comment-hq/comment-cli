package commentbus

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

const busConfigVersion = 2

type BusConfig struct {
	Version       int    `json:"version"`
	BotletsHome string `json:"botlets_home,omitempty"`
}

func ReadBusConfig(paths Paths) (BusConfig, bool, error) {
	data, err := os.ReadFile(busConfigPath(paths))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return BusConfig{}, false, nil
		}
		return BusConfig{}, false, err
	}
	var config BusConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return BusConfig{}, false, err
	}
	if config.Version != busConfigVersion {
		return BusConfig{}, false, errors.New("unsupported bus config version")
	}
	if config.BotletsHome != "" {
		resolved, err := ResolveBotletsHome(config.BotletsHome)
		if err != nil {
			return BusConfig{}, false, err
		}
		config.BotletsHome = resolved
	}
	return config, true, nil
}

func WriteBusConfig(paths Paths, config BusConfig) error {
	if config.Version == 0 {
		config.Version = busConfigVersion
	}
	if config.BotletsHome != "" {
		resolved, err := ResolveBotletsHome(config.BotletsHome)
		if err != nil {
			return err
		}
		config.BotletsHome = resolved
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return WritePrivateFileAtomic(busConfigPath(paths), append(data, '\n'), 0o600)
}

func busConfigPath(paths Paths) string {
	return filepath.Join(paths.Bus, "config.json")
}
