# Opt-in zmx background smoke test

Not run in the development sandbox: no Docker daemon/socket is available. SDK tests do not prove zmx and Docker PTY behavior. Run this procedure with a local image you control that contains zmx (start with 1.0.2) and `/bin/sh`. Record the daemon version, image ID, zmx version, container UID, shell, workdir, and result for each check. Leave acceptance pending until both the anchor and CLI checks pass. The Docker CLI here is a manual test tool only; makeslop does not invoke it.

```sh
image=your-local-zmx-image
id=$(docker run --pull=never -dit --tmpfs /tmp \
  -e ZMX_DIR=/tmp/makeslop-zmx -e ZMX_DIR_MODE=0700 \
  -e ZMX_SESSION= -e ZMX_SESSION_PREFIX= \
  "$image" zmx attach agent /bin/sh)
docker exec "$id" zmx version
docker version --format '{{.Server.Version}}'
docker inspect --format '{{.Image}} {{.Config.WorkingDir}} {{json .Config.Cmd}}' "$id"
docker exec "$id" id -u
docker exec "$id" pwd
docker exec "$id" zmx get agent  # exit 0, even with no labels
docker exec "$id" zmx get absent-session  # expect exit 1; must not create it
docker exec "$id" zmx list --short  # only agent
docker exec -it "$id" zmx attach agent /bin/sh
```

1. Check that startup requires no host input. Verify the two `get` exit statuses above and that the session list did not change.
2. Set `marker=alive` in the shell, print enough output to exceed normal stream buffers, and press Ctrl-\ to detach. Reattach with the last command and check `echo "$marker"`.
3. Attach from a second terminal, resize it, and check `stty size`. Close a host terminal and verify another client can reconnect. Check that the main client doesn't override the active client's terminal size.
4. Exit the persistent shell. `docker wait "$id"` must return; the stopped container must still exist. `docker start "$id"` must create a fresh session without the variable. Stop and remove this test container after the check.
5. Repeat startup with an image missing zmx and one with a non-delegating entrypoint. Neither may produce a usable session.

## Verify the makeslop CLI

From a registered test project, use a zmx-equipped local image:

```sh
first=$(makeslop bg run -i "$image")
makeslop bg run --attach -i "$image"  # store the UUID printed to stderr as `second`
```

1. In the second session set a non-exported `marker=alive`. Detach, attach twice (including once from another working directory), and check both clients see the same `marker`. Close one host terminal; the other should continue to work.
2. Use `makeslop bg exec "$second"`. Check the new shell cannot see `marker`; set a test file in the container's writable layer, not `/tmp`, the project, or a cache. Exit and confirm the persistent shell still has `marker`. Check `makeslop bg exec "$second" -- /bin/sh -c 'exit 23'` returns 23.
3. `bg start` on the running UUID must leave `marker` intact. After `bg stop`, `bg ls` must include the stopped UUID and `bg attach` must fail with a `bg start` hint. After `bg start` or `bg restart`, the variable must be gone and the test file must persist. `bg remove` must refuse a running container. Stop and remove both test UUIDs when done.
4. Separately create an unlabelled stopped test container named `makeslop-<unused-uuid-v4>`. Verify its UUID does not appear in `bg ls` and `bg remove <unused-uuid-v4>` leaves it alone. Remove it directly with Docker after the test.
5. Attempt detached launch with a local image lacking zmx. It must fail and must not print a UUID to stdout or leave a managed background container behind.
