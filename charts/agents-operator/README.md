# agents-operator

![Version: 0.0.0](https://img.shields.io/badge/Version-0.0.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.0.0](https://img.shields.io/badge/AppVersion-0.0.0-informational?style=flat-square)

Mission control for AI coding agents. One pod per session with its own PVC, the agent's real terminal streamed to the browser.

The hub runs as a single-replica Deployment (strategy `Recreate`; do not scale it) and creates one Pod, one PersistentVolumeClaim and one Secret per agent session in its own namespace. PostgreSQL comes from the Bitnami subchart by default.

## Install

```sh
helm repo add agents-operator https://dseif0x.github.io/agents-operator/
helm install agents-operator agents-operator/agents-operator -n agents-operator --create-namespace \
  --set publicUrl=https://agents-operator.example.com \
  --set ingress.host=agents-operator.example.com \
  --set auth.adminPasswordHash="$(AGENTS_OPERATOR_PASSWORD=... agents-operator hash-password)"
```

With Flux, add the repo as a `HelmRepository` and reference it from a `HelmRelease`. Secrets (`auth.existingSecret`, `postgresql.auth.existingSecret`, `database.existingSecret`) can be SealedSecrets.

The `auth.existingSecret` must contain `cookieSecret` (32+ bytes hex) and either `adminPasswordHash` (argon2id) or `adminPassword` (plaintext, hashed at startup).

## Session pod security

Every session pod runs non-root (UID 1000), with `readOnlyRootFilesystem`, all capabilities dropped, `seccompProfile: RuntimeDefault`, no service account token, no host namespaces and no host paths. None of this is configurable. `runner.networkPolicy` adds a default-deny policy: DNS to the cluster DNS (`dnsTo`), 443 and 22 outward minus `blockedCIDRs` (every private and link-local range, so neither the cluster, the nodes nor your LAN), plus `egressCIDRs`; inbound only from the hub on 7681.

## Requirements

Kubernetes: `>=1.27.0-0`

| Repository | Name | Version |
|------------|------|---------|
| oci://registry-1.docker.io/bitnamicharts | postgresql | 18.12.4 |

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Hub affinity |
| allowedHosts | list | `[]` | Extra allowed Host header values (the publicUrl host is always allowed) |
| auth.adminPasswordHash | string | `""` | argon2id hash for the admin user (`agents-operator hash-password`); if empty a random password is generated |
| auth.adminUsername | string | `"admin"` | Admin username |
| auth.cookieSecret | string | `""` | 32 bytes hex used to sign session cookies; generated if empty |
| auth.existingSecret | string | `""` | Existing Secret with keys `cookieSecret` and `adminPasswordHash` (or `adminPassword`); skips generation |
| database.existingSecret | string | `""` | Existing Secret holding the database URL (alternative to database.url) |
| database.existingSecretKey | string | `"url"` | Key in database.existingSecret |
| database.url | string | `""` | Database URL used only when postgresql.enabled=false |
| extraEnv | list | `[]` | Extra env for the hub container (list of EnvVar) |
| fullnameOverride | string | `""` | Override the full release name |
| image.pullPolicy | string | `"IfNotPresent"` | Hub image pull policy |
| image.repository | string | `"ghcr.io/dseif0x/agents-operator"` | Hub image repository |
| image.tag | string | `""` | Hub image tag; defaults to .Chart.AppVersion |
| imagePullSecrets | list | `[]` | Image pull secrets for both the hub and session pods |
| ingress.annotations | object | `{"cert-manager.io/cluster-issuer":"letsencrypt-dns","traefik.ingress.kubernetes.io/router.entrypoints":"websecure"}` | Ingress annotations |
| ingress.className | string | `"traefik"` | Ingress class |
| ingress.enabled | bool | `true` | Create an Ingress |
| ingress.host | string | `"agents-operator.homelab.seifert.id"` | Ingress host |
| ingress.tls | bool | `true` | Terminate TLS with a cert-manager issued certificate |
| ingress.tlsSecretName | string | `""` | TLS secret name; defaults to <fullname>-tls |
| logLevel | string | `"info"` | Hub log level: debug, info, warn, error |
| metrics.serviceMonitor.enabled | bool | `false` | Create a ServiceMonitor for kube-prometheus-stack |
| metrics.serviceMonitor.interval | string | `"30s"` | Scrape interval |
| metrics.serviceMonitor.labels | object | `{}` | Extra labels for the ServiceMonitor (e.g. release: kube-prometheus-stack) |
| nameOverride | string | `""` | Override the chart name |
| nodeSelector | object | `{}` | Hub node selector |
| podAnnotations | object | `{}` | Extra pod annotations for the hub |
| postgresql.auth.database | string | `"agents_operator"` |  |
| postgresql.auth.username | string | `"agents_operator"` |  |
| postgresql.enabled | bool | `true` | Install the Bitnami PostgreSQL subchart |
| postgresql.primary.persistence.size | string | `"5Gi"` |  |
| publicUrl | string | `"https://agents-operator.homelab.seifert.id"` | Public URL of the hub; cookies, Origin checks and the host allowlist derive from it |
| resources | object | `{"limits":{"cpu":"1","memory":"512Mi"},"requests":{"cpu":"100m","memory":"128Mi"}}` | Hub resources |
| runner.defaultSize | string | `"20Gi"` | Default PVC size for new sessions |
| runner.extraEnv | object | `{}` | Extra non-secret env injected into every session pod |
| runner.idleStopAfter | string | `"0h"` | Stop running sessions after this long without PTY output (0 = never) |
| runner.image.pullPolicy | string | `"IfNotPresent"` | Runner image pull policy |
| runner.image.repository | string | `"ghcr.io/dseif0x/agents-operator-runner"` | Runner image repository (session pods) |
| runner.image.tag | string | `""` | Runner image tag; defaults to .Chart.AppVersion |
| runner.maxResources | object | `{}` | The most a session may ask for, per limit (e.g. `limits: {cpu: "8", memory: 32Gi, nvidia.com/gpu: 2}`); a limit left out here is capped at its default |
| runner.networkPolicy.apiServerCIDRs | list | `[]` | Where session pods may reach the Kubernetes API once the runner ServiceAccount is on: CIDRs of the API server endpoints (control-plane node IPs) plus the `kubernetes` service IP. Empty = read from the cluster's `kubernetes` Endpoints and Service at install time (`helm template` cannot, and renders no rule) |
| runner.networkPolicy.apiServerPort | int | `6443` | Port of the API server on those endpoints (the service IP is always allowed on 443) |
| runner.networkPolicy.blockedCIDRs | list | `["10.0.0.0/8","172.16.0.0/12","192.168.0.0/16","100.64.0.0/10","169.254.0.0/16"]` | CIDRs excluded from the "443/22 anywhere" rules: every private, shared (CGNAT, Tailscale) and link-local IPv4 range, which covers the cluster's pod and service ranges, the nodes and the LAN around them. Open what sessions need with `egressCIDRs` |
| runner.networkPolicy.dnsTo | list | `[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"kube-system"}},"podSelector":{"matchLabels":{"k8s-app":"kube-dns"}}}]` | Where session pods may send DNS queries (NetworkPolicy "to" peers, port 53). Default: the cluster DNS pods (`k8s-app: kube-dns` in `kube-system`, as on k3s and most distributions). With NodeLocal DNSCache add its address as an ipBlock (e.g. `169.254.20.10/32`); `[]` allows DNS to any address |
| runner.networkPolicy.egressCIDRs | list | `[]` | Extra allowed egress CIDRs (all ports), e.g. an in-cluster LLM proxy or a git server on your LAN |
| runner.networkPolicy.enabled | bool | `true` | Create a default-deny NetworkPolicy for session pods |
| runner.networkPolicy.extraEgress | list | `[]` | Extra peers allowed to egress to on any port (raw NetworkPolicy "to" entries) |
| runner.networkPolicy.sessionEgress.allowedCIDRs | list | `["10.0.0.0/8","172.16.0.0/12","192.168.0.0/16","100.64.0.0/10"]` | Ranges a session may open for itself (its "local network access" list: an address or CIDR, optionally with a port), through a NetworkPolicy of its own on top of this one. Empty turns the option off |
| runner.networkPolicy.sessionEgress.deniedCIDRs | list | `["10.42.0.0/16","10.43.0.0/16"]` | Ranges no session may open even inside `allowedCIDRs`: the cluster's pod and service ranges (k3s defaults; adjust for your CNI), so sessions cannot reach each other, the hub, Postgres or the API service |
| runner.nodeSelector | object | `{}` | Node selector applied to every session pod |
| runner.resources | object | `{"limits":{"cpu":"2","memory":"4Gi"},"requests":{"cpu":"250m","memory":"512Mi"}}` | Resources a session gets when it asks for nothing (requests and limits); an extended resource here (e.g. `nvidia.com/gpu: 1`) goes to every session |
| runner.runtimeClassName | string | `""` | runtimeClassName for session pods (gVisor, Kata); empty = default runtime |
| runner.sandbox.chart.name | string | `"vcluster"` | vcluster chart name or `oci://` reference |
| runner.sandbox.chart.repo | string | `"https://charts.loft.sh"` | Helm repository of the vcluster chart (ignored when `name` is an `oci://` reference) |
| runner.sandbox.chart.version | string | `"0.37.3"` | vcluster chart version |
| runner.sandbox.domain | string | `""` | DNS domain for sandbox Ingresses: a session's Ingress hosts must be `<session name>.<domain>` or below it (admission policy), and are served by the host's ingress controller. Empty: Ingresses are not synced to the host |
| runner.sandbox.enabled | bool | `false` | Offer the "sandbox" access mode: such a session gets a virtual cluster (vcluster) of its own in a namespace `<namespacePrefix><session id>`, and only that cluster's kubeconfig. The hub may then create, label and delete namespaces carrying the sandbox label (enforced by a ValidatingAdmissionPolicy), create RoleBindings there and bind the two sandbox roles below. Needs Kubernetes 1.30+ for the admission policies |
| runner.sandbox.ingressFrom | list | `[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"kube-system"}},"podSelector":{"matchLabels":{"app.kubernetes.io/name":"traefik"}}}]` | NetworkPolicy peers allowed to reach the sandbox's workloads (vcluster's own policies admit only the vcluster): the ingress controller. Default: Traefik in kube-system, as on k3s |
| runner.sandbox.installerImage | string | `"alpine/helm:3.22.0"` | Image with the Helm CLI that installs vcluster (runs as a one-off Job in the sandbox namespace) |
| runner.sandbox.namespacePrefix | string | `"sandbox-"` | Prefix of sandbox namespace names (at most 16 characters) |
| runner.sandbox.podSecurity | string | `"baseline"` | Pod Security Standard enforced in sandbox namespaces, where the sandbox's workloads run: `baseline` (no privileged pods, host paths or host network) or `restricted` (vcluster's control plane runs as root by default, so `restricted` needs `values` that change its security context) |
| runner.sandbox.values | object | `{}` | Extra vcluster chart values merged over the hub's (resource quota, scheduling, storage). The hub always sets: no ClusterRole, the in-cluster kubeconfig server, no LoadBalancer or NodePort Services |
| runner.serviceAccount.clusterRole | string | `"view"` | ClusterRole it is bound to; the built-in `view` reads most things and never Secrets |
| runner.serviceAccount.clusterWide | bool | `false` | Bind it across the cluster (ClusterRoleBinding) rather than only in the release namespace (RoleBinding) |
| runner.serviceAccount.enabled | bool | `false` | Create a read-only ServiceAccount that sessions can opt into, so kubectl works inside the pod (the hub's own identity is the top-level `serviceAccount`) |
| runner.serviceAccount.name | string | `""` | Name of that ServiceAccount; defaults to `<fullname>-runner` |
| runner.serviceAccount.namespaceWrite | bool | `false` | Offer the "namespace" access mode: such a session gets a ServiceAccount of its own, bound to `clusterRole` like the shared one plus `writeClusterRole` in the namespaces its owner lists. The hub is then allowed to create ServiceAccounts here and RoleBindings in any namespace (and ClusterRoleBindings with `clusterWide`), and to bind those two roles |
| runner.serviceAccount.writeClusterRole | string | `"edit"` | ClusterRole granted in a session's listed namespaces; the built-in `edit` changes workloads and reads Secrets there but never touches RBAC |
| runner.storageClass | string | `"nfs-fast"` | StorageClass for session PVCs (empty = cluster default) |
| runner.tmpInit | bool | `true` | Run a root init container (`chmod 1777 /tmp`) so the /tmp emptyDir gets the sticky bit Claude Code demands for its sockets; disable under the "restricted" Pod Security Standard (the CLI then only warns) |
| runner.tolerations | list | `[]` | Tolerations applied to every session pod |
| service.port | int | `80` | Service port |
| service.type | string | `"ClusterIP"` | Service type |
| serviceAccount.annotations | object | `{}` | ServiceAccount annotations |
| serviceAccount.create | bool | `true` | Create the ServiceAccount |
| serviceAccount.name | string | `""` | ServiceAccount name; defaults to the fullname |
| tolerations | list | `[]` | Hub tolerations |

## Backup

```sh
kubectl -n agents-operator exec agents-operator-postgresql-0 -- env PGPASSWORD="$(kubectl -n agents-operator get secret agents-operator-postgresql -o jsonpath='{.data.password}' | base64 -d)" pg_dump -U agents_operator agents_operator > agents-operator.sql
```

