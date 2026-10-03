# WireGuard egress proxy for Docker

Sends all egress traffic of selected Docker containers (TCP, UDP, DNS, QUIC) through a WireGuard tunnel to a VM you control. Containers opt in with `--network container:vpn-proxy`.

```
 Docker host (Mac: Docker Desktop / OrbStack)              Egress VM (Debian/Ubuntu)
┌──────────────────────────────────────────┐      ┌──────────────────────────────────┐
│ your container                           │      │                                  │
│   --network container:vpn-proxy          │      │  wg7  10.66.0.1/24               │
│        │  (shares the network namespace) │      │   │  ip_forward + MASQUERADE     │
│        ▼                                 │ UDP  │   ▼                              │
│ vpn-proxy  wg0 10.66.0.2 ────────────────┼──────┼─► exit interface ──► internet    │
│   kill switch: no tunnel → no traffic    │57777 │   (eth0, or an upstream VPN)     │
└──────────────────────────────────────────┘      └──────────────────────────────────┘
```

## 1. VM (server)

Run all of section 1 in **one root shell** (`sudo -i`): later steps use variables set earlier.

### 1.1 Packages

```sh
apt install -y wireguard iptables
```

`wg-quick` needs the `iptables` command used below.

### 1.2 IP forwarding

```sh
echo 'net.ipv4.ip_forward=1' > /etc/sysctl.d/99-wireguard.conf
sysctl --system
sysctl net.ipv4.ip_forward     # must print 1
```

### 1.3 Settings

```sh
ip route get 1.1.1.1           # note the interface after "dev"
```

- `dev eth0` (or `ens5` etc.): the VM goes to the internet directly. Use that interface.
- `dev wg0` (or another tunnel): the VM itself sits behind another VPN. Use that tunnel's interface; client traffic then exits with the upstream VPN's IP.

```sh
EXIT_IF=eth0                   # interface from "ip route get" above
VM_IP=203.0.113.10             # address the Docker host uses to reach the VM (LAN IP, public IP or DNS name)
WG_PORT=57777
```

### 1.4 Keys

```sh
cd /etc/wireguard && umask 077
wg genkey | tee server.key | wg pubkey > server.pub
wg genkey | tee client.key | wg pubkey > client.pub
```

### 1.5 Server config

```sh
cat > /etc/wireguard/wg7.conf <<EOF
[Interface]
Address    = 10.66.0.1/24
ListenPort = $WG_PORT
PrivateKey = $(cat /etc/wireguard/server.key)
# Reject forwarding if the chosen exit interface goes down.
PostUp   = iptables -t nat -A POSTROUTING -s 10.66.0.0/24 -o $EXIT_IF -j MASQUERADE; iptables -I FORWARD -i %i -j ACCEPT; iptables -I FORWARD -o %i -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT; iptables -I FORWARD -i %i ! -o $EXIT_IF -j REJECT
PostDown = iptables -t nat -D POSTROUTING -s 10.66.0.0/24 -o $EXIT_IF -j MASQUERADE; iptables -D FORWARD -i %i -j ACCEPT; iptables -D FORWARD -o %i -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT; iptables -D FORWARD -i %i ! -o $EXIT_IF -j REJECT

[Peer]
PublicKey  = $(cat /etc/wireguard/client.pub)
AllowedIPs = 10.66.0.2/32
EOF
chmod 600 /etc/wireguard/wg7.conf
cat /etc/wireguard/wg7.conf    # check: real keys and interface filled in
```

The `! -o $EXIT_IF -j REJECT` rule prevents fallback to the VM's real IP if an upstream VPN fails.

### 1.6 Client config

Generate it on the VM, where both keys already are. It gets copied to the Docker host in step 2.2.

```sh
cat > /root/client-wg0.conf <<EOF
[Interface]
Address    = 10.66.0.2/32
PrivateKey = $(cat /etc/wireguard/client.key)
# Kill switch: drop anything not leaving via the tunnel, except Docker-internal traffic.
# PostUp only - the rule stays if the tunnel goes down, which is the point.
PostUp = iptables -I OUTPUT ! -o %i -m mark ! --mark \$(wg show %i fwmark) -m addrtype ! --dst-type LOCAL -j REJECT; iptables -I OUTPUT -d 172.16.0.0/12 -j ACCEPT

[Peer]
PublicKey           = $(cat /etc/wireguard/server.pub)
Endpoint            = $VM_IP:$WG_PORT
AllowedIPs          = 0.0.0.0/0
PersistentKeepalive = 25
EOF
chmod 600 /root/client-wg0.conf
cat /root/client-wg0.conf      # check: PostUp must contain the literal text "$(wg show %i fwmark)"
```

- `AllowedIPs = 0.0.0.0/0` sends all IPv4 traffic through the tunnel. Don't add `::/0`: Docker disables IPv6 in containers, and wg-quick then fails with `RTNETLINK answers: Permission denied`.
- No `DNS =` line: DNS servers are set in `compose.yaml` (`dns:`), and those queries go through the tunnel too.

### 1.7 systemd unit

```sh
cat > /etc/systemd/system/wg7.service <<'EOF'
[Unit]
Description=WireGuard egress tunnel (wg7)
Documentation=man:wg-quick(8) man:wg(8)
After=network-online.target nss-lookup.target
Wants=network-online.target
Conflicts=wg-quick@wg7.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/bin/wg-quick up wg7
ExecStop=/usr/bin/wg-quick down wg7
# Applies peer/key changes without dropping the tunnel; PostUp/PostDown are not re-run
ExecReload=/bin/bash -c 'exec /usr/bin/wg syncconf wg7 <(exec /usr/bin/wg-quick strip wg7)'
Environment=WG_ENDPOINT_RESOLUTION_RETRIES=infinity

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now wg7
wg show wg7
iptables -t nat -L POSTROUTING -nv     # MASQUERADE ... out <EXIT_IF>
ip route get 1.1.1.1 from 10.66.0.2 iif wg7   # "dev" must be EXIT_IF
```

- This unit does the same job as the distro's `wg-quick@wg7.service`. Enable only one of them; the unit declares `Conflicts=` for this reason.
- `systemctl reload wg7` applies peer and key changes without dropping the tunnel. It does **not** re-run PostUp/PostDown. To change those lines: `systemctl stop wg7`, edit `/etc/wireguard/wg7.conf`, `systemctl start wg7`. Stopping first lets the old PostDown remove the old rules.

### 1.8 Firewall

Open **UDP 57777** inbound:

```sh
ufw allow 57777/udp      # only if ufw is enabled
```

Also open it in the cloud provider's firewall, or forward it on the router if the VM is behind NAT.

### 1.9 VM is an LXC container

- `iptables -t nat` fails with `Table does not exist`: run `modprobe nf_nat` (and `modprobe wireguard`) **on the LXC host**.
- `ip_forward` can't be set: enable it in the container's settings on the host.

---

## 2. Docker host (client)

### 2.1 Check Docker's kernel has WireGuard

```sh
docker run --rm --cap-add NET_ADMIN alpine sh -c 'apk add -q iproute2 && ip link add wg0 type wireguard && echo ok'
```

This should print `ok`. Docker Desktop and OrbStack both include WireGuard.

### 2.2 Project directory and client config

```sh
mkdir -p ~/vpn-proxy && cd ~/vpn-proxy
ssh user@VM 'sudo cat /root/client-wg0.conf' > wg0.conf
chmod 600 wg0.conf
ssh user@VM 'sudo rm /root/client-wg0.conf /etc/wireguard/client.key'   # VM only needs client.pub
```

`wg0.conf` holds the client's private key. Don't commit it.

### 2.3 compose.yaml

```sh
cat > compose.yaml <<'EOF'
services:
  vpn_proxy:
    image: lscr.io/linuxserver/wireguard:latest
    container_name: vpn-proxy
    cap_add:
      - NET_ADMIN
    sysctls:
      net.ipv4.conf.all.src_valid_mark: "1"
    dns:
      - 1.1.1.1
      - 8.8.8.8
    volumes:
      # Interface name comes from the target file name (wg0), not the source
      - ./wg0.conf:/config/wg_confs/wg0.conf:ro
    healthcheck:
      # Healthy only after a handshake with the VM
      test: ["CMD-SHELL", "wg show wg0 latest-handshakes | awk '$$2 > 0 {ok=1} END {exit !ok}'"]
      interval: 10s
      timeout: 3s
      retries: 6
      start_period: 5s
    restart: unless-stopped

networks:
  default:
    name: vpn-proxy-net    # fixed name so other projects can join it as an external network
    ipam:
      config:
        - subnet: 172.30.0.0/24    # must stay inside 172.16.0.0/12, the kill switch allows only that
EOF
```

### 2.4 Start

```sh
docker compose up -d
docker ps --filter name=vpn-proxy --format '{{.Names}}  {{.Status}}'   # wait for "(healthy)"
docker exec vpn-proxy wg show                                          # "latest handshake: N seconds ago"
```

---

## 3. Route a container through the tunnel

Share `vpn-proxy`'s network namespace. The container then has no network of its own: all of its IPv4 egress goes through `wg0`, including TCP, UDP, DNS and QUIC.

Raw `docker run`:

```sh
docker run --rm --network container:vpn-proxy curlimages/curl -s https://ifconfig.me
docker run -d --name myapp --network container:vpn-proxy my-image
```

Another compose project:

```yaml
services:
  app:
    image: my-image
    network_mode: "container:vpn-proxy"
```

Start `vpn-proxy` first. A service in the same `compose.yaml` can use `network_mode: "service:vpn_proxy"` instead; other projects need `container:`.

Things to know:

- **Ports.** `-p`, `--dns`, `--hostname`, `--add-host` and `--mac-address` can't be combined with `--network container:`. To expose a port, publish it on `vpn-proxy` in `compose.yaml` (`ports: ["8080:8080"]`).
- **Shared localhost.** All attached containers and `vpn-proxy` share one network stack. Two containers can't listen on the same port, and they reach each other via `127.0.0.1`.
- **Recreation.** If `vpn-proxy` is recreated (`docker compose up` after a config change, image update), attached containers lose their network. Restart them. A plain restart of `vpn-proxy` is fine.
- **Joining `vpn-proxy-net` is not enough.** `--network vpn-proxy-net` only puts a container on the same bridge; its traffic still goes out directly. Only `container:vpn-proxy` routes through the tunnel.
- **Kill switch.** If the tunnel is down, egress is rejected instead of leaking. Only `172.16.0.0/12` (Docker networks) bypasses the tunnel, so `host.docker.internal` (`192.168.65.x`) and your LAN are unreachable from attached containers.
- **IPv4 only.** IPv6 isn't tunneled; Docker disables it in containers by default.

---

## 4. Verify

From the Docker host:

```sh
docker run --rm curlimages/curl -s https://ifconfig.me                                # your real IP
docker run --rm --network container:vpn-proxy curlimages/curl -s https://ifconfig.me  # VM's exit IP
```

The second command must print a different IP: the VM's public IP, or the upstream VPN's exit IP if the VM exits through one.

On the VM, watch tunnel traffic (DNS should show up here too):

```sh
tcpdump -ni wg7 -c 20
tcpdump -ni wg7 udp port 53
```

Kill switch test:

```sh
docker exec vpn-proxy wg-quick down wg0
docker run --rm --network container:vpn-proxy curlimages/curl -sS --max-time 5 https://ifconfig.me   # must fail
docker restart vpn-proxy
```

---

## 5. Troubleshooting

Start with:

```sh
# Docker host
docker logs vpn-proxy 2>&1 | tail -30
docker exec vpn-proxy wg show
docker run --rm --network container:vpn-proxy curlimages/curl -sSv --max-time 10 http://1.1.1.1

# VM
wg show wg7
sysctl net.ipv4.ip_forward
iptables -t nat -L POSTROUTING -nv
iptables -L FORWARD -nv
```

| Symptom | Cause / fix |
|---|---|
| `wg7.service` fails, `iptables: command not found` | `apt install -y iptables`, then `systemctl restart wg7`. |
| `Table does not exist` / can't set `ip_forward` (LXC) | `modprobe nf_nat` on the LXC host; enable forwarding in the container's settings on the host. |
| No `latest handshake` in `wg show` | UDP 57777 blocked (VM firewall, cloud firewall, router), wrong `Endpoint`, or keys swapped. Each side must have its own private key and the **other** side's public key. |
| Handshake works, but `transfer` shows ~300 B received and curl hangs | Packets reach the VM but don't get out. Check `ip_forward` = 1, then the MASQUERADE counter. |
| MASQUERADE counter stays at `0`, FORWARD counter grows | Wrong exit interface. Run `ip route get 1.1.1.1 from 10.66.0.2 iif wg7`; the `dev` it prints must match `-o` in PostUp/PostDown. Fix: `systemctl stop wg7`, edit both lines, `systemctl start wg7`. |
| `tcpdump -ni wg7` shows packets from `10.66.0.2`, nothing on the exit interface | Same as above, or another firewall drops forwarded traffic: `nft list ruleset`, `iptables-legacy -S`. |
| Small requests work, large HTTPS downloads hang | MTU. Add `MTU = 1280` to `[Interface]` in `~/vpn-proxy/wg0.conf`, then `docker compose up -d --force-recreate`. |
| `RTNETLINK answers: Permission denied` in container logs | `::/0` in `AllowedIPs`; use `0.0.0.0/0` only. |
| Attached container has no network after `docker compose up` | `vpn-proxy` was recreated. Restart the attached containers. |
