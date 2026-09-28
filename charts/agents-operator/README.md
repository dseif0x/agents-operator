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

Every session pod runs non-root (UID 1000), with `readOnlyRootFilesystem`, all capabilities dropped, `seccompProfile: RuntimeDefault`, no service account token, no host namespaces and no host paths. None of this is configurable. `runner.networkPolicy` adds a default-deny policy: DNS, 443 and 22 outward (minus `blockedCIDRs`), plus `egressCIDRs`; inbound only from the hub on 7681.

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
| runner.networkPolicy.blockedCIDRs | list | `["10.42.0.0/16","10.43.0.0/16"]` | CIDRs excluded from the "443/22 anywhere" rules: cluster pod and service ranges (k3s defaults) |
| runner.networkPolicy.egressCIDRs | list | `[]` | Extra allowed egress CIDRs (all ports), e.g. an in-cluster LLM proxy |
| runner.networkPolicy.enabled | bool | `true` | Create a default-deny NetworkPolicy for session pods |
| runner.networkPolicy.extraEgress | list | `[]` | Extra peers allowed to egress to on any port (raw NetworkPolicy "to" entries) |
| runner.nodeSelector | object | `{}` | Node selector applied to every session pod |
| runner.resources | object | `{"limits":{"cpu":"2","memory":"4Gi"},"requests":{"cpu":"250m","memory":"512Mi"}}` | Default (and maximum) resources for session pods; a session can only lower them |
| runner.runtimeClassName | string | `""` | runtimeClassName for session pods (gVisor, Kata); empty = default runtime |
| runner.storageClass | string | `"nfs-fast"` | StorageClass for session PVCs (empty = cluster default) |
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

