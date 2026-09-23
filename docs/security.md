# makeslop — Security

This document covers makeslop's security-relevant behaviors: secret masking, sandbox-policy
protection, network egress, and the home-directory guard. For in-container hardening flags
(`--cap-drop ALL`, `no-new-privileges`, `--tmpfs`, bind-mount rationale), see
[reference.md — In-container security flags](reference.md#in-container-security-flags).

## Table of Contents

- [Secret masking](#secret-masking)
  - [Trust assumptions](#trust-assumptions)
- [Project-local exclusions](#project-local-exclusions)
  - [Validation rules](#validation-rules)
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

`.gitignore` is intentionally ignored because most `.env` files are gitignored — that is precisely
why the scan is necessary.

When at least one file is masked, `makeslop` prints `makeslop: masked N secret file(s)` to stderr.
Zero hits are silent.

**Pre-existing projects:** makeslop never rewrites an existing `.makeslop.yaml`. If yours predates
the secret-masking feature, it has no `exclude.scan` block and masking will not run; if it predates
the current default list, it may be missing some of the patterns above. Copy the complete
`exclude.scan` block (with both `patterns` and `skip-dirs`) from the generated template in
[Project-local exclusions](#project-local-exclusions) below into your existing `.makeslop.yaml`.

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

---

## Project-local exclusions

`makeslop init` creates a `.makeslop.yaml` file at the project root. The generated file includes
the default `exclude.scan` block (patterns + skip-dirs for the secret scan), empty `files`/`dirs`
lists, and the `cache:` block:

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

(`init --global-only` writes `false` for both `cache:` keys.)

Edit this file to control scanning and hide additional directories and files from the container on
every `makeslop run` invocation:

- Entries under `exclude.scan.patterns` are **basename globs only** — `makeslop` matches each
  pattern against the file's *name* (e.g. `secret.pem`), not its full path. Remove all patterns to
  disable secret masking entirely.
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
and listed in `exclude.files`, only one overlay mount is emitted. Entries that do not exist on the
host, or have the wrong type (a directory under `files`, a file under `dirs`), are silently skipped.

**Symlink warnings.** If an entry in `exclude.files` or `exclude.dirs` is a symlink on the host,
it is dropped from masking and a warning is printed to stderr:

```
makeslop: warning: path "<rel>" is a symlink and is NOT masked
```

This warning bypasses `--quiet` — degraded protection is never silent.

### Validation rules

`.makeslop.yaml` is parsed before docker is invoked, and every error below aborts `makeslop run`
(`makeslop status` reports it as a non-blocking `!` on the secret-scan check):

- **Unknown keys** — the file is decoded in strict mode, so a typo or a stale block is a hard
  error. This includes the `network:` block (`network.proxy.address`) from earlier makeslop
  versions; the egress proxy it configured no longer exists (see [Network egress](#network-egress)).
  Remove the block if present.
- **Path-style scan patterns** — a pattern containing `/` could never match a basename, so it is
  rejected rather than silently masking nothing:
  ```
  projectconfig: scan pattern "secrets/*.pem" contains a path separator — patterns match basenames only
  ```
  Rewrite it as a basename glob (`*.pem`), or list the specific path under `exclude.files`
  (`secrets/prod.pem`). Empty patterns and invalid glob syntax are also rejected.
- **Invalid skip-dirs** — entries must be bare directory names (no `/`, not `.` or `..`, not empty).
- **Invalid paths** in `exclude.files`/`exclude.dirs` — empty, absolute, escaping the project
  root, referring to the root itself, or listed in both lists.
- **Reserved paths** — `.claude`, `.codex`, `docs`, `CLAUDE.md`, and `.makeslop.yaml` are already
  mounted by `makeslop run` (agent-state or sandbox-policy mounts). Listing them is rejected
  (`projectconfig: path "<path>" collides with a reserved agent path`).
- **Symlinked `.makeslop.yaml`** — a symlink (dangling or live) is rejected by `run` and `init`:
  ```
  projectconfig: .makeslop.yaml is a symlink — the project config must be a regular file
  ```
  A dangling link would otherwise read as "no config" and silently drop all scan patterns, and a
  live link could not be protected by the read-only bind (see
  [Sandbox-policy protection](#sandbox-policy-protection)). Replace the symlink with a regular file:
  ```sh
  cp --remove-destination "$(readlink .makeslop.yaml)" .makeslop.yaml
  ```
  On macOS (no `--remove-destination`):
  ```sh
  cp "$(readlink .makeslop.yaml)" .makeslop.yaml.tmp && mv .makeslop.yaml.tmp .makeslop.yaml
  ```

Invalid `environments:` entries are also hard errors; see
[reference.md — Environment variables](reference.md#environment-variables-environments-block-in-makeslopyaml).

---

## Sandbox-policy protection

`makeslop run` applies two additional mount-level protections to prevent an agent running inside the
container from escaping its sandbox:

### Config file read-only bind

When `.makeslop.yaml` is a regular file at the project root, `makeslop run` re-mounts it
**read-only** over itself inside the container (a bind mount layered on top of the read-write
project bind). This prevents the agent from modifying the file that controls scan patterns,
reserved paths, and secret masking — it cannot relax its own sandbox policy. If a scan pattern
(e.g. a broad `*.yaml`) would also mask the config file, that `/dev/null` overlay is dropped so it
does not replace the read-only bind.

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
masked**. makeslop does not chase the gitfile target. If you use worktrees or submodules, be aware
that the agent can write to the real hooks directory if it is reachable from the container.

Both protections are reflected in `--dry-run` output.

---

## Network egress

The container uses Docker's default bridge networking and has full internet access. There is no
built-in egress proxy, no `--network none` isolation, and no sidecar container. If you need to
route traffic through a proxy, set the usual variables yourself via the `environments:` block
(e.g. `HTTP_PROXY`, `HTTPS_PROXY`); makeslop does not enforce them.

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

`makeslop config`, `makeslop version`, `makeslop status`, `makeslop ls`, and `makeslop remove`
are **exempt** from the home-directory guard. `--out-of-home` is not a valid flag on these commands.
