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

type checkState string

const (
	checkOK   checkState = "ok"
	checkFail checkState = "fail"
	checkWarn checkState = "warn"
	checkInfo checkState = "info"
)

// statusGlyphs maps each state to its TTY glyph and plain-text fallback.
var statusGlyphs = map[checkState]struct{ tty, plain string }{
	checkOK:   {"✓", "[ok]"},
	checkFail: {"✗", "[fail]"},
	checkWarn: {"!", "[!]"},
	checkInfo: {"–", "[–]"},
}

type statusCheck struct {
	Name   string     `json:"name"`
	State  checkState `json:"state"`
	Detail string     `json:"detail"`
}

type statusResult struct {
	Checks []statusCheck `json:"checks"`
	Ready  bool          `json:"ready"`
}

// isTTYFunc decides whether to emit color/glyphs; tests inject one returning
// false to get plain output without a real PTY.
type isTTYFunc func(w io.Writer) bool

// defaultIsTTY reports whether w is a TTY *os.File and NO_COLOR is unset.
func defaultIsTTY(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// renderChecks writes the aligned check lines and verdict to w; tty selects
// Unicode glyphs over plain ASCII.
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

	// Rune-count widths so multi-byte Unicode glyphs align with ASCII bracket glyphs.
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
		// Next action is the first failing check's remedy.
		for _, c := range checks {
			if c.State == checkFail {
				fmt.Fprintf(w, "  not ready — %s\n", c.Detail)
				return
			}
		}
		fmt.Fprintln(w, "  not ready")
	}
}

func runStatus(cmd *cobra.Command, ws *workspace.Workspaces, baseDir string, jsonMode bool, ttyPred isTTYFunc, deps dockerDeps) error {
	ctx := cmd.Context()
	stderr := cmd.ErrOrStderr()

	var checks []statusCheck
	ready := true // cleared when any blocking check fails

	// 1. Daemon — bounded by preflightTimeout so a black-hole DOCKER_HOST cannot hang.
	var daemonErr error
	{
		pfCtx, pfCancel := docker.WithPreflightTimeout(ctx)
		daemonErr = deps.daemon.CheckDaemon(pfCtx)
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

	// 2. Base config. loadedSettings is reused by the image check to avoid a second Load.
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
				// stale is non-blocking
			} else {
				checks = append(checks, statusCheck{
					Name:  "base config",
					State: checkOK,
				})
			}
		}
	}

	// 3. Image — bounded by preflightTimeout.
	imageName := config.DefaultImage
	if loadedSettings != nil {
		imageName = loadedSettings.Image
	}
	var imageFound bool
	var imageErr error
	{
		pfCtx, pfCancel := docker.WithPreflightTimeout(ctx)
		imageFound, imageErr = deps.image.ImageExists(pfCtx, imageName)
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

	// 4. Workspace — reuse loadedSettings from check 2; if settings are absent
	// or corrupt, pass a nil settings so Lookup returns ErrNotRegistered rather
	// than a redundant parse-error detail.
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
		// When settings were unreadable (loadedSettings == nil) we pass nil;
		// Lookup treats nil as "no workspaces registered" → ErrNotRegistered.
		// This avoids a duplicate / misleading parse-error detail in the workspace
		// check when the real cause was already surfaced by the base-config check.
		var lookupErr error
		workspaceRoot, _, lookupErr = ws.Lookup(loadedSettings, pwd)
		if lookupErr != nil {
			if errors.Is(lookupErr, workspace.ErrNotRegistered) {
				detail := fmt.Sprintf("not registered — run 'makeslop init' in %s", pwd)
				if loadedSettings == nil {
					// Settings were unreadable; registering a workspace is not the
					// real remedy — surface the constraint more precisely.
					detail = "cannot check — settings unreadable"
				}
				checks = append(checks, statusCheck{
					Name:   "workspace",
					State:  checkFail,
					Detail: detail,
				})
			} else {
				checks = append(checks, statusCheck{
					Name:   "workspace",
					State:  checkFail,
					Detail: fmt.Sprintf("lookup error: %v", lookupErr),
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

	// 5. Secret scan summary (non-blocking), only when workspace resolved.
	if workspaceRoot != "" {
		yamlExcludes, _, _, pcErr := projectconfig.Load(workspaceRoot)
		if pcErr != nil {
			checks = append(checks, statusCheck{
				Name:   "secret scan",
				State:  checkWarn,
				Detail: fmt.Sprintf("cannot read .makeslop.yaml: %v", pcErr),
			})
		} else {
			masked, _, scanErr := security.Scan(ctx, workspaceRoot, yamlExcludes.Patterns, yamlExcludes.SkipDirs)
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
		checks = append(checks, statusCheck{
			Name:  "secret scan",
			State: checkInfo,
		})
	}

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

func newStatusCmd(ws *workspace.Workspaces, baseDir string, ttyPred isTTYFunc, deps dockerDeps) *cobra.Command {
	var jsonMode bool

	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Report readiness: daemon, image, workspace, scan",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd, ws, baseDir, jsonMode, ttyPred, deps)
		},
	}
	cmd.Flags().BoolVar(&jsonMode, "json", false,
		"emit JSON instead of human-readable output")
	return cmd
}
