package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Zwergpro/makeslop/internal/config"
)

func newConfigCmd(baseDir string) *cobra.Command {
	// Shared by bare `config` and `config list`.
	runConfigList := func(cmd *cobra.Command, _ []string) error {
		s, err := config.Load(baseDir)
		if err != nil {
			return err
		}
		for _, e := range config.ConfigList(s) {
			fmt.Fprintf(cmd.OutOrStdout(), "%s = %s\n", e.Name, e.Value)
		}
		return nil
	}

	configCmd := &cobra.Command{
		Use:          "config",
		Short:        "View or change makeslop settings",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE:         runConfigList,
	}

	configListCmd := &cobra.Command{
		Use:          "list",
		Short:        "Print current effective settings as key = value lines",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE:         runConfigList,
	}

	configSetCmd := &cobra.Command{
		Use:          "set <key> <value>",
		Short:        "Validate, set, and persist a configuration key",
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Pre-validate before locking so a bad input doesn't create the lock
			// file as a side effect. On pre-load failure (corrupt JSON), skip and
			// let the locked path surface the error.
			if scratch, err := config.Load(baseDir); err == nil {
				if err := config.ConfigSet(scratch, args[0], args[1]); err != nil {
					return err
				}
			}

			var storedVal string
			if err := config.WithLock(baseDir, func() error {
				s, err := config.Load(baseDir)
				if err != nil {
					return err
				}
				if err := config.ConfigSet(s, args[0], args[1]); err != nil {
					return err
				}
				if err := config.Save(baseDir, s); err != nil {
					return err
				}
				// Echo the stored (normalized) value, not the raw argument.
				storedVal, _ = config.ConfigGet(s, args[0])
				return nil
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s = %s\n", args[0], storedVal)
			return nil
		},
	}

	configCmd.AddCommand(configListCmd, configSetCmd)
	return configCmd
}
