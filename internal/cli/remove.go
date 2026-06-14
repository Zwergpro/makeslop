package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Zwergpro/makeslop/internal/workspace"
)

// newRemoveCmd does not take baseDir unlike other ws-accepting constructors because
// remove delegates all settings I/O to ws.Remove and needs no direct config access.
func newRemoveCmd(ws *workspace.Workspaces) *cobra.Command {
	return &cobra.Command{
		Use:          "remove <name>",
		Aliases:      []string{"rm"},
		Short:        "Unregister a workspace by name and delete its cache directory",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			quiet, _ := cmd.Flags().GetBool("quiet")
			name := args[0]

			cacheDir, err := ws.Remove(name)
			if err != nil {
				if errors.Is(err, workspace.ErrNotRegistered) {
					fmt.Fprintf(cmd.ErrOrStderr(), "no workspace named %q — run 'makeslop ls'\n", name)
					return errSilent
				}
				return err
			}

			if err := os.RemoveAll(cacheDir); err != nil {
				return fmt.Errorf("remove cache dir %s: %w", cacheDir, err)
			}

			chrome := &quietWriter{w: cmd.ErrOrStderr(), quiet: quiet}
			fmt.Fprintf(chrome, "removed %s\n", name)
			return nil
		},
	}
}
