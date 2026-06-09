package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/docker"
	"github.com/Zwergpro/makeslop/internal/projectconfig"
	"github.com/Zwergpro/makeslop/internal/security"
	"github.com/Zwergpro/makeslop/internal/workspace"
)

// checkState represents the outcome of a single status check.
type checkState string

const (
	checkOK   checkState = "ok"
	checkFail checkState = "fail"
	checkWarn checkState = "warn"
	checkInfo checkState = "info"
)

// statusGlyphs maps each check state to its TTY glyph and plain-text fallback.
var statusGlyphs = map[checkState]struct{ tty, plain string }{
	checkOK:   {"✓", "[ok]"},
	checkFail: {"✗", "[fail]"},
	checkWarn: {"!", "[!]"},
	checkInfo: {"–", "[–]"},
}

// statusCheck is the result of one ordered status check.
type statusCheck struct {
	Name   string     `json:"name"`
	State  checkState `json:"state"`
	Detail string     `json:"detail"`
}

// statusResult is the full status output for --json mode.
type statusResult struct {
	Checks []statusCheck `json:"checks"`
	Ready  bool          `json:"ready"`
}

// isTTYFunc is the injectable predicate used by the status renderer to decide
// whether to emit ANSI color / glyphs. Tests inject a func returning false to
// get plain text output without needing a real PTY.
type isTTYFunc func(w io.Writer) bool

// defaultIsTTY returns true when w is os.Stderr and it is a TTY, and NO_COLOR
// is unset. It casts w to *os.File for the fd test; any non-file writer → false.
func defaultIsTTY(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	// fd 2 is stderr; we only color stderr output.
	return term.IsTerminal(int(f.Fd()))
}

// renderChecks writes the aligned check lines and the verdict line to w.
// isTTY controls glyph/color output; when false plain ASCII is used.
func renderChecks(w io.Writer, checks []statusCheck, ready bool, tty bool) {
	type row struct {
		glyph  string
		name   string
		detail string
	}

	rows := make([]row, len(checks))
	for i, c := range checks {
		g := "?"
		if gl, ok := statusGlyphs[c.State]; ok {
			if tty {
				g = gl.tty
			} else {
				g = gl.plain
			}
		}
		rows[i] = row{glyph: g, name: c.Name, detail: c.Detail}
	}

	// Compute column widths in rune count so that Unicode glyphs (3 bytes each)
	// and ASCII bracket glyphs (e.g. "[ok]") align correctly in the same output.
	maxGlyph := 0
	maxName := 0
	for _, r := range rows {
		if w := utf8.RuneCountInString(r.glyph); w > maxGlyph {
			maxGlyph = w
		}
		if w := utf8.RuneCountInString(r.name); w > maxName {
			maxName = w
		}
	}

	for _, r := range rows {
		if r.detail != "" {
			fmt.Fprintf(w, "  %-*s  %-*s  %s\n", maxGlyph, r.glyph, maxName, r.name, r.detail)
		} else {
			fmt.Fprintf(w, "  %-*s  %s\n", maxGlyph, r.glyph, r.name)
		}
	}

	fmt.Fprintln(w)
	if ready {
		fmt.Fprintln(w, "  ready")
	} else {
		// Emit the first failing check's remedy as the next action.
		for _, c := range checks {
			if c.State == checkFail {
				fmt.Fprintf(w, "  not ready — %s\n", c.Detail)
				return
			}
		}
		fmt.Fprintln(w, "  not ready")
	}
}

// runStatus implements the status command. It is a separate function so tests
// can inject the isTTY predicate for plain-output assertions.
func runStatus(cmd *cobra.Command, ws *workspace.Workspaces, baseDir string, jsonMode bool, ttyPred isTTYFunc) error {
	ctx := cmd.Context()
	stderr := cmd.ErrOrStderr()

	var checks []statusCheck
	ready := true // guilty when any blocking check fails

	// 1. Daemon check
	// Bound by preflightTimeout so a black-hole DOCKER_HOST does not hang forever.
	var daemonErr error
	{
		pfCtx, pfCancel := docker.WithPreflightTimeout(ctx)
		daemonErr = docker.CheckDaemon(pfCtx)
		pfCancel()
	}
	if daemonErr != nil {
		checks = append(checks, statusCheck{
			Name:   "daemon",
			State:  checkFail,
			Detail: "is docker running? — run 'docker info'",
		})
		ready = false
	} else {
		checks = append(checks, statusCheck{
			Name:  "daemon",
			State: checkOK,
		})
	}

	// 2. Base config check
	// loadedSettings is set when config loads successfully; shared with the
	// image check below to avoid a second Load call.
	var loadedSettings *config.Settings
	exists, err := config.BaseConfigExists(baseDir)
	if err != nil {
		checks = append(checks, statusCheck{
			Name:   "base config",
			State:  checkFail,
			Detail: fmt.Sprintf("cannot read settings: %v", err),
		})
		ready = false
	} else if !exists {
		checks = append(checks, statusCheck{
			Name:   "base config",
			State:  checkFail,
			Detail: "run 'makeslop init' to create ~/.makeslop",
		})
		ready = false
	} else {
		s, loadErr := config.Load(baseDir)
		if loadErr != nil {
			checks = append(checks, statusCheck{
				Name:   "base config",
				State:  checkFail,
				Detail: fmt.Sprintf("corrupt settings: %v", loadErr),
			})
			ready = false
		} else {
			loadedSettings = s
			current, latest, stale := config.MigrationStatus(s)
			if stale {
				checks = append(checks, statusCheck{
					Name:   "base config",
					State:  checkWarn,
					Detail: fmt.Sprintf("v%d (latest: v%d) — run 'makeslop migrate'", current, latest),
				})
				// Non-blocking: stale config does not prevent "ready"
			} else {
				checks = append(checks, statusCheck{
					Name:  "base config",
					State: checkOK,
				})
			}
		}
	}

	// 3. Image check
	// Bound by preflightTimeout.
	imageName := config.DefaultImage
	if loadedSettings != nil {
		imageName = loadedSettings.Image
	}
	var imageFound bool
	var imageErr error
	{
		pfCtx, pfCancel := docker.WithPreflightTimeout(ctx)
		imageFound, imageErr = docker.ImageExists(pfCtx, imageName)
		pfCancel()
	}
	if imageErr != nil {
		checks = append(checks, statusCheck{
			Name:   "image",
			State:  checkFail,
			Detail: fmt.Sprintf("error checking image %q: %v — is docker running?", imageName, imageErr),
		})
		ready = false
	} else if !imageFound {
		checks = append(checks, statusCheck{
			Name:   "image",
			State:  checkFail,
			Detail: fmt.Sprintf("image %q not built — run 'makeslop build'", imageName),
		})
		ready = false
	} else {
		checks = append(checks, statusCheck{
			Name:  "image",
			State: checkOK,
		})
	}

	// 4. Workspace check
	var pwd string
	var workspaceRoot string
	pwd, err = resolvePwd()
	if err != nil {
		checks = append(checks, statusCheck{
			Name:   "workspace",
			State:  checkFail,
			Detail: fmt.Sprintf("cannot resolve cwd: %v", err),
		})
		ready = false
	} else {
		workspaceRoot, _, err = ws.Lookup(pwd)
		if err != nil {
			if errors.Is(err, workspace.ErrNotRegistered) {
				checks = append(checks, statusCheck{
					Name:   "workspace",
					State:  checkFail,
					Detail: fmt.Sprintf("not registered — run 'makeslop init' in %s", pwd),
				})
			} else {
				checks = append(checks, statusCheck{
					Name:   "workspace",
					State:  checkFail,
					Detail: fmt.Sprintf("lookup error: %v", err),
				})
			}
			ready = false
		} else {
			checks = append(checks, statusCheck{
				Name:  "workspace",
				State: checkOK,
			})
		}
	}

	// 5. Secret scan summary (non-blocking)
	// Only run when workspace was successfully resolved.
	if workspaceRoot != "" {
		yamlExcludes, _, _, pcErr := projectconfig.Load(workspaceRoot)
		if pcErr != nil {
			checks = append(checks, statusCheck{
				Name:   "secret scan",
				State:  checkWarn,
				Detail: fmt.Sprintf("cannot read .makeslop.yaml: %v", pcErr),
			})
		} else {
			masked, scanErr := security.Scan(ctx, workspaceRoot, yamlExcludes.Patterns, yamlExcludes.SkipDirs)
			if scanErr != nil {
				checks = append(checks, statusCheck{
					Name:   "secret scan",
					State:  checkWarn,
					Detail: fmt.Sprintf("scan error: %v", scanErr),
				})
			} else if len(masked) > 0 {
				checks = append(checks, statusCheck{
					Name:   "secret scan",
					State:  checkOK,
					Detail: fmt.Sprintf("will mask %d file(s)", len(masked)),
				})
			} else {
				checks = append(checks, statusCheck{
					Name:  "secret scan",
					State: checkInfo,
				})
			}
		}
	} else {
		// Workspace not resolved: report scan as info only
		checks = append(checks, statusCheck{
			Name:  "secret scan",
			State: checkInfo,
		})
	}

	// Output
	if jsonMode {
		result := statusResult{Checks: checks, Ready: ready}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(result); encErr != nil {
			return fmt.Errorf("encode status JSON: %w", encErr)
		}
	} else {
		tty := ttyPred(stderr)
		renderChecks(stderr, checks, ready, tty)
	}

	if !ready {
		return errSilent
	}
	return nil
}

// newStatusCmd constructs and returns the `status` cobra command.
// ws is the workspace registry; baseDir is the makeslop home.
// ttyPred is the injectable TTY predicate (use defaultIsTTY for production).
func newStatusCmd(ws *workspace.Workspaces, baseDir string, ttyPred isTTYFunc) *cobra.Command {
	var jsonMode bool

	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Report readiness: daemon, image, workspace, scan",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd, ws, baseDir, jsonMode, ttyPred)
		},
	}
	cmd.Flags().BoolVar(&jsonMode, "json", false,
		"emit JSON instead of human-readable output")
	return cmd
}
