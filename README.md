<p align="center"><img src="ui/static/logo-animated.svg" alt="le veilleur — who needs it up?" width="640"></p>

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

## Three configs, two graphs, one decider

**`signals/`** — a named question. A command runs on a node; a zero exit means
the sentence in `means:` is true. That is the only kind of fact this program
has.

```yaml
console_in_use:
  run_on: tower        # its NODE. Nothing of ours runs inside a guest.
  means: "a client is connected, or somebody has a shell on it"
  ttl: 90s
```

**`targets/`** — what exists, what it `needs` before it can start, and how
long after being raised it is allowed to be considered idle.

**`down/`** — when a thing may stop, and what that may free. **Deliberately
not the inverse of `needs`**, because things also get started by hand, by an
autostart flag, or by another service — and a stop chain derived from the
wake chain cannot see any of them.

```yaml
tower:
  stop_when:
    - "!any_guest_running"   # measured, not inferred from who asked
    - "!human_session"
    - "!hold:tower"
    - "cluster_whole"        # never leave a cluster short - a plain signal
  grace: 10m
  manages: [tower]         # the ONLY things this may stop
```

## The rules, each one paid for

- **A stop needs a positive answer, never an absence.** "Nothing has claimed
  it" and "no log arrived" are not evidence that nobody is using a machine.
- **UNKNOWN blocks a stop; it never permits one.** A question that cannot be
  put reads as *possibly in use*.
- **Only what a `manages` list names is ever stopped.** A guest someone
  started by hand is untouched — and it keeps its machine up, which is
  correct.
- **A thing just raised is not yet idle** (`min_uptime`). Between "raised"
  and "in use" every activity signal honestly says no. Without a floor, the
  stop path undoes the wake — which is exactly how a backup server was
  stopped one minute after being woken for the night's backup.
- **Backstops may wake. Backstops may not stop.** A stop-backstop races the
  thing it is backing up; a wake-backstop can only cost idle minutes.
- **Observing may use a cached answer. Stopping may not.** Every signal is
  cached for its `ttl`, which is right for drawing the board and for letting a
  grace run — but the moment a stop actually fires, every condition is asked
  again, fresh, with the target's lock held. A node was once powered off six
  seconds after a guest started on it, from an answer that was 60s old and
  perfectly "fresh" by the observing rules. A stop is the one act that cannot
  be taken back, so its last word has to be the current one.

## Holding something up

A **hold** is the only state a person writes, and the only thing without an
expiry — because a person decided, and a person can be asked. It carries who
and why, and it ages loudly. `hands_off` additionally refuses to *start* the
target: for when you are working on the machine.

## Nothing of ours runs inside a guest

A guest is *observed* from its node. Where the firewall is on a guest's NIC
the node's own conntrack already carries that guest's flows, tagged with its
vmid — so "is anybody using this?" is a question the node can answer with no
agent, no credential and no door opened into the guest. Where that is not
enough, `qm guest exec` runs a command in the guest over virtio, which is
still a command on a hypervisor.

**But conntrack only answers the question you asked.** A signal written
against ESTABLISHED *tcp* cannot see a *udp* session, and for a game stream
the tcp ports are handshake only — they are all closed by the time anyone is
actually playing. The first version of `console_in_use` did exactly this and
shut a games console down under its player, three times in one evening, while
the only ESTABLISHED tcp connection on the box belonged to the status panel
polling it. Write the signal against the traffic that flows *during* use, and
prove it against a real session before trusting it.

## How it touches machines

One ssh key restricted to one forced command on every hypervisor —
[`veilleur-node`](deploy/proxmox/veilleur-node): `signal <name>`,
`state <target>`, `up <target>`, `down <target>`, `list`.

**The node holds the commands; Le Veilleur only names them.** Every name is
looked up in that node's own table and never interpolated into a shell, so
the watchman may ask for `signal console_in_use` but cannot ask for
`rm -rf /`, and cannot touch a target the node was not given. **A 24/7 node
refuses to sleep because it simply has no `down:` entry for itself** — not
because anything remembered to check.

## Run it

```sh
podman run --rm -p 8080:8080 \
  -v ./config.yaml:/etc/veilleur/config.yaml:ro -v ./data:/data \
  ghcr.io/tomblancdev/veilleur:0.4.0
```

`scratch` + one binary, uid 65532, read-only root. Config: [`example/`](example) — `main.yaml` plus the three directories, and
the `commands.conf` a node holds. `door.mode: mock` gives you an imaginary fleet that touches nothing.
Structured JSON logs on stdout; Prometheus metrics at `/metrics`, including
`veilleur_node_powered_seconds_total`, which is how you find out whether any
of this worked. Reference unit:
[`deploy/quadlet/veilleur.container`](deploy/quadlet/veilleur.container).

## Develop

No toolchain on the host: `podman run --rm -v "$PWD":/src -w /src golang:1.24-alpine go test ./...`.
The engine's arithmetic — including the 06:00 case above — is covered in
[`internal/fleet/engine_test.go`](internal/fleet/engine_test.go) against an
in-memory fleet. Tags `v*` build and push the image.

## This repo carries no environment

Addresses, hostnames, domains and the house word belong to whoever runs it;
the only thing crossing between a deployment and this repo is a pinned image
tag. The example fleet and the tests use the documentation reserves —
`192.0.2.0/24` (RFC 5737), `example.com` (RFC 2606) — and generic machine
names (`node-a`, `node-b`, `tower`), so nothing here describes a real fleet.
Set `house:` and the wordmark is redrawn with it, so the same binary shows
`LE VEILLEUR` to a stranger and their own word to the people who run it.
`sh tools/no-environment.sh` enforces it; CI runs it before anything else.

## License

MIT — Tom Blanc. The mark and the faces belong to the
[La Loge](https://github.com/tomblancdev/la-loge) family (Big Shoulders
Stencil + IBM Plex Mono, OFL, embedded).
