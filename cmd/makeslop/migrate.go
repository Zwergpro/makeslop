package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Zwergpro/makeslop/internal/config"
)

func newMigrateCmd(baseDir string) *cobra.Command {
	return &cobra.Command{
		Use:          "migrate",
		Short:        "Refresh ~/.makeslop with the latest embedded assets",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			applied, err := config.Migrate(baseDir)
			if err != nil {
				return err
			}
			if applied {
				fmt.Fprintln(cmd.OutOrStdout(), "makeslop: ~/.makeslop updated")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "makeslop: ~/.makeslop already up to date")
			}
			return nil
		},
	}
}
