# Running jobs-registry on Kubernetes

`k8s.yaml` holds the Deployment + PVC + Service. Fill in the image and the
jobs-server endpoint ID (printed in the server's startup log), then:

```sh
kubectl apply -f k8s.yaml -n <namespace>
```

Notes on the manifest:

- **One replica per volume.** The amber store dir is flocked — exactly one
  process per volume. `strategy: Recreate` moves the PVC with the pod. To
  scale, deploy independent Deployment+PVC pairs; blob digests are
  reproducible so the caches agree.
- **fsGroup 65532.** The image is distroless `nonroot`; without `fsGroup`
  the fresh PVC is root-owned and the registry fails with
  `mkdir /data/repos: permission denied`.
- **Probes hit `/v2/`,** which answers whenever the daemon is up — by design
  the registry keeps serving cached images while the jobs-server is
  unreachable, so probes must not depend on upstream health.

## Letting the kubelet pull from it

Pods can reach `jobs-registry.<namespace>.svc:5000`, but **image pulls
can't**: pulling is done by containerd on the host, which resolves names via
the node's `/etc/resolv.conf` — cluster DNS is never consulted, so `*.svc`
names don't exist and the pull fails with `no such host`. (Plain HTTP is a
second obstacle: containerd assumes HTTPS for anything except localhost.)

Both problems disappear if the registry is reachable as `localhost:5000` on
every node: containerd allows plain HTTP for localhost out of the box, and
no node-level registry configuration (`registries.yaml`, `certs.d`, restarts)
is needed. `registry-proxy.yaml` does exactly that — a DaemonSet running an
nginx stream proxy with `hostPort: 5000` pinned to `hostIP: 127.0.0.1`,
forwarding to the Service through cluster DNS (the proxy is a pod, so it
*does* resolve `*.svc` names):

```sh
kubectl apply -f registry-proxy.yaml -n <namespace>
```

Then reference images as:

```yaml
image: localhost:5000/jobs:<build hash>
```

Details worth knowing:

- **`hostIP: 127.0.0.1` is load-bearing.** Without it the hostPort binds on
  all interfaces and anyone on the node's network can pull images.
- **No resolver IPs are hard-coded.** nginx resolves the `resolver`
  directive's own hostname (`kube-dns.kube-system.svc.cluster.local`) at
  config load via the pod's `/etc/resolv.conf`; the backend service name is
  re-resolved at runtime through a `map` variable, so a recreated Service
  with a new ClusterIP is picked up within `valid=`.
- **Tags are content hashes,** so `imagePullPolicy: IfNotPresent` is safe and
  keeps pods startable while the registry is down; `Always` re-checks the
  registry on every pod start.
- Adjust the DaemonSet's `tolerations` to cover any tainted nodes that must
  pull jobs images.
