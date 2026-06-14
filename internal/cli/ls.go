package cli

import (
	"fmt"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Zwergpro/makeslop/internal/config"
)

func newLsCmd(baseDir string) *cobra.Command {
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

			if len(s.Workspaces) == 0 {
				chrome := &quietWriter{w: cmd.ErrOrStderr(), quiet: quiet}
				fmt.Fprintln(chrome, "no workspaces registered — run 'makeslop init'")
				return nil
			}

			type row struct {
				Name    string
				Path    string
				Created string
			}
			rows := make([]row, 0, len(s.Workspaces))
			for path, ws := range s.Workspaces {
				rows = append(rows, row{
					Name:    ws.Name,
					Path:    path,
					Created: ws.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"),
				})
			}
			sort.Slice(rows, func(i, j int) bool {
				return rows[i].Name < rows[j].Name
			})

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tPATH\tCREATED")
			for _, r := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Name, r.Path, r.Created)
			}
			return tw.Flush()
		},
	}
}
