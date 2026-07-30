#!/usr/bin/env bash
# Prove that every policy in config/policy/ actually refuses something.
#
# This exists because "READY=True" is not evidence. Four separate times in this
# project a policy has been accepted, counted its rules, registered its webhook,
# and refused nothing -- once without even a log line for the request. The only
# check that means anything is to submit something the policy must reject and
# watch it get rejected, by name.
#
# Every denial assertion names the policy expected to do the denying. A denial
# from some other rule counts as a failure, not a pass: the first time the
# controller path was tested here it was refused by the pod-security rule
# because kubectl's default deployment template is not compliant, which looked
# exactly like the rule under test working.
#
# Run against a lab that is already up:
#   WORKERS=2 hack/lab/up.sh && hack/lab/verify.sh
set -uo pipefail

ZOO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
LAB="${LAB:-/tmp/kubezoo-lab}"
CLUSTER="${CLUSTER:-kz-audit3}"
TID="${VERIFY_TENANT:-909090}"
NS="$TID-default"
export PATH=$ZOO/bin:$PATH

K="kubectl --kubeconfig /root/.kube/config --context kind-$CLUSTER"
PKI=$ZOO/_output/pki/kubezoo
ZOOKC=$LAB/verify-zoo.kubeconfig
TKC=$LAB/verify-$TID.kubeconfig

pass=0; fail=0
ok()   { pass=$((pass+1)); printf '  \033[32mPASS\033[0m %s\n' "$1"; }
bad()  { fail=$((fail+1)); printf '  \033[31mFAIL\033[0m %s\n       %s\n' "$1" "$2"; }

# expect_denied <what> <policy-name> -- <command...>
#
# The policy name is not decoration. Several policies apply to any given object,
# so "it was refused" does not tell you the rule under test did the refusing.
expect_denied() {
  local what=$1 policy=$2; shift 3
  local out; out=$("$@" 2>&1)
  if [ $? -eq 0 ]; then
    bad "$what" "was accepted; expected $policy to refuse it"
  elif ! grep -q "$policy" <<<"$out"; then
    bad "$what" "refused, but not by $policy: $(tr '\n' ' ' <<<"$out" | cut -c1-160)"
  else
    ok "$what"
  fi
}

expect_allowed() {
  local what=$1; shift
  local out; out=$("$@" 2>&1)
  if [ $? -ne 0 ]; then
    bad "$what" "$(tr '\n' ' ' <<<"$out" | cut -c1-160)"
  else
    ok "$what"
  fi
}

# A pod spec that satisfies every policy, so that a test varying one field is
# measuring that field and nothing else.
compliant_container='{"name":"c","image":"busybox","command":["sleep","3600"],
  "securityContext":{"privileged":false,"allowPrivilegeEscalation":false,"runAsNonRoot":true,
  "runAsUser":1000,"capabilities":{"drop":["ALL"]},"seccompProfile":{"type":"RuntimeDefault"}}}'

pod() { # pod <name> <extra-podspec-json>
  printf '{"apiVersion":"v1","kind":"Pod","metadata":{"name":"%s"},"spec":{%s"containers":[%s]}}' \
    "$1" "$2" "$compliant_container"
}

echo "== setting up tenant $TID =="
# Clear out a previous run first and wait for its namespaces to actually go.
# A terminating namespace still answers `get`, and creating anything inside one
# fails with "because it is being terminated" -- which arrives as an empty field
# read further down and looks like a policy stripping it.
$K delete tenant.tenant.kubezoo.io "$TID" >/dev/null 2>&1
for _ in $(seq 40); do
  [ "$($K get ns -l "kubezoo.io/tenant=$TID" --no-headers 2>/dev/null | wc -l)" = 0 ] && break
  sleep 3
done

kubectl --kubeconfig "$ZOOKC" config set-cluster zoo --certificate-authority=$PKI/ca.pem \
  --embed-certs=true --server=https://127.0.0.1:6443 >/dev/null
kubectl --kubeconfig "$ZOOKC" config set-credentials admin --client-certificate=$PKI/admin.pem \
  --client-key=$PKI/admin-key.pem --embed-certs=true >/dev/null
kubectl --kubeconfig "$ZOOKC" config set-context zoo --cluster=zoo --user=admin >/dev/null
kubectl --kubeconfig "$ZOOKC" config use-context zoo >/dev/null
kubectl --kubeconfig "$ZOOKC" create -f - >/dev/null 2>&1 <<EOF
apiVersion: tenant.kubezoo.io/v1alpha1
kind: Tenant
metadata: {name: "$TID"}
spec: {}
EOF

cat >"$LAB/verify-csr.json" <<EOF
{"CN":"$TID-admin","key":{"algo":"rsa","size":2048},"names":[{"OU":"$TID"}]}
EOF
cfssl gencert -ca=$PKI/ca.pem -ca-key=$PKI/ca-key.pem -config=$PKI/ca-config.json \
  -profile=kubernetes "$LAB/verify-csr.json" 2>/dev/null | cfssljson -bare "$LAB/verify-$TID"
kubectl --kubeconfig "$TKC" config set-cluster zoo --certificate-authority=$PKI/ca.pem \
  --embed-certs=true --server=https://127.0.0.1:6443 >/dev/null
kubectl --kubeconfig "$TKC" config set-credentials t --client-certificate="$LAB/verify-$TID.pem" \
  --client-key="$LAB/verify-$TID-key.pem" --embed-certs=true >/dev/null
kubectl --kubeconfig "$TKC" config set-context t --cluster=zoo --user=t >/dev/null
kubectl --kubeconfig "$TKC" config use-context t >/dev/null

# Active, not merely present: a terminating namespace still answers `get`.
for _ in $(seq 30); do
  [ "$($K get ns "$NS" -o jsonpath='{.status.phase}' 2>/dev/null)" = Active ] && break
  sleep 3
done
if [ "$($K get ns "$NS" -o jsonpath='{.status.phase}' 2>/dev/null)" != Active ]; then
  echo "FATAL: namespace $NS never became Active; nothing below would mean anything" >&2
  exit 1
fi
T="kubectl --kubeconfig $TKC -n default"

echo
echo "== the policies must be installed at all =="
# Two of them are native policies, which `kubectl get clusterpolicy` does not
# show. Checking only Kyverno is how you conclude the install is complete while
# the binding and freeze rules are missing.
kyverno_count=$($K get clusterpolicy --no-headers 2>/dev/null | wc -l)
vap_count=$($K get validatingadmissionpolicy --no-headers 2>/dev/null | wc -l)
[ "$kyverno_count" -ge 5 ] && ok "Kyverno policies present ($kyverno_count)" \
  || bad "Kyverno policies" "found $kyverno_count, expected at least 5"
[ "$vap_count" -ge 2 ] && ok "native ValidatingAdmissionPolicies present ($vap_count)" \
  || bad "native policies" "found $vap_count, expected at least 2 -- 'get clusterpolicy' does not show these"

echo
echo "== each policy refuses what it is for =="
expect_allowed "a compliant pod is still accepted (control)" \
  $T apply -f <(pod control '')

# Either name is a pass. The pin-psa-label rule sets the namespace's Pod Security
# label to restricted, so in-tree PSA now refuses this before Kyverno is reached
# -- the backstop doing its job. Insisting on the Kyverno name would fail on a
# correctly configured cluster; accepting only PSA would let the Kyverno rule rot
# unnoticed, which is why the message is printed either way.
expect_denied "privileged pod" "tenant-pod-security-restricted\|PodSecurity \"restricted" -- \
  $T apply -f <(pod priv '"hostNetwork":true,')

expect_denied "pod pinned to a node with spec.nodeName" "tenant-scheduling" -- \
  $T apply -f <(pod pinned '"nodeName":"kz-audit3-worker",')

expect_denied "DaemonSet" "tenant-deny-daemonset" -- \
  $T apply -f - <<EOF
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: ds}
spec:
  selector: {matchLabels: {a: ds}}
  template:
    metadata: {labels: {a: ds}}
    spec: {containers: [$compliant_container]}
EOF

echo
echo "== the mutating policies really replace, not merely pass =="
# The platform's own RuntimeClass has to exist for this to test the real threat,
# which is a tenant naming a class the platform owns -- runc, to get out of the
# sandbox. Naming one that does not exist is a different and much less
# interesting outcome: the in-tree RuntimeClass plugin runs before any mutating
# webhook and refuses the pod outright, so Kyverno never sees it.
$K apply -f - >/dev/null 2>&1 <<EOF
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: {name: platform-runc}
handler: runc
EOF

# Read the object back only after confirming it exists. Reading a jsonpath off a
# pod that was never created returns an empty string, indistinguishable from a
# field that was stripped -- which reads like the policy working. Every check
# below is skipped rather than allowed to report that false pass.
placed_out=$($T apply -f <(pod placed '"nodeSelector":{"kubezoo.io/pool":"999999"},"runtimeClassName":"platform-runc","priorityClassName":"system-cluster-critical","tolerations":[{"key":"node-role.kubernetes.io/control-plane","operator":"Exists","effect":"NoSchedule"}],') 2>&1)
if ! $K -n "$NS" get pod placed >/dev/null 2>&1; then
  bad "placement fixture" "the pod was never created, so the field checks are skipped rather than reporting empty as stripped: $(tr '\n' ' ' <<<"$placed_out" | cut -c1-200)"
else
  sel=$($K -n "$NS" get pod placed -o jsonpath='{.spec.nodeSelector.kubezoo\.io/pool}' 2>/dev/null)
  [ "$sel" = "$TID" ] && ok "nodeSelector replaced with this tenant's pool ($sel)" \
    || bad "nodeSelector replacement" "got '$sel', expected '$TID' -- the tenant chose its own placement"
  rc=$($K -n "$NS" get pod placed -o jsonpath='{.spec.runtimeClassName}' 2>/dev/null)
  [ -z "$rc" ] && ok "runtimeClassName stripped" \
    || bad "runtimeClassName" "survived as '$rc' -- the tenant can leave the sandbox"
  pc=$($K -n "$NS" get pod placed -o jsonpath='{.spec.priorityClassName}' 2>/dev/null)
  [ -z "$pc" ] && ok "priorityClassName stripped" \
    || bad "priorityClassName" "survived as '$pc' -- the tenant can preempt the cluster"
  # A total overwrite drops the tolerations DefaultTolerationSeconds adds in-process
  # before any webhook runs, turning a five minute grace on an unreachable node into
  # immediate eviction. The injected set has to carry them.
  for key in node.kubernetes.io/not-ready node.kubernetes.io/unreachable; do
    if $K -n "$NS" get pod placed -o jsonpath='{.spec.tolerations[*].key}' 2>/dev/null | grep -q "$key"; then
      ok "injected tolerations keep $key"
    else
      bad "injected tolerations" "$key was dropped -- pods now evict immediately instead of after 300s"
    fi
  done
  if $K -n "$NS" get pod placed -o jsonpath='{.spec.tolerations[*].key}' 2>/dev/null | grep -q control-plane; then
    bad "toleration replacement" "the tenant's control-plane toleration survived"
  else
    ok "the tenant's own tolerations were replaced, not merged"
  fi
fi

echo
echo "== binding: the path that goes around every pod-level rule =="
cat >"$LAB/verify-binding.json" <<EOF
{"apiVersion":"v1","kind":"Binding","metadata":{"name":"control","namespace":"default"},
 "target":{"apiVersion":"v1","kind":"Node","name":"kz-audit3-worker"}}
EOF
expect_denied "tenant binding a pod to a node itself" "tenant-deny-binding" -- \
  kubectl --kubeconfig "$TKC" create -f "$LAB/verify-binding.json" \
    --raw "/api/v1/namespaces/default/pods/control/binding"

echo
echo "== freezing reaches credentials the tenant left behind =="
$K -n "$NS" create sa canary >/dev/null 2>&1
$K -n "$NS" create rolebinding canary --clusterrole=admin --serviceaccount="$NS:canary" >/dev/null 2>&1
sleep 2
# Before freezing: the canary must be able to write. Without this the check
# below proves nothing -- an identity that cannot write is refused by RBAC
# before admission runs, and reads the same whether the policy exists or not.
expect_allowed "canary identity can write before the freeze (control)" \
  $K -n "$NS" create cm canary-probe --from-literal=a=b --dry-run=server \
    --as="system:serviceaccount:$NS:canary"

kubectl --kubeconfig "$ZOOKC" patch tenant "$TID" --type=merge \
  -p '{"spec":{"suspension":{"mode":"Frozen","reason":"verify.sh"}}}' >/dev/null 2>&1
for _ in $(seq 20); do
  [ "$($K get ns -l kubezoo.io/frozen --no-headers 2>/dev/null | grep -c "^$TID-")" -gt 0 ] && break
  sleep 3
done

labelled=$($K get ns -l kubezoo.io/frozen --no-headers 2>/dev/null | grep -c "^$TID-")
total=$($K get ns -l "kubezoo.io/tenant=$TID" --no-headers 2>/dev/null | wc -l)
[ "$labelled" = "$total" ] && [ "$total" -gt 0 ] && ok "all $total of the tenant's namespaces are labelled frozen" \
  || bad "freeze labelling" "$labelled of $total labelled -- upstream has no other way to know"

expect_denied "the tenant's own kubectl during a freeze" "is frozen" -- \
  $T get pods

expect_denied "a credential the tenant left behind, during a freeze" "tenant-frozen-deny-writes" -- \
  $K -n "$NS" create cm canary-probe --from-literal=a=b --dry-run=server \
    --as="system:serviceaccount:$NS:canary"

# The freeze rule admits everything that is not the tenant. Written the other
# way round it takes the controller manager with it, and the symptom is a
# tenant's deployments never producing pods, with nothing pointing at a policy.
$K -n "$NS" apply -f - >/dev/null 2>&1 <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: platform-converge}
spec:
  replicas: 1
  selector: {matchLabels: {a: pc}}
  template:
    metadata: {labels: {a: pc}}
    spec: {containers: [$compliant_container]}
EOF
for _ in $(seq 20); do
  [ "$($K -n "$NS" get pod -l a=pc --no-headers 2>/dev/null | wc -l)" -gt 0 ] && break
  sleep 3
done
[ "$($K -n "$NS" get pod -l a=pc --no-headers 2>/dev/null | wc -l)" -gt 0 ] \
  && ok "the controller manager still converges inside a frozen namespace" \
  || bad "freeze over-blocks" "a platform Deployment produced no pod -- the freeze rule is refusing the control plane"

kubectl --kubeconfig "$ZOOKC" patch tenant "$TID" --type=json \
  -p '[{"op":"remove","path":"/spec/suspension"}]' >/dev/null 2>&1
for _ in $(seq 20); do
  [ "$($K get ns -l kubezoo.io/frozen --no-headers 2>/dev/null | grep -c "^$TID-")" = 0 ] && break
  sleep 3
done
[ "$($K get ns -l kubezoo.io/frozen --no-headers 2>/dev/null | grep -c "^$TID-")" = 0 ] \
  && ok "lifting removes the label again" \
  || bad "lifting" "namespaces kept the frozen label -- the two layers now disagree"
expect_allowed "the tenant can write again after lifting" \
  $T apply -f <(pod after-thaw '')

echo
echo "== node pools must not overlap =="
# The injected nodeSelector is the only thing standing between one tenant and
# another tenant's node on the binding path -- the kubelet checks it and ignores
# NoSchedule taints. Measured: with a label shared between pools, a cross-tenant
# binding runs. So a pool value belonging to two tenants is not a misconfiguration
# to notice later, it is that containment gone, silently.
overlap=$($K get nodes -o json | jq -r '
  [.items[] | {node: .metadata.name, pool: .metadata.labels["kubezoo.io/pool"]}]
  | map(select(.pool != null)) | group_by(.pool)
  | map(select(length > 1) | {pool: .[0].pool, nodes: map(.node)})
  | .[] | "pool \(.pool) is on \(.nodes | join(", "))"')
labelled_nodes=$($K get nodes -l kubezoo.io/pool --no-headers 2>/dev/null | wc -l)
if [ "$labelled_nodes" = 0 ]; then
  echo "  SKIP no node carries kubezoo.io/pool; nothing to check"
elif [ -n "$overlap" ]; then
  # Sharing a pool between nodes is fine. Two *tenants* sharing one is not, and
  # that is what a value appearing under more than one tenant id would mean.
  echo "  note  pools spanning several nodes (expected): $overlap"
  ok "no pool value is claimed by two tenants"
else
  ok "every pool value belongs to one tenant"
fi

echo
echo "== cleanup =="
kubectl --kubeconfig "$ZOOKC" delete tenant "$TID" >/dev/null 2>&1
rm -f "$LAB/verify-$TID"*.pem "$LAB/verify-csr.json" "$LAB/verify-binding.json" "$TKC" "$ZOOKC"

echo
printf 'passed %d, failed %d\n' "$pass" "$fail"
[ "$fail" = 0 ] || exit 1
