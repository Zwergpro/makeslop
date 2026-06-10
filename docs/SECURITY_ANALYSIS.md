# makeslop — Security & Reliability Analysis

*Date: 2026-06-09. Scope: full codebase review — spec assembly, run/build paths, secret scan, project config, workspace registry, embedded Dockerfile.*

## What's already solid

The engineering foundation is strong, and several deliberate choices are exactly right:

- **Pure/impure split** with a drift-guard test (`internal/docker/spec.go` vs `run.go`/`build.go`).
- **Fail-loud secret scan** — walk errors abort the launch rather than silently skipping a directory that can't be proven secret-free.
- **Atomic settings writes** (temp file + rename) and file locking around load–mutate–save.
- **Preflight timeouts** so a black-hole `DOCKER_HOST` can't hang the CLI.
- **Staged Dockerfile build context** — the daemon never sees siblings of the Dockerfile (credentials, workspace cache).
- **Force-remove on cancellation** covering pre-start aborts and start failures.
- **Container hardening flags**: `--cap-drop ALL`, `no-new-privileges`, size-bounded `--tmpfs /tmp`.

The findings below are the gaps that remain.

---

## Security findings (highest impact first)

### 1. The agent can rewrite its own sandbox policy

`.makeslop.yaml` lives at the project root, which is bind-mounted read-write
(`internal/docker/spec.go:77`). An agent inside the container can edit it — empty
`exclude.scan.patterns`, clear `exclude.files` — and the **next** `makeslop run` launches with
secret masking silently disabled. The policy file is writable by the thing it constrains.

**Fix:** overlay `.makeslop.yaml` itself with a read-only bind (or `/dev/null`) inside the
container, the same way secrets are masked. Cheap, and it closes a real persistence vector.

### 2. The agent can plant payloads the host user will later execute

A prompt-injected agent can write to `.git/hooks/`, `Makefile`, `package.json` scripts, `.envrc` —
files executed on the host after the session. Inherent to read-write project mounts; can't be
fully fixed, but should be:

- (a) stated in `docs/security.md` as an explicit non-goal / residual risk, and
- (b) partially mitigated by masking `.git/hooks` by default.

### 3. `skip-dirs` creates masking blind spots — pruned from the *scan* but still *mounted*

`.git`, `node_modules`, `vendor`, `.venv` are skipped by `security.Scan`, yet their full contents
are visible in the container. `.git` is the worst case: `.git/config` can hold credential URLs,
and any secret ever committed is readable from object history even if deleted from the working
tree. With unrestricted network egress (a deliberate design choice), masking is the only line of
defense — and this hole is in it.

**Fix:** at minimum mask `.git/config` and `.git-credentials` inside skipped dirs, or scan
skip-dirs with patterns anyway. Skip-dirs as a *recursion* optimization shouldn't mean *exemption*.

### 4. The image has a sudo user with password `password`

`internal/assets/files/Dockerfile:36-39`. Under makeslop's flags this is neutralized —
`no-new-privileges` makes setuid sudo fail — but the image also exists for anyone who runs
`docker run claudebox` directly, where it's a trivial root escalation. The sudo group membership
and the known password serve no purpose at runtime.

**Fix:** remove both (defense in depth).

### 5. Global agent credentials are mounted read-write into every container

`~/.makeslop/.claude.json` and `.claude/`/`.codex/` (`spec.go:78-80`) are shared across all
workspaces. Two consequences:

- A prompt-injected agent in project A can exfiltrate Claude/Codex OAuth tokens over the open
  network.
- It can poison global agent state (memory, settings) that project B's sessions then trust
  (cross-workspace contamination).

The tokens are needed for the agent to function, so this is partly irreducible — but document it
as the trust boundary it is, and consider whether any of those mounts can be read-only.

### 6. Dockerfile supply chain: three `curl | bash` executions and a mutable base tag

`nodesource setup_lts.x`, `zsh-in-docker`, and `claude.ai/install.sh` all execute unpinned remote
scripts at build time; `debian:trixie-slim` is an unpinned tag. Anyone who compromises those
endpoints owns every rebuilt sandbox.

**Fix:** pin the base image by digest, pin the Claude installer version, prefer checksum-verified
downloads where upstream offers them.

### 7. Smaller masking gaps

- **Default patterns miss common shapes:** `*.p12`/`*.pfx`, `credentials` (AWS),
  `kubeconfig`/`*.kubeconfig`, `service-account*.json`, `.pypirc`, `.htpasswd`, `*.tfstate`,
  `.dockercfg` / `.docker/config.json`.
- **Symlinks are silently dropped** both by the scan and by `exclude.files`
  (`projectconfig.go` `statFilter`). A user who explicitly lists a symlinked secret gets no mask
  and no warning — contradicting the fail-loud philosophy. Warn (or error) instead.
- **TOCTOU:** a file created between the scan and `ContainerCreate` isn't masked. Probably
  acceptable; worth one sentence in the docs.

---

## Reliability findings

### 1. No resource limits at all

`HostConfig()` (`spec.go:229`) sets no `Memory`, `PidsLimit`, or CPU constraints. A runaway agent
(or fork bomb — fork needs no capabilities) can exhaust host PIDs and RAM. Worse, the
`MaskedDirs` tmpfs mounts are created with no size option — Docker's default tmpfs sizing lets
each grow to ~50% of host RAM. An agent that `npm install`s into a tmpfs-masked `node_modules` is
filling host memory.

**Fix:** add `PidsLimit` and a memory cap (config keys with sane defaults), and a size on mask
tmpfs mounts.

### 2. No `Init: true`

PID 1 in the container is zsh; an init process (`HostConfig.Init`) gives proper zombie reaping and
signal forwarding for long agent sessions. One-line change.

### 3. UID mapping friction on Linux

The image hardcodes UID/GID 1000. On a host where the user isn't 1000 (or with rootless/userns
Docker), the read-write workspace mount either fails to write or litters the project with
wrong-owner files. Detect and warn, support a build-arg UID, or document.

### 4. Concurrent `makeslop run` in the same workspace

Two sessions share the per-workspace `.claude/`/`.codex/` cache read-write with no coordination —
agents can clobber each other's state. A lock file (the `config.WithLock` machinery already
exists) or at least a warning would prevent confusing corruption.

### 5. Optional hardening to consider

- A `--read-only` rootfs mode (with tmpfs for `$HOME` scratch areas) for users who don't need
  `apt`/global installs mid-session.
- A docs note recommending rootless Docker or userns-remap, since anything with daemon-socket
  access is otherwise root-equivalent on the host.

---

## Where to start

Cheapest high-value changes:

1. Mask `.makeslop.yaml` and `.git/hooks` in `BuildSpec`.
2. Delete sudo membership + password from the Dockerfile.
3. Add `PidsLimit` / memory cap / tmpfs sizes to `HostConfig`; set `Init: true`.
4. Add an honest threat-model section to `docs/security.md`: *"the agent can write files you may
   later execute, and can read/exfiltrate the mounted agent credentials."* Costs nothing and is
   arguably the most important item — the current docs imply stronger isolation than the
   read-write mounts actually provide.
