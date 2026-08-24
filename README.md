<p align="center"><img src="ui/static/logo-animated.svg" alt="Le Squat — le veilleur // who needs it up?" width="640"></p>

# Le Veilleur

**The watchman of a home lab: machines are awake exactly as long as somebody
has claimed them, and asleep the rest of the time.** A workload says *"I need
the console"* or *"I need the backup server"*; Le Veilleur works out what that
requires — wake the tower, then start the guest — does it in order, and puts
everything back to sleep when the last claim is gone.

It exists because power policy has a way of ending up scattered: a timer here
that stops a VM, a cron there that powers off a node in a window someone chose
for a different workload. Each one is correct alone; together they cannot do
arithmetic. Two workloads on one machine is all it takes:

> The backups finish at 05:10 and release the backup server — but somebody is
> still playing at 06:00, so the tower must stay awake. The backup VM should
> stop anyway.

That is not a special case here. It is what refcounting does.

[La Loge](https://github.com/tomblancdev/la-loge) keeps the door and
[Le Videur](https://github.com/tomblancdev/videur) decides who passes; Le
Veilleur keeps watch while the house sleeps.

## The one object

A **claim**: *subject needs target up, because reason, until condition.*

**Targets** are nodes, guests, and (later) services, and they declare what
they **require**. The rule, whole:

> A target is up while any claim on it — or on anything that requires it — is
> held. It comes down when that set is empty, its grace has passed, and no
> guard objects.

```
 a game server ──requires──▶ the servers VM ──requires──┐
                                                        ├──▶ muscle1 (on demand)
 the console  ─────────────────────requires─────────────┤
 the backups  ─────────────────────requires─────────────┘
```

**Guards** only ever refuse to *stop* something — never to start it: somebody
logged in, a maintenance pass, a converge running, the cluster not whole
(never leave a quorum short), an HA-managed guest. A guard that cannot be
evaluated counts as occupied.

## What it will not do

- **Never powers a 24/7 node.** A node target must declare `on_demand: true`
  or the config is refused, and each node's own copy of the forced command
  refuses a `poweroff` it was not built to accept.
- **Never stops an HA resource.** That is the cluster manager's job.
- **Never acts blind.** If it cannot observe the fleet it does *nothing* —
  losing power savings is cheap; powering off a machine somebody is using is
  not. It fails *as-is*, which is the opposite of how a bouncer should fail.
- **It is not a scheduler.** Your timers still fire; only the power decisions
  move here.

## Talking to it

An [OpenAPI](internal/web/openapi.go) document at `/openapi.json` — no SDK,
no library. The verb most clients want is *ensure*:

```sh
curl -sX POST -H "Authorization: Bearer $TOKEN" \
     -d '{"reason":"play page: wake it","hold":"6h"}' \
     https://veilleur.example.net/api/targets/console/ensure
# 202 {"claim":{...},"up":false,"eta_seconds":240,"chain":[{"name":"muscle1",...}]}
```

Then release it when you are done — or let it expire, because **everything
expires**: a claim carries a deadline whatever its release rule, so a client
that dies cannot pin a machine on forever.

`explicit` (the client says when) · `idle` (the target reports it is unused,
refreshed by heartbeats) · `deadline` (the clock alone).

Humans get a board at `/`: what is up, what holds it up, what is refusing to
sleep and why, and a *hold it for 2 h* button.

## Reporting from inside

A target can say *"I am in use"* instead of being guessed at. `deploy/agent/veilleur-report`
is a POSIX shell script (curl, nothing else) that holds an **idle-ruled
claim** on its own target and refreshes it while an activity check passes:

```sh
veilleur-report --url https://veilleur.example.net \
                --target console --activity /usr/local/bin/is-someone-playing
```

The activity check is whatever "in use" means for that service — exit 0 = in
use — and it lives with the service, not here. When activity stops the
reporter simply stops talking; the claim ages out on the server's side and
**Le Veilleur** decides what to do about it. A reporter that dies cannot slam
the door, and a brief pause does not cost a restart.

That division is the point, and it was learned the hard way: a guest that
decides for itself races the watchman. In this design's first week the games
console's own 20-minute watchdog powered the box off twice *inside a held
claim*, and the watchman dutifully restarted it each time — two interruptions
to a real game. **One decider, many reporters.**

## How it touches machines

One ssh key restricted to one forced command on every hypervisor —
[`squat-veilleur`](deploy/proxmox/squat-veilleur): `status`, `wake <node>`,
`start <vmid>`, `stop <vmid>`, `poweroff`. Each node renders its own
allowlist, so the key is not a power-of-attorney over the cluster: a node
refuses a vmid that is not in the graph, and a 24/7 node refuses to sleep at
all. Stops are graceful; nothing is ever forced.

## Run it

```sh
podman run --rm -p 8080:8080 \
  -v ./config.yaml:/etc/veilleur/config.yaml:ro -v ./data:/data \
  ghcr.io/tomblancdev/veilleur:0.3.1
```

`scratch` + one binary, uid 65532, read-only root. Config:
[`targets.example.yaml`](targets.example.yaml) — the whole product in one
file. `door.mode: mock` gives you an imaginary fleet that touches nothing.
Structured JSON logs on stdout; Prometheus metrics at `/metrics`, including
`veilleur_node_powered_seconds_total`, which is how you find out whether any
of this worked. Reference unit:
[`deploy/quadlet/veilleur.container`](deploy/quadlet/veilleur.container).

## Develop

No toolchain on the host: `podman run --rm -v "$PWD":/src -w /src golang:1.24-alpine go test ./...`.
The engine's arithmetic — including the 06:00 case above — is covered in
[`internal/fleet/engine_test.go`](internal/fleet/engine_test.go) against an
in-memory fleet. Tags `v*` build and push the image.

## License

MIT — Tom Blanc. The mark and the faces belong to the
[La Loge](https://github.com/tomblancdev/la-loge) family (Big Shoulders
Stencil + IBM Plex Mono, OFL, embedded).
