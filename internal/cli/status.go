package cli

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

// checkList accumulates status checks; ready is cleared by the first fail().
type checkList struct {
	checks []statusCheck
	ready  bool // starts true; fail() clears it
}

func newCheckList() *checkList { return &checkList{ready: true} }

func (c *checkList) ok(name, detail string) {
	c.checks = append(c.checks, statusCheck{Name: name, State: checkOK, Detail: detail})
}

func (c *checkList) fail(name, detail string) {
	c.checks = append(c.checks, statusCheck{Name: name, State: checkFail, Detail: detail})
	c.ready = false
}

func (c *checkList) warn(name, detail string) {
	c.checks = append(c.checks, statusCheck{Name: name, State: checkWarn, Detail: detail})
}

func (c *checkList) info(name string) {
	c.checks = append(c.checks, statusCheck{Name: name, State: checkInfo})
}

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

// isTTYFunc gates color/glyph output; tests inject a stub returning false.
type isTTYFunc func(w io.Writer) bool

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
		if gw := utf8.RuneCountInString(r.glyph); gw > maxGlyph {
			maxGlyph = gw
		}
		if nw := utf8.RuneCountInString(r.name); nw > maxName {
			maxName = nw
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

	cl := newCheckList()

	// 1. Daemon.
	if daemonErr := deps.checkDaemonPreflight(ctx); daemonErr != nil {
		cl.fail("daemon", "is docker running? — run 'docker info'")
	} else {
		cl.ok("daemon", "")
	}

	// 2. Base config. loadedSettings is reused by checks 3 and 4.
	var loadedSettings *config.Settings
	exists, err := config.BaseConfigExists(baseDir)
	if err != nil {
		cl.fail("base config", fmt.Sprintf("cannot read settings: %v", err))
	} else if !exists {
		cl.fail("base config", "run 'makeslop init' to create ~/.makeslop")
	} else {
		s, loadErr := config.Load(baseDir)
		if loadErr != nil {
			cl.fail("base config", fmt.Sprintf("corrupt settings: %v", loadErr))
		} else {
			loadedSettings = s
			current, latest, stale := config.MigrationStatus(s)
			if stale {
				// stale is non-blocking
				cl.warn("base config", fmt.Sprintf("v%d (latest: v%d) — run 'makeslop migrate'", current, latest))
			} else {
				cl.ok("base config", "")
			}
		}
	}

	// 3. Image.
	imageName := config.DefaultImage
	if loadedSettings != nil {
		imageName = loadedSettings.Image
	}
	imageFound, imageErr := deps.imageExistsPreflight(ctx, imageName)
	if imageErr != nil {
		cl.fail("image", fmt.Sprintf("error checking image %q: %v — is docker running?", imageName, imageErr))
	} else if !imageFound {
		cl.fail("image", fmt.Sprintf("image %q not built — run 'makeslop build'", imageName))
	} else {
		cl.ok("image", "")
	}

	// 4. Workspace — nil loadedSettings when settings unreadable; Lookup treats nil
	// as ErrNotRegistered, avoiding a duplicate parse-error detail here.
	var workspaceRoot string
	pwd, pwdErr := resolvePwd()
	if pwdErr != nil {
		cl.fail("workspace", fmt.Sprintf("cannot resolve cwd: %v", pwdErr))
	} else {
		var lookupErr error
		workspaceRoot, _, lookupErr = ws.Lookup(loadedSettings, pwd)
		if lookupErr != nil {
			if errors.Is(lookupErr, workspace.ErrNotRegistered) {
				detail := fmt.Sprintf("not registered — run 'makeslop init' in %s", pwd)
				if loadedSettings == nil {
					detail = "cannot check — settings unreadable"
				}
				cl.fail("workspace", detail)
			} else {
				cl.fail("workspace", fmt.Sprintf("lookup error: %v", lookupErr))
			}
		} else {
			cl.ok("workspace", "")
		}
	}

	// 5. Secret scan summary (non-blocking), only when workspace resolved.
	if workspaceRoot != "" {
		yamlExcludes, _, _, pcErr := projectconfig.Load(workspaceRoot)
		if pcErr != nil {
			cl.warn("secret scan", fmt.Sprintf("cannot read .makeslop.yaml: %v", pcErr))
		} else {
			masked, _, scanErr := security.Scan(ctx, workspaceRoot, yamlExcludes.Patterns, yamlExcludes.SkipDirs)
			if scanErr != nil {
				cl.warn("secret scan", fmt.Sprintf("scan error: %v", scanErr))
			} else if len(masked) > 0 {
				cl.ok("secret scan", fmt.Sprintf("will mask %d file(s)", len(masked)))
			} else {
				cl.info("secret scan")
			}
		}
	} else {
		cl.info("secret scan")
	}

	if jsonMode {
		result := statusResult{Checks: cl.checks, Ready: cl.ready}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(result); encErr != nil {
			return fmt.Errorf("encode status JSON: %w", encErr)
		}
	} else {
		tty := ttyPred(stderr)
		renderChecks(stderr, cl.checks, cl.ready, tty)
	}

	if !cl.ready {
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
