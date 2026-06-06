# Fix silent `makeslop build` — BuildKit trace decode wire-format mismatch

## Overview
`makeslop build` runs a real BuildKit build (observed: ~15 seconds of actual work)
but prints **zero progress** — no `[+] Building` UI, no plain lines, nothing until
the prompt returns. The base image still builds; only the on-screen progress is
missing.

Root cause (empirically confirmed, see below): `decodeBuildKitAux` in
`internal/docker/build.go` parses the wrong BuildKit trace wire format, so every
trace frame is silently dropped before it reaches the `progressui` display. This
plan fixes the decoder and rewrites the test fixtures that baked in the wrong
format (which is why the bug shipped with green tests).

This is a pure decoder bug fix plus a test-correctness fix. No behavioural change
to the lazy-display streaming machinery, no new dependencies.

## Context (from discovery)
- **Files/components involved:**
  - `internal/docker/build.go` — `decodeBuildKitAux` (~line 274) and
    `renderBuildOutput` (~line 189). Only `decodeBuildKitAux` changes.
  - `internal/docker/build_test.go` — `makeAuxMessage` helper (line 319) and the
    trace/decode tests that depend on it.
- **The actual wire format** (ground truth):
  `jsonstream.Message` (`github.com/moby/moby/api@v1.54.2/types/jsonstream/message.go`)
  has both `ID string` and `Aux *json.RawMessage`. The daemon emits each BuildKit
  trace frame as a **top-level id-keyed** message:
  ```json
  { "id": "moby.buildkit.trace", "aux": "<base64-of-proto-StatusResponse>" }
  ```
  i.e. `msg.ID == "moby.buildkit.trace"` and `msg.Aux` is a JSON **string** whose
  base64 contents are the proto-marshalled `controlapi.StatusResponse`.
- **What the current code wrongly assumes:** `msg.Aux` is a JSON **object**
  `{"moby.buildkit.trace": "<base64>"}`, and it looks the key up *inside* Aux. So
  `json.Unmarshal(*msg.Aux, &map[string]json.RawMessage)` errors on the string
  value → returns `nil` → **every** frame dropped. BuildKit frames carry empty
  `Stream`/`Status`, so the plain-fallback branch prints nothing either, and the
  lazy display goroutine never starts (`statusCh` stays `nil`).
- **Empirical proof (this session):** a scratch in-package test built a real
  daemon-shaped frame (`proto.Marshal` a `StatusResponse` → `json.Marshal` the
  `[]byte` to get the base64 string → `jsonstream.Message{ID:"moby.buildkit.trace",
  Aux:&raw}`). Current `decodeBuildKitAux` returned `nil` (dropped it); the
  corrected decoder returned a `SolveStatus` with 1 vertex named `[1/2] FROM alpine`.
- **Why tests didn't catch it:** `makeAuxMessage` (build_test.go:319) builds the
  fake nested-map shape with **no `ID`** — the exact shape the buggy decoder
  expects. The tests validated the wrong format and shipped green.
- **Dependencies identified:** all imports already present in `build.go`
  (`controlapi`, `bkclient`, `jsonstream`, `proto`, `json`). No new deps.
- **Patterns/conventions:** pure/impure split (`decodeBuildKitAux` stays a pure
  helper); table/scratch-built fixtures; POSIX-only invariant.

## Development Approach
- **Testing approach:** Regular (fix the decoder, then correct/extend the tests in
  the same task). The fix itself is verified; the test rework is the larger part.
- Complete each task fully before the next.
- **Every task includes tests** — here the tests *are* the substance of the fix's
  durability (they must fail against the old decoder, pass against the new one).
- **All tests must pass before starting the next task.**
- Run `go test ./...` after each change.
- Maintain backward compatibility (plain-fallback, error, empty-body paths
  unchanged).

## Testing Strategy
- **Unit tests:** required every task.
  - `decodeBuildKitAux`: positive (real id-keyed frame → non-nil, expected vertex),
    negatives (nil Aux; wrong/absent ID; Aux present but malformed base64 / bad
    proto → nil, no panic).
  - `renderBuildOutput`: streaming trace, trace-then-error, trace+plain mixed,
    plain fallback, empty body, context-cancel — all using **real** frames where a
    trace frame is intended.
- **e2e tests:** project has no UI e2e suite. The real end-to-end confirmation is
  the gated live-daemon integration test:
  `MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/` — note in the
  verification task that it exercises the actual daemon stream (cannot run in CI /
  this sandbox without a daemon).
- **Sandbox note (not a code change):** test binaries need an exec-capable tmpdir;
  in this sandbox `/tmp` is noexec, so tests were run with
  `GOTMPDIR=$PWD/.tmpexec TMPDIR=$PWD/.tmpexec`. Whoever runs the suite on a normal
  machine can ignore this.

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document blockers with ⚠️ prefix
- keep this plan in sync with actual work

## Solution Overview
Rewrite `decodeBuildKitAux` to match docker/cli's canonical decoder: gate on
`msg.ID == "moby.buildkit.trace"`, unmarshal `*msg.Aux` into `[]byte` (base64 →
proto bytes), `proto.Unmarshal` into `controlapi.StatusResponse`, return
`bkclient.NewSolveStatus(&sr)`. Then correct the test fixtures so a real
daemon-shaped frame is what flows through `renderBuildOutput`. Everything else in
`renderBuildOutput` is already correct and stays untouched.

## Technical Details

**Corrected decoder** (`internal/docker/build.go`, replaces current
`decodeBuildKitAux` body):
```go
func decodeBuildKitAux(msg jsonstream.Message) *bkclient.SolveStatus {
	if msg.ID != "moby.buildkit.trace" || msg.Aux == nil {
		return nil
	}
	var dt []byte
	if err := json.Unmarshal(*msg.Aux, &dt); err != nil { // base64 string → proto bytes
		return nil
	}
	var sr controlapi.StatusResponse
	if err := proto.Unmarshal(dt, &sr); err != nil {
		return nil
	}
	return bkclient.NewSolveStatus(&sr)
}
```
Update the function's doc comment to describe the real format (id-keyed message,
Aux = base64 JSON string of proto `StatusResponse`).

**Corrected test helper** (`internal/docker/build_test.go`, replaces
`makeAuxMessage`; name it `realDaemonFrame` and update call sites):
```go
func realDaemonFrame(t *testing.T, sr *controlapi.StatusResponse) jsonstream.Message {
	t.Helper()
	dt, err := proto.Marshal(sr)
	if err != nil { t.Fatalf("proto.Marshal: %v", err) }
	auxBytes, err := json.Marshal(dt) // []byte → base64 JSON string
	if err != nil { t.Fatalf("json.Marshal: %v", err) }
	raw := json.RawMessage(auxBytes)
	return jsonstream.Message{ID: "moby.buildkit.trace", Aux: &raw}
}
```
(Provide a thin `realDaemonFrameEmpty(t)` or pass `&controlapi.StatusResponse{}`
for tests that only need a decodable frame.)

## What Goes Where
- **Implementation Steps** (`[ ]`): decoder fix, helper + fixtures rewrite,
  regression test, full-suite verification, doc updates.
- **Post-Completion** (no checkboxes): live-daemon integration run on a real
  machine; manual `makeslop build` smoke test to eyeball the `[+] Building` UI.

## Implementation Steps

### Task 1: Fix `decodeBuildKitAux` and add the regression test that catches this bug

**Files:**
- Modify: `internal/docker/build.go`
- Modify: `internal/docker/build_test.go`

- [ ] replace `decodeBuildKitAux` body with the id-keyed decoder (see Technical
      Details); update its doc comment to describe the real wire format
- [ ] confirm no import changes needed (`controlapi`, `bkclient`, `jsonstream`,
      `proto`, `json` already imported)
- [ ] add `realDaemonFrame(t, sr)` helper to `build_test.go`
- [ ] add `TestDecodeBuildKitAux_RealFrame`: build a frame with a known vertex
      (e.g. digest `sha256:abc`, name `[1/2] FROM alpine`) via `realDaemonFrame`,
      assert `decodeBuildKitAux` returns non-nil with that vertex — this is the
      test that would have caught the bug
- [ ] add `TestDecodeBuildKitAux_WrongID`: returns nil when `ID` is not
      `moby.buildkit.trace`; include an explicit **empty-`ID` + valid-Aux** sub-case
      (the daemon also emits non-trace aux frames like `moby.image.id` and bare-Aux
      frames — both must be ignored)
- [ ] run tests — must pass before Task 2
      (`GOTMPDIR=$PWD/.tmpexec TMPDIR=$PWD/.tmpexec go test ./internal/docker/`)

### Task 2: Migrate existing fixtures and trace tests to the real frame shape

**Files:**
- Modify: `internal/docker/build_test.go`

- [ ] remove `makeAuxMessage` (the fake nested-map helper) and repoint
      `TestRenderBuildOutput_StreamingTrace`, `TestRenderBuildOutput_TraceFollowedByError`,
      `TestRenderBuildOutput_TraceAndPlainMixed`, and `TestRenderBuildOutput_ContextCanceled`
      to `realDaemonFrame`
- [ ] rewrite `TestDecodeBuildKitAux_NoTraceKey` → `TestDecodeBuildKitAux_NoID`
      (frame with some other `ID` / no `ID`) so the negative test is meaningful
      under the new format
- [ ] rewrite `TestDecodeBuildKitAux_InvalidProto` to use the real shape: valid
      base64 string Aux + `ID:"moby.buildkit.trace"` but junk proto bytes → nil,
      no panic. **Pin a known-failing byte sequence** (e.g. the existing
      `"this is not a valid proto message"`, verified to fail, or `[]byte{0xff,0xff}`)
      with a comment — `proto.Unmarshal` is lenient and some arbitrary bytes decode
      to an empty valid message (non-nil), so the junk value must be one that
      reliably fails
- [ ] keep `TestDecodeBuildKitAux_NilAux` (still valid: nil Aux → nil)
- [ ] keep the plain-fallback / error / empty-body tests unchanged (they don't use
      trace frames)
- [ ] (optional) strengthen `TestRenderBuildOutput_StreamingTrace` to assert at
      least one frame reached the display path (e.g. non-empty stdout in PlainMode,
      or a vertex name appears) so a future format regression isn't a silent no-op
- [ ] run tests — must pass before Task 3

### Task 3: Verify acceptance criteria
- [ ] verify a real daemon-shaped trace frame is decoded (not dropped) — covered by
      `TestDecodeBuildKitAux_RealFrame`
- [ ] verify negative paths still degrade gracefully (nil Aux, wrong ID, bad proto)
- [ ] run full suite: `GOTMPDIR=$PWD/.tmpexec TMPDIR=$PWD/.tmpexec go test ./...`
- [ ] confirm no `MigrationVersion` / `CurrentVersion` bump (no Dockerfile/Settings
      change — per CLAUDE.md rules)
- [ ] note (do not run here — needs a daemon): the gated end-to-end check is
      `MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/`

### Task 4: [Final] Documentation and cleanup
- [ ] update the "renderBuildOutput lazy-display pattern" note in `CLAUDE.md`:
      `decodeBuildKitAux` decodes the **id-keyed** trace frame
      (`msg.ID == "moby.buildkit.trace"`, Aux = base64 JSON string of proto
      `StatusResponse`) — and that test fixtures must be built via `realDaemonFrame`,
      never the nested-map shape
- [ ] update `docs/` user-facing build docs only if they describe progress output
      (likely none needed)
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention or external systems — informational only*

**Manual verification:**
- On a machine with Docker: run `makeslop build --no-cache` and confirm the
  `[+] Building` UI streams step-by-step (the original symptom is gone).
- Run the gated integration test against a live daemon:
  `MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/`.
- Sanity-check the AutoMode→PlainMode fallback by piping:
  `makeslop build | cat` should still emit plain progress lines (non-TTY path).
