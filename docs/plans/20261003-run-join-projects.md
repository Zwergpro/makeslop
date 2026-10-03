# Join projects in `run`

## Goal

`makeslop run -j <path>[:ro|:rw]` mounts another makeslop project at
`/workspace/<basename>`. The flag is repeatable; `rw` is the default. Without joins, mount
order and dry-run output stay unchanged.

## Design decisions

- `docker.Options.Projects[0]` is the current project. Each later entry has its own bind,
  policy mounts, and masks. Only the current project gets global and cache overlays.
- Each join uses its own `exclude:` rules. Its cache, environment, and network settings are
  ignored. A regular `.makeslop.yaml` is required both during path validation and loading, so
  deleting it between those steps cannot disable masking.
- Paths resolve against the physical current directory. Joins cannot overlap the current
  project, the makeslop data directory, or another join; mount names must be unique. Lexical
  and inode checks catch aliases of roots and ancestors, though a bind mount sourced from a
  subdirectory can evade them.
- Path validation precedes daemon preflight. Config parsing and scanning follow it, preserving
  the daemon-first error order. Dry-run skips daemon, image, and network preflight.
- `Spec.Sections` labels mount groups in `ShellCommand` only. Blank lines and `#` comments
  make joined dry-run output readable but not paste-ready. `Args()` and SDK projections use
  the same mounts without labels.

## Verification recorded during implementation

- Unit tests cover suffixes, path errors, overlap, name collisions, masking isolation, mount
  order, read-only joins, warning behavior, and daemon error precedence.
- The full Go suite and `go vet ./...` passed. Dry-run output without joins was compared with
  `main` on a project with scan and explicit masks.

## Manual check

Run against a real image to verify that joined files are visible, masks hide secrets,
read-only joins reject writes, and writable joins expose an empty `.git/hooks` directory.
