# Drop Image Building: Image From Settings or -i/--image

## Overview
- Narrow makeslop's responsibility: it no longer builds a Docker image and no longer embeds a Dockerfile.
- The image comes only from `settings.json` (`image` key) or a `-i/--image` override on `run` and `status`.
- `image` has no default. When it is unset, `run` fails, `status` reports ✗, and `init` warns. Each message tells the user to run `makeslop config set image <ref>`.
- The BuildKit/progressui stack, the embedded-asset package, and the whole migrate/version mechanism (which only existed to refresh the Dockerfile) all go away.
- **No backward compatibility**: `makeslop build`/`migrate` simply become unknown commands, and old `"version"` fields in settings.json are ignored.

## Context (from discovery)
- **Files/components involved:**
  - `internal/cli/{build,migrate,init,run,status,deps,root,config}.go`
  - `internal/docker/{build,client}.go`, `internal/docker/fakes_test.go`
  - `internal/config/{config,migrate,configkeys}.go`
  - `internal/assets/`
  - `docs/*.md`, `README.md`, `CLAUDE.md`
- **Patterns found:**
  - consumer-side interfaces in `deps.go`
  - `dockerNewErrStub` defers a `docker.New()` failure
  - the `chrome` quietWriter for suppressible stderr
  - `errSilent` for already-printed errors
  - the `checkList` ok/fail/warn/info API in `status.go`
  - flags registered only on the commands that use them (e.g. `--out-of-home`)
- **Dependencies to drop:** `github.com/moby/buildkit`, `github.com/tonistiigi/fsutil`, and anything else only `build.go` pulls in (via `go mod tidy`).

## Development Approach
- **testing approach**: Regular (code first, then tests)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run `go test -timeout=100s ./...` after each change
- backward compatibility is explicitly NOT required for this change

## Testing Strategy
- **unit tests**: required for every task. CLI tests use `newRootCmdWithDeps(baseDir, deps)` with `fakeDocker` (`internal/cli/main_test.go`).
- No e2e/UI tests in this project. The build integration test (`-tags integration`) is deleted along with build.

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- keep plan in sync with actual work done

## Solution Overview
- A single resolver in the cli package, `resolveImage(flagVal, settingsImage string) (string, error)`, picks the image in this order:
  1. the trimmed flag, if non-empty
  2. the trimmed settings image, if non-empty
  3. otherwise `errNoImage`

  It takes strings rather than `*config.Settings`, so callers never need a nil-settings workaround.
- `-i/--image` is a local flag on `run` and `status` only. `init`, `ls`, `remove` and `config` don't use an image, so they don't get the flag.
- `config.Load` stops defaulting `Image`. An empty string means unset.
- If the image is not present locally, makeslop fails with a `docker pull` hint and never pulls on its own. That keeps registry auth and pull-progress UI out of scope.
- The Dockerfile moves to `examples/claudebox/Dockerfile` as an unembedded starting point that users build themselves.
- `config.BaseConfigExists` **stays**: `status.go` uses it for the base-config check.

## Technical Details
- `internal/cli/image.go`:
  ```go
  var errNoImage = errors.New("no image configured — run 'makeslop config set image <ref>' or pass -i/--image")
  func resolveImage(flagVal, settingsImage string) (string, error)
  ```
- **`run` order**:
  1. load settings
  2. `resolveImage` (before `ws.Lookup`: config errors fail fast)
  3. `ws.Lookup`
  4. daemon preflight (skipped for `--dry-run`)
  5. project config, then secret scan
  6. `BuildSpec` with the resolved image
  7. dry-run print, or image-exists preflight followed by `Run`

  The resolve error is returned as-is: the root prints `makeslop: <err>` and exits 1. A missing local image prints `makeslop: image "X" not found locally — run 'docker pull X'` and returns `errSilent`.
- **`status` image check order**:
  1. `-i` given (non-empty) → skip the settings checks and go straight to the daemon/inspect steps, even if settings.json is corrupt or absent.
  2. settings corrupt → fail `cannot check — settings unreadable`.
  3. `resolveImage` returns `errNoImage` (settings absent or image unset) → fail `no image configured — run 'makeslop config set image <ref>'`. This check doesn't need the daemon.
  4. daemon down → fail `cannot check — daemon unreachable`.
  5. otherwise inspect the image → ok, or fail `image "X" not found locally — run 'docker pull X'`.

  Note: today the daemon-down message wins over corrupt settings (`status.go:181-185`). This reorders them, so the affected test (around `status_test.go:530`) needs updating.
- **`status` base config check**: ok if settings.json exists and parses, fail otherwise. The stale/warn branch goes away.
- **`init`**: after registering, if the loaded `s.Image == ""`, print `note: no image configured — run 'makeslop config set image <ref>'` to `chrome`. The final chrome line becomes `registered <name> — run 'makeslop run'`. Exit code 0.
- **`Settings`**: `{Image, Shell, TmpDirSize, Workspaces}`, with `Version` removed. `Load` still defaults `Shell` and `TmpDirSize`.
- **Test helper**: `initWithImage(t, baseDir)` in `internal/cli/main_test.go` runs `init` and then `config set image test-img`. It replaces the bare `runCmd(t, baseDir, "init")` seeding in the run/status tests that rely on an image being present.

## What Goes Where
- **Implementation Steps**: code changes, tests and doc updates in this repo.
- **Post-Completion**: manual smoke test against a real daemon, and release notes.

## Implementation Steps

### Task 1: Remove the build command and BuildKit plumbing

**Files:**
- Delete: `internal/cli/build.go`, `internal/cli/build_test.go`
- Delete: `internal/docker/build.go`, `internal/docker/build_test.go`, `internal/docker/build_integration_test.go`
- Modify: `internal/docker/client.go`, `internal/docker/docker.go`, `internal/docker/spec.go`, `internal/docker/preflight.go`
- Modify: `internal/docker/fakes_test.go`, `internal/docker/run_test.go`
- Modify: `internal/cli/deps.go`, `internal/cli/root.go`, `internal/cli/main_test.go`, `internal/cli/version_test.go`, `internal/cli/init_test.go`
- Modify: `go.mod`, `go.sum`, optionally `.golangci.yml`

- [ ] delete the build command files and remove `newBuildCmd` from `root.go`
- [ ] delete the docker build files
- [ ] `client.go`: remove `ImageBuild` and `DialHijack` from `apiClient`, and drop the now-unused `io`/`net` imports
- [ ] `spec.go`: delete `BuildOptions` (`spec.go:338-346`)
- [ ] fix the stale "Build" wording in the doc comments at `docker.go:11` and `preflight.go:13`
- [ ] `fakes_test.go`: delete the `fakeBuildClient` type (`:180-211`) and the `noopClient` `ImageBuild`/`DialHijack` methods (`:59-65`). Fix the `noopClient` doc comment and remove unused imports.
- [ ] `run_test.go:238-240`: delete `(*fakeClient).DialHijack`
- [ ] `deps.go`: remove `imageBuilder`, the `dockerDeps.builder` field and `dockerNewErrStub.Build`, and drop the `io` import. In `root.go:50,56`, remove the `builder:` wiring.
- [ ] `main_test.go`: remove the `fakeDocker.Build` method and the `LastBuildOpts` field (`:33`, `:54`). Drop `builder: f` from `depsFrom` (`:104-105`) and fix the nearby comment that mentions "four interfaces".
- [ ] `main_test.go:259`: delete `TestRoot_BareInvocation_ListsBuildCommand`
- [ ] remove the `{"build", …}` rows from the flag-rejection tables in `version_test.go:73-74` and `init_test.go:638-639`. Delete the build+init test in `init_test.go:492-523`, or strip its build part.
- [ ] run `go mod tidy` and confirm buildkit and fsutil are gone. Optionally drop the stale `(net.Conn).SetDeadline` / `internal/networks.Gateway` exclusions in `.golangci.yml`.
- [ ] run `go build ./... && go test -timeout=100s ./...`, which must pass before task 2

### Task 2: Remove the migrate/version mechanism and embedded assets

**Files:**
- Delete: `internal/config/migrate.go`, `internal/config/migrate_test.go`
- Delete: `internal/cli/migrate.go`, `internal/cli/migrate_test.go`
- Delete: `internal/assets/` (package and test)
- Create: `examples/claudebox/Dockerfile` (via `git mv` from `internal/assets/files/Dockerfile`)
- Modify: `internal/config/config.go`, `internal/config/config_test.go`, `internal/config/lock_test.go`, `internal/config/configkeys_test.go`
- Modify: `internal/workspace/workspace_test.go`
- Modify: `internal/cli/init.go`, `internal/cli/init_test.go`, `internal/cli/status.go`, `internal/cli/status_test.go`, `internal/cli/root.go`, `internal/cli/ls_test.go`, `internal/cli/main_test.go`, `internal/cli/version_test.go`

- [ ] `git mv internal/assets/files/Dockerfile examples/claudebox/Dockerfile` and delete the rest of `internal/assets/`
- [ ] delete the migrate files and remove `newMigrateCmd` from `root.go`
- [ ] `main_test.go:247`: delete `TestRoot_BareInvocation_ListsMigrateCommand`
- [ ] `config.go`: remove `ConfigVersion`, `DockerfileFile`, `Settings.Version`, the Dockerfile entry in `bootstrapFiles` and the `assets` import. **Keep `BaseConfigExists`.**
- [ ] `init.go`: remove the `BaseConfigExists`/`freshSeed` stamp-and-nudge logic
- [ ] `status.go`: the base-config check becomes ok/fail only, with no `MigrationStatus` and no warn
- [ ] `config_test.go`: drop the version assertions (25-26, 237-238, 391-475, and the `"version": 0` test at 432-450), the assets import (:15), and `TestBootstrap_CreatesDockerfile` / `…DoesNotOverwriteExistingDockerfile` (:593, :610). Also remove **every** remaining `Version:`/`ConfigVersion` in fixtures (e.g. :140, :187, :215, :273, :313, :412); `grep -n 'Version' internal/config/*_test.go` must come back clean.
- [ ] `lock_test.go:39-59`: **rewrite** the lost-update test, not delete it. Each goroutine adds `Workspaces[fmt.Sprint(i)]` inside `Update`, and the test asserts `len(Workspaces) == goroutines`.
- [ ] drop the `Version:`/`ConfigVersion` fields from test fixtures:
  - `configkeys_test.go:206-211`
  - `workspace_test.go:196,230,258,565,744`
  - `ls_test.go:60,134`
- [ ] `init_test.go`: delete the version=1 assertion (48-55), the fresh-seed stamping tests (385-424) and the stale-nudge tests (425-470)
- [ ] `main_test.go:451-486`: delete `TestQuiet_SuppressesInitNudge`. Task 6 adds the image-note replacement.
- [ ] remove the `{"migrate", …}` rows in `version_test.go` and `init_test.go`
- [ ] update the `status_test.go` stale-warn cases
- [ ] run `go test -timeout=100s ./...`, which must pass before task 3

### Task 3: Add the image resolver, drop the default image, add the test helper

**Files:**
- Create: `internal/cli/image.go`, `internal/cli/image_test.go`
- Modify: `internal/config/config.go`, `internal/config/config_test.go`, `internal/config/configkeys_test.go`, `internal/config/lock_test.go`
- Modify: `internal/cli/main_test.go`, `internal/cli/run_test.go`, `internal/cli/status_test.go`, `internal/cli/config_test.go`, `internal/cli/ls_test.go`, `internal/cli/status.go`

- [ ] add `errNoImage` and `resolveImage(flagVal, settingsImage string)` in `image.go`
- [ ] `config.go`: remove `DefaultImage` and the `Image` defaulting in `Load` (`:64-69`, `:80-82`). Update the "defaulting … backward compatibility" comments (`:26`, `:56-58`) so they only mention `Shell`/`TmpDirSize`.
- [ ] `status.go`: replace the `config.DefaultImage` fallback with a nil-guarded bridge: `imageName := ""; if loadedSettings != nil { imageName = loadedSettings.Image }`. `loadedSettings` is nil when settings.json is absent. Task 5 rewrites this.
- [ ] add `initWithImage(t, baseDir)` to `main_test.go`. Switch the run/status tests that need an image (about 40 in `run_test.go`, 15 in `status_test.go`, 4 in `main_test.go`) from bare `init` to this helper.
- [ ] replace the `DefaultImage` uses in tests: `configkeys_test.go:149,211`, `config_test.go`, `lock_test.go:16`, `cli/config_test.go:48`, `ls_test.go:61,135`
- [ ] write a `resolveImage` table test with these cases: flag wins over settings; flag used when settings are empty; settings used when there is no flag; both empty → `errNoImage`; whitespace-only flag falls back to settings; whitespace-only flag with empty settings → `errNoImage`
- [ ] write config tests: `Load` of a missing file → `Image == ""`; a file without `image` → `""`; `Shell`/`TmpDirSize` are still defaulted; `config list` prints `image = `
- [ ] run `go test -timeout=100s ./...`, which must pass before task 4

### Task 4: Wire -i/--image into run

**Files:**
- Modify: `internal/cli/run.go`, `internal/cli/run_test.go`, `internal/cli/main_test.go`

- [ ] register `-i, --image` on `run` and call `resolveImage` right after settings load, before `ws.Lookup`
- [ ] pass the resolved image into `docker.Options.Image` and the image-exists preflight
- [ ] change the missing-image message to `image "X" not found locally — run 'docker pull X'`
- [ ] write tests: `-i` overrides settings, lands in the spec, and shows up in the `--dry-run` output
- [ ] write tests for a registered workspace with no image and no flag: the error contains `config set image`; neither the daemon fake nor the runner fake is called; the same holds for `--dry-run`
- [ ] write a test for an unregistered workspace with no image: the image error wins, because resolve runs before Lookup
- [ ] update the existing missing-image test to expect the pull hint
- [ ] `main_test.go:664-688`: update `TestErrorVoice_ImageMissing_ContainsRemedy` to expect `docker pull` instead of `makeslop build`, seeding it with `initWithImage` so it reaches the image-exists preflight
- [ ] run `go test -timeout=100s ./...`, which must pass before task 5

### Task 5: Wire -i/--image into status

**Files:**
- Modify: `internal/cli/status.go`, `internal/cli/status_test.go`

- [ ] register `-i, --image` on `status` and thread it into `runStatus`
- [ ] rewrite the image check in the order given in Technical Details: flag, corrupt, unset, daemon down, inspect
- [ ] write tests:
  - unset image → ✗ with the config-set hint, including when the daemon is down
  - `-i` overrides settings
  - `-i` works when settings.json is absent
  - `-i` works when settings.json is corrupt
- [ ] write tests: the `--json` output reflects the unset-image failure and `ready: false`; a missing local image shows the pull hint
- [ ] update the existing corrupt-settings-with-daemon-down test (around `status_test.go:530`) for the new order
- [ ] run `go test -timeout=100s ./...`, which must pass before task 6

### Task 6: Add the init warning when no image is set

**Files:**
- Modify: `internal/cli/init.go`, `internal/cli/init_test.go`, `internal/cli/main_test.go`

- [ ] in `init`: print the `note: no image configured …` line to `chrome` when `Image == ""`, and change the final hint to `run 'makeslop run'`
- [ ] write tests:
  - no image → note printed, exit 0, workspace registered
  - image set → no note
- [ ] add `TestQuiet_SuppressesInitImageNote` in `main_test.go`, replacing the deleted nudge test
- [ ] run `go test -timeout=100s ./...`, which must pass before task 7

### Task 7: Verify acceptance criteria
- [ ] this search returns nothing:
  ```sh
  grep -rnE "DefaultImage|ConfigVersion|buildkit|progressui|internal/assets|BuildOptions|makeslop build|makeslop migrate" --include='*.go' .
  ```
  `claudebox` is left out on purpose: it is a valid test image name in spec/run tests.
- [ ] `go mod tidy` produces no diff
- [ ] `go build ./cmd/makeslop` succeeds and `./makeslop --help` lists neither build nor migrate
- [ ] run the full test suite: `go test -timeout=100s ./...`
- [ ] run `golangci-lint run`, which also catches leftover unused fakes and imports

### Task 8: [Final] Update documentation
- [ ] `README.md`:
  - rewrite the quickstart as: build or pull an image (e.g. `docker build -t claudebox examples/claudebox`), `makeslop config set image <ref>`, `makeslop init`, `makeslop run`
  - remove the build/migrate rows from the command table, the `--refresh` paragraph, and the Dockerfile mentions (around lines 15-16, 40-48, 115, 135-144)
  - document `-i/--image`
- [ ] `docs/reference.md`: same changes. Remove the `build`/`migrate` sections, the "not built — run 'makeslop build'" wording, and the migrate text in the `status` docs. Add `-i/--image` to `run` and `status`.
- [ ] `docs/architecture.md`: drop the BuildKit, embedded-asset and config-versioning sections, and describe image resolution
- [ ] `docs/security.md`: drop the build-context/sync note
- [ ] `CLAUDE.md`:
  - remove the Build section and the integration-test command
  - Layout: remove `build`/`migrate` from the command list, `build.go` from the `internal/docker` line, and the `internal/assets` entry
  - DI section: drop `imageBuilder`
  - timeouts: "Run/Build get no deadline" becomes "Run gets no deadline"
  - "Config and versioning" becomes "Settings", keeping only the locking notes. Delete the "Bump ConfigVersion" paragraph and the `init` stale-nudge bullet.
  - add: "`image` is never defaulted; commands resolve it via `resolveImage` (flag > settings > `errNoImage`)"
- [ ] `root.go:72`: reword the `--quiet` help text if it still mentions build progress
- [ ] leave the historical `docs/plans/` files untouched
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention or external systems. Informational only.*

**Manual verification:**
- against a real daemon:
  - `makeslop config set image claudebox` after building `examples/claudebox`, then `makeslop run` in an interactive terminal
  - `makeslop run -i some/unpulled:tag` prints the pull hint
  - `makeslop status` with no image shows ✗ and the hint
- a fresh `~/.makeslop` (move the old one aside): `init` prints the note and `run` fails with the config-set hint

**Release notes:**
- breaking change: the `build` and `migrate` commands are gone; `image` must now be set explicitly; `examples/claudebox/Dockerfile` is provided as a starting point
