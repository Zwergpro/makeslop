# Cache Mount Modes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Accept optional read-only/read-write suffixes on cache entries, defaulting to read-write.

**Architecture:** Parse suffixes in projectconfig and preserve mode in typed declarations. CLI resolution copies the boolean into existing Docker mounts; existing rendering and SDK projection remain authoritative.

**Tech Stack:** Go, yaml.v3, Cobra, moby SDK; no new dependencies.

**Spec:** `docs/superpowers/specs/2026-11-07-cache-mount-modes-design.md`

## Global Constraints
- Default remains read-write; unknown modes and extra colon-separated fields fail.
- Dry runs create nothing; real runs prepare missing managed directories, including read-only sources.
- Strip mode before path validation and collision checks; preserve all existing filesystem safeguards.
- No new Docker calls or dependencies.

## Review Focus
- Same normalized managed name with different modes must still fail as a duplicate (Task 1).
- Empty paths, traversal with suffixes, repeated suffixes, uppercase or empty modes must fail (Task 1).
- Static files and directories must preserve mode through resolution (Task 2).
- Read-only mode must not bypass policy/exclude/destination collision checks (Task 2).
- Real runs must prepare read-only managed sources while dry runs remain side-effect-free (Task 2).

### Task 1: Parse modes into typed configuration

**Files:** `internal/projectconfig/cache.go`, `internal/projectconfig/cache_test.go`, `internal/projectconfig/projectconfig_test.go`; update managed declaration fixtures in `internal/cli/cache_test.go` as required for compilation.

**Interfaces:** Introduce `CacheDirectory struct { Name string; ReadOnly bool }`; change `CacheMounts.Shared` and `.Local` to `[]CacheDirectory`. Add `ReadOnly bool` to `CacheBind`. Keep `decodeCache(*yaml.Node) (CacheMounts, error)` unchanged.

- [ ] Add `TestDecodeCache_Modes`: each group accepts absent, `ro`, and `rw`; assert stripped names/paths and exact booleans. Add invalid cases for empty mode, unknown/uppercase mode, extra fields, empty path and traversal with mode. Assert `local: [docs, docs/:ro]` fails as a normalized duplicate.
- [ ] Run `go test -timeout=100s ./internal/projectconfig`; confirm new syntax tests fail under the old parser. Replace the obsolete invalid-static `x:/x:ro` fixture with an actually invalid mode.
- [ ] Implement typed declarations and suffix parsing before existing path checks. Distinguish static's required path separator from the optional mode separator. Preserve zero-value slices and strict YAML validation. Update decoder and CLI fixtures for the typed API, and CLI managed resolution to access `.Name` so all packages compile (mode propagation belongs to Task 2).
- [ ] Run `go test -timeout=100s ./internal/projectconfig ./internal/cli`; expect PASS for existing behavior and decoder mode tests.
- [ ] Commit the parser and fixture migration.

### Task 2: Propagate and verify mount modes end-to-end

**Files:** `internal/cli/cache.go`, `internal/cli/cache_test.go`, `internal/cli/run_cache_test.go`, `internal/docker/spec_test.go` and existing Docker drift tests where located; `docs/reference.md`, `docs/security.md`, `internal/projectconfig/projectconfig.go` where examples/comments need updating.

**Interfaces:** Consume Task 1's `CacheDirectory.ReadOnly` and `CacheBind.ReadOnly`; populate existing `docker.Mount.ReadOnly`. `resolveCache` and directory preparation APIs remain unchanged.

- [ ] Add `TestResolveCache_Modes`: typed shared/local declarations and static file/directory binds produce exact `ReadOnly` flags and suffix-free paths. Include read-only destination collision cases using existing policy/exclude fixtures.
- [ ] Add CLI integration tests loading mixed-mode YAML: real run receives correct flags and creates read-only managed sources; dry run emits `readonly` only for `ro` entries and creates no managed sources. Check both omitted and explicit `rw` entries.
- [ ] Run `go test -timeout=100s ./internal/cli -run 'Cache.*Mode|Cache.*ReadOnly'`; confirm mode assertions fail before propagation.
- [ ] Populate `docker.Mount.ReadOnly` for managed and static mounts. Leave directory preparation and validation ordering unchanged.
- [ ] Extend configured-cache Docker projection/drift fixtures to include both modes, checking SDK read-only flags and rendered parity. Update reference syntax and examples, remove claims that suffixes are disallowed/all mounts writable, and explain `rw` default and managed preparation for `ro`.
- [ ] Run `gofmt` on changed Go files, `go test -timeout=100s ./...`, and `go build ./cmd/makeslop`; expect PASS. Inspect diff for unrelated changes and confirm both mode paths are covered.
- [ ] Commit implementation, tests, and documentation.
