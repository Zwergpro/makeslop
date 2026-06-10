package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/docker"
)

func newBuildCmd(baseDir string, deps dockerDeps) *cobra.Command {
	var noCache bool
	var buildArgs []string
	var refresh bool

	cmd := &cobra.Command{
		Use:          "build",
		Short:        "Build (or rebuild) the base docker image from ~/.makeslop/Dockerfile",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			quiet, _ := cmd.Flags().GetBool("quiet")
			if err := config.Bootstrap(baseDir); err != nil {
				return err
			}
			if refresh {
				if err := config.WriteDockerfile(baseDir); err != nil {
					return err
				}
				if !quiet {
					fmt.Fprintln(cmd.ErrOrStderr(),
						"makeslop: refreshed ~/.makeslop/Dockerfile from embedded assets")
				}
			}
			s, err := config.Load(baseDir)
			if err != nil {
				return err
			}
			o := docker.BuildOptions{
				Image:          s.Image,
				DockerfilePath: filepath.Join(baseDir, config.DockerfileFile),
				NoCache:        noCache,
				BuildArgs:      buildArgs,
				Quiet:          quiet,
			}
			return deps.builder.Build(cmd.Context(), o, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().BoolVar(&noCache, "no-cache", false,
		"do not use cache when building the image")
	cmd.Flags().StringArrayVar(&buildArgs, "build-arg", nil,
		"set build-time variables (repeatable)")
	cmd.Flags().BoolVar(&refresh, "refresh", false,
		"overwrite ~/.makeslop/Dockerfile from embedded assets before building")
	return cmd
}
