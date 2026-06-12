# makeslop — Security

This document covers makeslop's security-relevant behaviors: secret masking, network egress
control, and the home-directory guard. For in-container hardening flags (`--cap-drop ALL`,
`no-new-privileges`, `--tmpfs`, bind-mount rationale), see
[reference.md — In-container security flags](reference.md#in-container-security-flags).

## Table of Contents

- [Secret masking](#secret-masking)
- [Project-local exclusions](#project-local-exclusions)
  - [Breaking change: path-style patterns rejected](#breaking-change-path-style-patterns-rejected)
  - [Breaking change: symlinked `.makeslop.yaml` rejected](#breaking-change-symlinked-makeslopya-ml-rejected)
- [Sandbox-policy protection](#sandbox-policy-protection)
- [Network egress](#network-egress)
- [Home-directory guard](#home-directory-guard)

---

## Secret masking

Before launching the container, `makeslop` scans for secret files under the project root using a
native Go `filepath.WalkDir` walk driven entirely by the project's `.makeslop.yaml`. Each matched
file is overlaid with `/dev/null` inside the container — the agent sees a zero-byte file at that
path instead of the real credential.

Secret masking is **opt-in and config-driven**: if `exclude.scan` is absent (or `patterns` is
empty) in `.makeslop.yaml`, no scan is performed and no pattern-matched files are masked (explicit
`exclude.files` entries are still overlaid). `makeslop init` seeds the default patterns and
skip-dirs as active values in the generated `.makeslop.yaml`, so new projects are safe by default.

The default `exclude.scan.patterns` cover the common secret-file shapes:

```yaml
patterns:
  - "*.env"
  - ".env.*"
  - "*.pem"
  - "*.key"
  - "*.p12"
  - "*.pfx"
  - "*.tfstate"
  - "id_rsa*"
  - "id_ed25519*"
  - ".npmrc"
  - ".netrc"
  - ".git-credentials"
  - ".pypirc"
  - ".htpasswd"
  - "service-account*.json"
  - "kubeconfig"
  - "*.kubeconfig"
```

The default `skip-dirs` are `.git`, `node_modules`, `vendor`, and `.venv`. See
[Project-local exclusions](#project-local-exclusions) for the full generated `.makeslop.yaml`.

Patterns are basename globs (`filepath.Match`). Regular files matching a pattern are masked.
Symlinks matching a pattern are **not masked** (WalkDir does not follow symlinks), but
`makeslop run` prints a warning to stderr for each such symlink so the gap is visible — this warning
is **not suppressed by `--quiet`** (degraded protection is not silent chrome):

```
makeslop: warning: symlink <rel-path> matches a secret pattern but is NOT masked
```

Directories named in `skip-dirs` are pruned entirely during the walk.

Walk errors (e.g. unreadable subdirectories) are propagated immediately and abort the launch. This
matches the no-secret-leak invariant: if a directory cannot be read, we cannot prove it is
secret-free.

### Trust assumptions

`skip-dirs` directories are **bind-mounted into the container unscanned**. The scan guarantee
("no secret-pattern file will be visible to the agent") applies only to the paths that are actually
walked. Secrets inside skipped directories — for example, credentials embedded in
`.git/config` (e.g. HTTPS URLs with tokens), OAuth tokens cached by package managers under
`node_modules/`, or private keys accidentally committed and reachable via `vendor/` — are the
user's responsibility.

To widen the scan guarantee, remove entries from `exclude.scan.skip-dirs` in `.makeslop.yaml`. The
trade-off is a longer pre-launch walk on large trees. The default skip list (`.git`, `node_modules`,
`vendor`, `.venv`) is chosen to balance performance against the most common secret locations; `.git`
in particular is skipped because it is almost always benign and scanning it would be very slow on
repos with long histories.

`.gitignore` is intentionally ignored because most `.env` files are gitignored — that is precisely
why the scan is necessary.

When at least one file is masked, `makeslop` prints `makeslop: masked N secret file(s)` to stderr.
Zero hits are silent.

**Pre-existing projects:** if your `.makeslop.yaml` predates the secret-masking feature, it will
not contain an `exclude.scan` block — secret masking will not run. Copy the complete
`exclude.scan` block (with both `patterns` and `skip-dirs`) from the generated template in
[Project-local exclusions](#project-local-exclusions) below into your existing `.makeslop.yaml`
to restore masking.

**Updating scan patterns:** if your `.makeslop.yaml` has an `exclude.scan` block but was generated
before the hardening pass (2026-06-10), it may be missing the 8 newer patterns (`*.p12`, `*.pfx`,
`*.tfstate`, `.pypirc`, `.htpasswd`, `service-account*.json`, `kubeconfig`, `*.kubeconfig`). These
are not added automatically (makeslop never rewrites a project-local config). Copy the missing
entries from the template below into your `exclude.scan.patterns` list.

---

## Project-local exclusions

`makeslop init` creates a `.makeslop.yaml` file at the project root. The generated file includes
the default `exclude.scan` block (patterns + skip-dirs for the secret scan) and empty `files`/`dirs`
lists:

```yaml
exclude:
  scan:
    patterns:
      - "*.env"
      - ".env.*"
      - "*.pem"
      - "*.key"
      - "*.p12"
      - "*.pfx"
      - "*.tfstate"
      - "id_rsa*"
      - "id_ed25519*"
      - ".npmrc"
      - ".netrc"
      - ".git-credentials"
      - ".pypirc"
      - ".htpasswd"
      - "service-account*.json"
      - "kubeconfig"
      - "*.kubeconfig"
    skip-dirs:
      - .git
      - node_modules
      - vendor
      - .venv
  files: []
  dirs: []
cache:
  content: true
  agent: true
```

Edit this file to control scanning and hide additional directories and files from the container on
every `makeslop run` invocation:

- Entries under `exclude.scan.patterns` are **basename globs only** — patterns must not contain a
  `/` path separator. `makeslop` matches each pattern against the file's *name* (e.g. `secret.pem`),
  not its full path (e.g. `secrets/secret.pem`). Path-style patterns such as `secrets/*.pem` or
  `**/*.env` are now rejected with a hard error at startup (see
  [Breaking change: path-style patterns rejected](#breaking-change-path-style-patterns-rejected)
  below). Use basename forms: `*.pem`, `*.env`.
  Remove all patterns to disable secret masking entirely.
- Entries under `exclude.scan.skip-dirs` are bare directory names pruned during the walk.
- Entries under `exclude.dirs` are mounted as an empty in-memory tmpfs, so the container sees an
  empty directory at that path instead of the real contents.
- Entries under `exclude.files` are overlaid with `/dev/null`, so the container sees a zero-byte
  file at that path.

All paths under `exclude.dirs` and `exclude.files` must be relative to the project root. Example
(showing only the additions; the `exclude.scan` block stays as generated by `init`):

```yaml
exclude:
  scan:
    # ... patterns and skip-dirs unchanged from the generated template ...
  dirs:
    - node_modules        # large build artifact — skip it entirely
    - secrets             # local secrets directory
  files:
    - secrets/local.env   # specific file overlay
```

The scan results and the `exclude.files` entries are merged; if the same path is found by the scan
and listed in `exclude.files`, only one overlay mount is emitted. A YAML parse error aborts the
launch before docker is invoked.

### Breaking change: path-style patterns rejected

Starting with this release, `exclude.scan.patterns` entries that contain a `/` are **rejected with a
hard error** at startup (`makeslop run`, `makeslop status`):

```
projectconfig: scan pattern "secrets/*.pem" contains a path separator — patterns match basenames only
```

**Why:** `Scan` matches patterns against the file's *basename* using `filepath.Match`. A pattern
like `secrets/*.pem` could never match because the basename `secret.pem` does not contain a slash.
Previously such patterns were silently accepted and silently dropped — masking appeared configured
but nothing was masked.

**Migration:** change any path-style pattern to its basename equivalent:

```yaml
# Before (silently broken — never matched anything):
exclude:
  scan:
    patterns:
      - "secrets/*.pem"
      - "**/*.env"
      - "config/credentials.json"

# After (correct basename globs):
exclude:
  scan:
    patterns:
      - "*.pem"
      - "*.env"
      - "credentials.json"
```

If you need to mask a *specific file* at a specific path (not a pattern), add it to
`exclude.files` instead:

```yaml
exclude:
  files:
    - secrets/prod.pem
    - config/credentials.json
```

### Breaking change: symlinked `.makeslop.yaml` rejected

`makeslop run`, `makeslop init`, and `makeslop status` now reject a `.makeslop.yaml` that is a
symlink (dangling or live) with a hard error:

```
projectconfig: .makeslop.yaml is a symlink — the project config must be a regular file
```

**Why:** a dangling symlink was previously treated as "no config present" (the follow of a broken
link returned `ENOENT`), which silently dropped all scan patterns. Even a live symlink to a valid
file is rejected because `ProtectProjectConfig` already refuses to create the read-only bind mount
for a symlinked config (a symlink bind-mount does not protect the file contents). Consistent
rejection at load time prevents a split-brain state where the file is loaded but not protected.

**Migration:** replace the symlink with a regular file:

```sh
cp --remove-destination "$(readlink .makeslop.yaml)" .makeslop.yaml
```

Or, on macOS (no `--remove-destination`):

```sh
cp "$(readlink .makeslop.yaml)" .makeslop.yaml.tmp && mv .makeslop.yaml.tmp .makeslop.yaml
```

---

**YAML parse errors are hard failures.** Any unknown field in `.makeslop.yaml` — including the now-removed
`network:` block from earlier makeslop versions — causes a strict-decode error that aborts `makeslop run`
before Docker is contacted. If you upgrade from a version that had proxy support and your `.makeslop.yaml`
contains a `network:` block, remove it:

```yaml
# Remove this block entirely if present:
# network:
#   proxy:
#     address: 10.0.0.5:3128
```

**Reserved paths.** The paths `.claude`, `.codex`, `docs`, `CLAUDE.md`, and `.makeslop.yaml` are
already mounted by `makeslop run` (agent state or sandbox-policy mounts). Listing them in
`.makeslop.yaml` is rejected with an error
(`projectconfig: path %q collides with a reserved agent path`).

**Symlink warnings.** If an entry in `exclude.files` or `exclude.dirs` is a symlink on the host,
it is dropped from masking and a warning is printed to stderr:

```
makeslop: warning: path "<rel>" is a symlink and is NOT masked
```

This warning bypasses `--quiet` — degraded protection is never silent.

---

## Sandbox-policy protection

`makeslop run` applies two additional mount-level protections to prevent an agent running inside the
container from escaping its sandbox:

### Config file read-only bind

When `.makeslop.yaml` exists at the project root, `makeslop run` re-mounts it **read-only** over
itself inside the container (a bind mount layered on top of the read-write project bind). This
prevents the agent from modifying the file that controls scan patterns, reserved paths, and secret
masking — it cannot relax its own sandbox policy.

When `.makeslop.yaml` is absent, the read-only bind is skipped (a missing bind source would fail
container create, and there is nothing to protect).

### Git hooks tmpfs mask

When the project root contains a `.git` directory (not a gitfile), `makeslop run` overlays
`.git/hooks` inside the container with an empty tmpfs. This prevents the agent from planting git
hooks (e.g. `post-commit`, `pre-push`) that would execute on the host when the user runs git
operations after the session.

**Worktrees and submodules (residual risk).** In git worktrees and submodules, `.git` is a regular
*file* (a gitfile pointing at the real gitdir elsewhere). The directory gate correctly leaves the
hooks tmpfs off in that case — the daemon would otherwise create an empty `.git/hooks/` directory
in the project root. However, the real hooks directory lives outside the workspace and is **not
masked**. Makeslop does not chase the gitfile target (that would require parsing `.git` file contents
and resolving `GIT_DIR`). If you use worktrees or submodules, be aware that the agent can write to
the real hooks directory.

Both protections are reflected in `--dry-run` output.

---

## Embedded image hardening

The container image built by `makeslop build` is derived from a pinned Debian base (`debian:trixie-slim`
referenced by digest) and installs infrastructure tools (Go, Node.js) from official distribution
tarballs with per-architecture sha256 checksum verification.

**"Pin infra, float agents" policy:** infrastructure layers whose sha256 is verified at build time
(base image digest, Go tarball, Node tarball, zsh-in-docker script) are pinned to exact versions
with hardcoded checksums in the Dockerfile. Agent installers (`claude.ai/install.sh`,
`@openai/codex`) are intentionally left floating — these are under active development and users
benefit from receiving the latest agent version on each build. Pinning agent versions is an
accepted residual risk (documented maintainer decision).

**Maintaining pins:** when `GO_VERSION` or `NODE_VERSION` is bumped, the corresponding
per-architecture sha256 values in the `RUN` commands must be updated to match the new release, and
`ConfigVersion` must be incremented so existing installs pick up the refreshed Dockerfile via
`makeslop migrate` + `makeslop build`.

---

## Network egress

The app container uses standard Docker bridge networking with full internet access. There is no
built-in egress proxy, no `--network none` isolation, and no socat sidecar.

Use `--dry-run` to preview the resulting container launch command (printed as an equivalent
`docker run` invocation), including all exclusion mounts, before launching:

```
makeslop run --dry-run
```

---

## Home-directory guard

By default, `makeslop run` and `makeslop init` refuse to run from any directory outside the user's
home directory. This prevents accidentally registering sensitive system paths (e.g. `/`, `/etc`)
as workspaces and mounting them into a container. On violation the tool prints:

```
makeslop: refusing to run from <pwd> (outside <home>) — pass --out-of-home to override
```

Pass `--out-of-home` to bypass this check. The flag is scoped to `init` and `run` only:

```
makeslop init --out-of-home
makeslop run --out-of-home
```

`makeslop build`, `makeslop migrate`, `makeslop config`, `makeslop version`, and `makeslop status`
are **exempt** from the home-directory guard — they operate on `~/.makeslop/` directly and do not
consult the current working directory. `--out-of-home` is not a valid flag on these commands.
