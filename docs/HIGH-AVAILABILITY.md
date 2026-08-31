# High availability

This document describes how to run more than one `excalidraw-wopi`
instance, for uptime beyond one process. It assumes you have read
`docs/ARCHITECTURE.md`'s "Peer routing" section; this document is the
deployment-facing companion to it.

## Setup 1: DNS health checks

Recommended for Kubernetes, and for VM fleets that already run Consul.
Peer discovery re-resolves a DNS name every 5 seconds and treats every
returned address as a live peer, so the arbiter is whatever keeps that
name's records limited to healthy instances.

Two variables configure it:

- `EXCALIDRAW_WOPI_DNS_PEERS` — `<host>:<port>` of the peer-discovery
  DNS name. Every A/AAAA record behind it becomes a peer at
  `http://<addr>:<port>`. Empty means single-replica mode: no routing
  layer, no peer lookups.
- `EXCALIDRAW_WOPI_DNS_SELF` — this instance's own advertised URL, as
  another instance reaches it. Required whenever `DNS_PEERS` is set.

Every instance must also share the same session secret
(`EXCALIDRAW_WOPI_SESSION_SECRET` or `_SECRET_FILE`) and the same
proof key. Cross-instance token verification and WOPI proof signing
both need identical secrets everywhere.

### Kubernetes: Deployment plus a headless Service

The Helm chart at `deploy/helm/excalidraw-wopi` implements this
manifest set. It is published as an OCI artifact:

```sh
helm install excalidraw-wopi oci://ghcr.io/zeylos/charts/excalidraw-wopi \
  --version <version> \
  --set config.publicUrl=https://excalidraw.example.org \
  --set 'config.wopiAllowedOrigins={https://drive.example.org}'
```

Scale with a plain `Deployment` and its `replicas:` count, not a
`StatefulSet`. A second, headless `Service` in front of the same pods
gives every instance the peer set through DNS. The normal,
client-facing `Service` stays as it is. Keep
`publishNotReadyAddresses: false` on the headless `Service`: this
gates the peer set on readiness, so a crashlooping pod never joins DNS
and never owns a room. `DNS_SELF` comes from the downward API, each
pod's own IP. The YAML below is the reference shape the chart
renders:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: excalidraw-wopi-peers
spec:
  clusterIP: None
  publishNotReadyAddresses: false
  selector:
    app: excalidraw-wopi
  ports:
    - port: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: excalidraw-wopi
spec:
  selector:
    app: excalidraw-wopi
  ports:
    - port: 8080
      targetPort: 8080
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: excalidraw-wopi
spec:
  replicas: 2
  selector:
    matchLabels:
      app: excalidraw-wopi
  template:
    metadata:
      labels:
        app: excalidraw-wopi
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        fsGroup: 65532
      volumes:
        - name: excalidraw-wopi-secrets
          secret:
            secretName: excalidraw-wopi-secrets
            defaultMode: 0440
      containers:
        - name: excalidraw-wopi
          image: ghcr.io/zeylos/excalidraw-wopi:<version>
          ports:
            - containerPort: 8080
          volumeMounts:
            - name: excalidraw-wopi-secrets
              mountPath: /etc/excalidraw-wopi
              readOnly: true
          readinessProbe:
            httpGet:
              path: /healthz
              port: 8080
            periodSeconds: 5
          env:
            - name: POD_IP
              valueFrom:
                fieldRef:
                  fieldPath: status.podIP
            - name: EXCALIDRAW_WOPI_DNS_SELF
              value: "http://$(POD_IP):8080"
            - name: EXCALIDRAW_WOPI_DNS_PEERS
              value: "excalidraw-wopi-peers:8080"
            - name: EXCALIDRAW_WOPI_PROOF_KEY_PATH
              value: /etc/excalidraw-wopi/proof-key.pem
            - name: EXCALIDRAW_WOPI_SESSION_SECRET_FILE
              value: /etc/excalidraw-wopi/session-secret
```

The `Secret` named `excalidraw-wopi-secrets` holds two keys:
`proof-key.pem` and `session-secret`. The volume mount makes each key
a file of the same name under `/etc/excalidraw-wopi`.

Scaling is the `replicas:` field only. No other field changes when the
replica count changes.

### VMs: Consul health checks

Register each node as a Consul service, with an HTTP check against its
own `/healthz`. Consul removes a node from DNS answers as soon as its
check fails, so the peer set follows node health without any change to
`excalidraw-wopi` itself. On node `10.0.1.10`:

```json
{
  "service": {
    "name": "excalidraw-wopi",
    "port": 8080,
    "address": "10.0.1.10",
    "check": {
      "http": "http://10.0.1.10:8080/healthz",
      "interval": "5s",
      "timeout": "2s"
    }
  }
}
```

The peer-discovery name is then `excalidraw-wopi.service.consul`. Each
node's own resolver must forward the `.consul` zone to the local
Consul agent's DNS interface, on port 8600. With dnsmasq:

```
server=/consul/127.0.0.1#8600
```

Or with systemd-resolved, a drop-in under
`/etc/systemd/resolved.conf.d/`:

```ini
[Resolve]
DNS=127.0.0.1:8600
Domains=~consul
```

Each node then sets the resulting env pair (`DNS_SELF` names that
node's own address, not the same value on every node):

```sh
EXCALIDRAW_WOPI_DNS_PEERS=excalidraw-wopi.service.consul:8080
EXCALIDRAW_WOPI_DNS_SELF=http://10.0.1.10:8080
```

### The resolver-caching caveat

`excalidraw-wopi` re-resolves `DNS_PEERS` every 5 seconds. Any caching
resolver between the service and the health-aware DNS answer (a
distribution's local stub resolver, a sidecar, a corporate forwarder)
must honor a low TTL on that record. A resolver that caches past the
health check's own update rate holds a stale peer set, and a node that
just failed its check can stay a peer until the cache entry expires.

### Failure behavior

- **A crashlooping Kubernetes pod.** The pod never turns ready, so
  `publishNotReadyAddresses: false` keeps it out of the headless
  Service's DNS records. It never becomes a peer, and it never owns a
  room.
- **A Consul node failing its health check.** Consul stops answering
  that node's address once the check fails. The node drops out of
  every other node's peer set on the next 5-second re-resolve.
- **A rollout or a scale event.** Peers re-resolve every 5 seconds, so
  a rolling update or a scaling change can leave instances with briefly
  different peer sets. A room can then briefly exist on two instances.
  This is the tolerated transient described in
  `docs/ARCHITECTURE.md`'s "Peer routing" section. It clears itself
  within about one save interval (60 seconds). No operator action is
  needed.

## Setup 2: a load balancer with a consistent hash on `room`

Recommended everywhere else: bare VMs, a PaaS, or any fleet without
Consul. Leave every peer variable unset. Each instance runs alone,
with no routing layer of its own and no knowledge that another
instance exists. The load balancer in front of them is the arbiter: it
decides which instance sees a given file's traffic, and it detects a
dead instance on its own.

**The hash is mandatory, not an optimization.** Every client request
for a file carries `?room=<fileId>` — on the `/socket.io/` handshake
and on every `/api/board*` call. Route two requests for the same file
to two different instances, and each opens its own room, takes its own
WOPI lock, and saves on its own schedule. A plain round-robin or
least-connections balancer creates one room per instance per file, and
a permanent stream of save conflicts. The balancer must hash on the
`room` query parameter and send every request for one file to the same
instance.

All three examples below front the same set of independent instances,
each running the full `excalidraw-wopi` binary on its own port, and
each proxying every path (`/`, `/launch`, `/api/`, `/socket.io/`,
`/hosting/discovery`) with one rule, per `docs/DEPLOYMENT.md`'s
"Ingress" section. Each example also passes a WebSocket connection
through unmodified; a `/socket.io/` client is websocket-only, so the
balancer only needs to forward the upgrade, not understand it.

### Caddy

```
excalidraw.example.org {
    reverse_proxy 10.0.1.10:8080 10.0.1.11:8080 10.0.1.12:8080 {
        lb_policy query room
        health_uri /healthz
        health_interval 5s
        health_timeout 2s
    }
}
```

`lb_policy query room` hashes on the `room` query parameter. Caddy's
built-in active health check on `/healthz` removes a failed instance
from the backend set within `health_interval`.

### HAProxy

```
frontend excalidraw_wopi_fe
    bind *:443 ssl crt /etc/haproxy/certs/excalidraw.pem
    default_backend excalidraw_wopi_be

backend excalidraw_wopi_be
    balance url_param room
    option httpchk GET /healthz
    http-check expect status 200
    timeout tunnel 1h
    server node1 10.0.1.10:8080 check
    server node2 10.0.1.11:8080 check
    server node3 10.0.1.12:8080 check
```

`balance url_param room` hashes on the same query parameter. `option
httpchk` adds an active health check against `/healthz`; `check` on
each `server` line turns it on for that server.

### nginx (open source)

```
map $http_upgrade $connection_upgrade {
    default upgrade;
    '' close;
}

upstream excalidraw_wopi {
    hash $arg_room consistent;
    server 10.0.1.10:8080 max_fails=3 fail_timeout=10s;
    server 10.0.1.11:8080 max_fails=3 fail_timeout=10s;
    server 10.0.1.12:8080 max_fails=3 fail_timeout=10s;
}

server {
    listen 443 ssl;
    server_name excalidraw.example.org;

    location / {
        proxy_pass http://excalidraw_wopi;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_set_header Host $host;
    }
}
```

`hash $arg_room consistent` reads the `room` query parameter and
hashes on it. Open-source nginx has no active health check: that
feature is a paid nginx Plus feature. `max_fails=3 fail_timeout=10s`
gives passive detection instead — nginx marks a server down after 3
failed proxied requests, and retries it after 10 seconds.

### The enforcement difference

`docs/ARCHITECTURE.md`'s "Peer routing" enforces ownership at the
token layer: a non-owner instance rejects a mismatched file id even
when a proxy sent it the request. Setup 2 has no such second check,
because its instances do not know about each other. A client that
tampers with its own `?room=` value can reach a non-owner instance
directly and open a duplicate room for that file. The damage stays
bounded to files that client can already write, through its own
session token; it cannot use this to reach or corrupt a file it has no
access to.

`make e2e-ha` exercises this topology: two instances behind a hashing
proxy, against the fake WOPI host, proving that same-file traffic
converges on one instance and that a failover reroutes cleanly.

## Choosing

| | Arbiter | Failover speed | Extra components | Recommended for |
|---|---|---|---|---|
| Setup 1: DNS health checks | The DNS answer behind `DNS_PEERS`, kept live by a health-checked registry | Seconds: the registry's own check interval, plus the service's 5-second re-resolve | Kubernetes: none beyond the cluster itself. VMs: a Consul agent per node | Kubernetes; VM fleets with Consul |
| Setup 2: LB with a consistent hash | The load balancer's own health check | Seconds: the balancer's own check interval | A hashing-capable load balancer | Everywhere else |

Both setups reach the same underlying safety: one owner per file, a
deterministic lock, and a bounded, client-healed data-loss window. Pick
Setup 1 where a health-aware DNS answer already exists, in Kubernetes
or a Consul-managed fleet. Pick Setup 2 where a load balancer with
consistent hashing is easier to deploy than a service registry.
Either one is more than a single supervised instance needs, so start
there and move to multi-instance only once one process is not enough.
