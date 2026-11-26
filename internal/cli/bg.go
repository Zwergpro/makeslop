package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/Zwergpro/makeslop/internal/docker"
	"github.com/Zwergpro/makeslop/internal/workspace"
)

const dryRunBGUUID = "00000000-0000-4000-8000-000000000000"

func newBGCmd(ws *workspace.Workspaces, baseDir string, deps dockerDeps) *cobra.Command {
	cmd := &cobra.Command{Use: "bg", Short: "Run and manage persistent background terminals", Args: cobra.NoArgs}
	cmd.AddCommand(
		newBGRunCmd(ws, baseDir, deps),
		&cobra.Command{Use: "ls", Short: "List running and stopped background containers", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error { return runBGList(cmd, deps) }},
		bgTargetCmd("attach", "Reconnect to a background terminal", deps.api.AttachBG),
		newBGExecCmd(deps),
		bgTargetCmd("stop", "Stop a background container without removing it", deps.api.StopBG),
		bgTargetCmd("start", "Start a stopped background container", deps.api.StartBG),
		bgTargetCmd("restart", "Replace a background terminal with a fresh session", deps.api.RestartBG),
		bgTargetCmd("remove", "Remove a stopped background container", deps.api.RemoveBG),
	)
	return cmd
}

func newBGRunCmd(ws *workspace.Workspaces, baseDir string, deps dockerDeps) *cobra.Command {
	var opts launchOptions
	var attach bool
	cmd := &cobra.Command{Use: "run", Short: "Start a persistent background terminal for this workspace", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.Quiet, _ = cmd.Flags().GetBool("quiet")
			return runBG(cmd, ws, baseDir, opts, attach, deps)
		},
	}
	bindLaunchFlags(cmd, &opts)
	cmd.Flags().BoolVar(&attach, "attach", false, "attach to the terminal after starting it")
	return cmd
}

func runBG(cmd *cobra.Command, ws *workspace.Workspaces, baseDir string, opts launchOptions, attach bool, deps dockerDeps) error {
	launch, err := prepareLaunch(cmd, ws, baseDir, opts, deps)
	if err != nil {
		return err
	}
	if opts.DryRun {
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "# Example UUID; no container was created.\n"+launch.Spec.ForBackground(dryRunBGUUID, launch.Project).ShellCommand())
		return err
	}
	if attach {
		if err := deps.api.CheckTTY(); err != nil {
			return bgTTYError(err)
		}
	}
	if err := prepareLaunchExecution(cmd, launch, deps); err != nil {
		return err
	}
	uuid, err := docker.NewBGUUID()
	if err != nil {
		return err
	}
	if err := deps.api.RunBG(cmd.Context(), launch.Spec.ForBackground(uuid, launch.Project)); err != nil {
		return err
	}
	if !attach {
		_, err = fmt.Fprintln(cmd.OutOrStdout(), uuid)
		return err
	}
	if _, err := fmt.Fprintln(cmd.ErrOrStderr(), uuid); err != nil {
		return err
	}
	return bgTTYError(deps.api.AttachBG(cmd.Context(), uuid))
}

func runBGList(cmd *cobra.Command, deps dockerDeps) error {
	ctx, cancel := docker.WithPreflightTimeout(cmd.Context())
	defer cancel()
	rows, err := deps.api.ListBG(ctx)
	if err != nil {
		return err
	}
	out := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(out, "UUID\tPROJECT\tSTATE\tIMAGE")
	for _, row := range rows {
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", row.UUID, safeBGCell(row.Project), safeBGCell(row.State), safeBGCell(row.Image))
	}
	return out.Flush()
}

func safeBGCell(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}

func bgTargetCmd(verb, help string, action func(context.Context, string) error) *cobra.Command {
	return &cobra.Command{Use: verb + " <uuid>", Short: help, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return bgTTYError(action(cmd.Context(), args[0])) },
	}
}

func newBGExecCmd(deps dockerDeps) *cobra.Command {
	cmd := &cobra.Command{Use: "exec <uuid> [--] [command [args...]]", Short: "Run a separate interactive process in a background container", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() == 0 {
				return fmt.Errorf("background UUID must precede '--'")
			}
			argv := args[1:]
			if len(argv) > 0 && argv[0] == "--" {
				argv = argv[1:]
			}
			return bgTTYError(deps.api.ExecBG(cmd.Context(), args[0], argv))
		},
	}
	// After the UUID, flags and any additional -- belong to the child process.
	cmd.Flags().SetInterspersed(false)
	return cmd
}

func bgTTYError(err error) error {
	if errors.Is(err, docker.ErrNoTTY) {
		return fmt.Errorf("%w — run in an interactive terminal", err)
	}
	return err
}
