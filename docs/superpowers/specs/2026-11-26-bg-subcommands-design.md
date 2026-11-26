# Background containers: `makeslop bg`

**Status:** Draft for review. The `bg` command group, `ls` spelling, UUID-only user interface, Docker name prefix, and zmx backend were selected in discussion. The lifecycle and error behavior below are proposed v1 defaults.

## User experience

Leave a project terminal running, disconnect, and come back to the same shell or agent later. One background container owns one persistent zmx terminal. An attachment is a new client of that terminal, not a second shell or agent.

```sh
id=$(makeslop bg run)       # from a registered project; returns when the terminal is ready
makeslop bg attach "$id"    # start Claude, Codex, or another program in the shell
# Press Ctrl-\ to detach; the program keeps running.
makeslop bg ls             # from anywhere on the same Docker daemon
makeslop bg attach "$id"    # return to the live program
makeslop bg stop "$id"      # end the session, keep the container
makeslop bg restart "$id"   # same container and UUID, fresh shell
makeslop bg stop "$id"
makeslop bg remove "$id"    # delete the stopped container, not the project
```

### Command contract

| Command | Behavior |
|---|---|
| `bg run [run flags]` | Always create a new container and start the configured shell in a zmx PTY in the main project's workdir. Do not require a host TTY, attach the caller, or wait for the shell to exit. Print **only the full UUID and a newline** to stdout on success. Multiple runs from one project get distinct UUIDs. |
| `bg ls` | List **all** managed containers on the selected daemon, including stopped ones, newest first. Show `UUID`, `PROJECT`, `STATE`, and `IMAGE`; use the full copyable UUID. An empty list is a success with a header and no rows. `makeslop ls` still lists registered workspaces. |
| `bg attach <uuid>` | Require interactive stdin and stdout. Connect to the existing terminal and propagate terminal resizes. With default zmx bindings, `Ctrl-\` disconnects this client; closing the host terminal also leaves the session running. Two attachments share input and output; neither gets a private shell. A normal detach succeeds and does not stop the container. |
| `bg stop <uuid>` | Stop the container, ending the shell, any running agent, and attached clients. Keep it listed and keep its writable layer. Already stopped is a successful no-op. |
| `bg restart <uuid>` | Start a stopped container, or stop and start a running one. In both cases start **one fresh session** using the original launch configuration. Keep the UUID and writable layer. Return success only when the fresh terminal is ready. |
| `bg remove <uuid>` | Remove a stopped container and its writable layer. Refuse a running container with a hint to use `bg stop`. Never unregister the workspace or delete host project files or cache directories. |

Only `attach` requires a host TTY. Successful `stop`, `restart`, and `remove` need no stdout. Errors go to stderr and return nonzero; `--quiet` does not hide errors or required masking warnings. Commands taking a UUID require exactly one target; never infer it from the current directory or pick the most recent container. For v1 accept full UUIDs, not raw Docker names, Docker IDs, or abbreviated UUIDs.

### What ends the session?

Detaching a client changes nothing about the shell. Quitting an agent launched from the shell returns to that shell. Exiting the **session shell itself** ends the background run: the container stops but is not automatically removed. `attach` must not silently start a shell, including if the session exits while a client is connecting. For a stopped target, report e.g. `session stopped; use 'makeslop bg restart <uuid>'`. If Docker is running but the session is unavailable, fail with a restart hint rather than opening a replacement terminal.

Stopping or restarting is **not** a way to resume live process memory. Restart uses the same Docker container, mounts, environment, and masks; it does not reload project YAML, pick up changed host environment values, or rescan for newly created secrets. To apply new settings or masks, stop the old container and use `bg run` to create a new one. Bind-mounted host file changes remain visible according to the original mounts.

## Identity and discovery

Generate a random UUID v4 on each real creation and name the Docker container `makeslop-<uuid>`. The prefix is internal: makeslop's normal output, diagnostics, examples, and command arguments use only the UUID. A Docker-level projection such as `--dry-run` must show the actual Docker name to accurately describe the request; it is not an alternative user handle.

Docker is the source of truth. List by name prefix, then validate the **entire** name against the generated name format; a substring or an arbitrary `makeslop-*` name is not a match. For an action resolve the exact managed name and act on its inspected Docker ID, not an arbitrary argument passed through to Docker. Store no host session registry or current selection. Project display information may come from container metadata such as its configured workdir. A matching name is a convention, not an ownership or authorization check: users sharing a Docker daemon can see and control the same containers. Renaming one outside makeslop can take it out of discovery.

## Image and launch contract

- **Background images must have [zmx](https://zmx.sh/getting-started) on `PATH` inside the image.** Do not require a host installation, install it at launch, build an image, or pull one. `bg ls`, `stop`, and `remove` operate through Docker and must not execute zmx. Foreground `makeslop run` remains unchanged and does not require zmx. Document the requirement under custom images as well as requirements in `docs/reference.md`.
- Share foreground `run`'s image resolution, workspace and home guards, ordered joins, secret scan, masks, caches, project environment, networks, and sandbox flags. Support `--image/-i`, `--join/-j`, `--out-of-home`, and `--dry-run/-n`. Dry run renders the background launch, skips Docker and source creation, and does not suggest that its example UUID is a live target.
- The configured shell (currently `/bin/zsh` by default) is the session payload. Preserve the image's normal initialization when its entrypoint delegates to the supplied command. Do not hardcode an agent, assume a login shell, or launch a fresh copy of the image entrypoint on each attach. An incompatible entrypoint or missing zmx must produce a startup error, not a UUID for an unusable session.
- Use the Moby SDK for all Docker operations. Create without auto-remove. Keep a foreground process in the container tied to the lifetime of the persistent session, independent of any host client. Start the shell with a PTY even on a piped `bg run`. Attach via an interactive Docker exec client under the same container user and zmx socket location. Session sockets must be private to the container, not placed in a project or shared cache.
- Bound startup/readiness checks, not the duration of an attached terminal. On failed initial startup, return nonzero, print no UUID on stdout, and clean up only the newly created container. If cleanup fails, report its UUID for recovery. Failed restart retains the existing container for inspection/removal. Ordinary attach errors never remove a container.

## Approaches considered

- **Selected: `bg` namespace + zmx.** Keeps lifecycle verbs separate from workspace `ls`/`remove`; gives one persistent terminal without asking users to manage panes. Each attach uses a new client but reaches the same PTY.
- **`run -d` + lifecycle commands.** Familiar Docker idiom, but splits creation and management across command groups. A plain exec of the configured shell would also open a different terminal on every connection.
- **`bg` + tmux.** Supports persistent terminals and an existing-session-only attach; adds a multiplexer interface we don't need. zmx was selected instead. Its documented `attach` creates a missing session, so a bare `zmx attach` is **not** sufficient evidence that the existing-session-only contract is met.

## Acceptance and remaining builder decisions

- Exercise two launches for one project, management outside a workspace, stopped and empty listing, invalid/foreign names, pipe-safe stdout, missing zmx, stop/restart/remove transitions, and failed-start cleanup. Verify unchanged foreground behavior and security flags/masks in the SDK and dry-run projections.
- Test with a real zmx-equipped image: set a shell variable, detach, then reconnect and read it. A second client must see the same shell. After restart the variable must be gone but a file in the container's writable layer must remain. After exiting the session shell, attach must fail, including during a concurrent attach/exit; no client may land in a new shell.
- Before settling on a launch wrapper, verify zmx version and commands, headless startup, shell-exit detection, attach/exit races, resize and client disconnect with the supported image. A check-then-attach sequence alone cannot guarantee existing-session-only behavior. If the chosen backend cannot meet this contract, revisit the behavior rather than silently creating sessions. Record the tested zmx version in image documentation.
- Out of scope: multiple named terminals per container, arbitrary `bg exec`, session recovery across container restarts, short-ID resolution, automatic image installation, a host-side daemon, and changes to the foreground command.
