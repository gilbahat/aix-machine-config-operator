#!/usr/bin/env bash
#
# Writes a kubeconfig for the amco-agent service account. The agent runs on
# AIX nodes as an SRC subsystem, not as a pod, so it has no projected service
# account token; it uses the long-lived token Secret from
# manifests/03-agent-rbac.yaml instead. Run with cluster-admin `oc`.
#
#   hack/agent-kubeconfig.sh > kubeconfig
#
# Delete the Secret amco-agent-token to revoke every agent's access.
set -euo pipefail

ns=${NAMESPACE:-aix-machine-config-operator}
sa=amco-agent
secret=amco-agent-token

for _ in $(seq 30); do
	token=$(oc get secret -n "$ns" "$secret" -o jsonpath='{.data.token}' 2>/dev/null | base64 -d)
	[ -n "$token" ] && break
	sleep 1
done
[ -n "${token:-}" ] || { echo "token was not populated for secret $ns/$secret (apply manifests/ first)" >&2; exit 1; }

ca=$(oc get secret -n "$ns" "$secret" -o jsonpath='{.data.ca\.crt}')
server=$(oc whoami --show-server)
# Prefer the internal API endpoint that nodes use when it is published.
internal=$(oc get infrastructure cluster -o jsonpath='{.status.apiServerInternalURI}' 2>/dev/null || true)
[ -n "$internal" ] && server=$internal

cat <<KUBECONFIG
apiVersion: v1
kind: Config
clusters:
- name: cluster
  cluster:
    server: ${server}
    certificate-authority-data: ${ca}
users:
- name: ${sa}
  user:
    token: ${token}
contexts:
- name: ${sa}
  context:
    cluster: cluster
    user: ${sa}
current-context: ${sa}
KUBECONFIG
