package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/workspace"
)

const lsTimeLayout = "2006-01-02 15:04 UTC"

func newLsCmd(ws *workspace.Workspaces, baseDir string) *cobra.Command {
	return &cobra.Command{
		Use:          "ls",
		Short:        "List registered workspaces",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			quiet, _ := cmd.Flags().GetBool("quiet")

			s, err := config.Load(baseDir)
			if err != nil {
				return err
			}

			workspaces := ws.List(s)
			if len(workspaces) == 0 {
				chrome := &quietWriter{w: cmd.ErrOrStderr(), quiet: quiet}
				fmt.Fprintln(chrome, "no workspaces registered — run 'makeslop init'")
				return nil
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tPATH\tCREATED")
			for _, w := range workspaces {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", w.Name, w.Path, w.CreatedAt.UTC().Format(lsTimeLayout))
			}
			return tw.Flush()
		},
	}
}
