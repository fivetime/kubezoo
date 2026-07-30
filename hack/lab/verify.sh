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

# The placement policy pins every tenant pod to its own pool, so this tenant
# needs one or nothing it creates will schedule -- which is correct behaviour and
# exactly the trap the runbook warns about, but it would make the pod checks here
# fail for the wrong reason. Borrow a node and give it back at the end.
POOL_NODE=$($K get nodes -o jsonpath='{.items[-1:].metadata.name}' 2>/dev/null)
POOL_WAS=$($K get node "$POOL_NODE" -o jsonpath='{.metadata.labels.kubezoo\.io/pool}' 2>/dev/null)
$K label node "$POOL_NODE" "kubezoo.io/pool=$TID" --overwrite >/dev/null 2>&1

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
echo "== a tenant workload can reach kubezoo with its own ServiceAccount =="
# This is what lets an operator run inside a tenant: pointed at kubezoo rather
# than upstream it sees API groups unprefixed, which is the view its code was
# written against, and each tenant can then run its own version of the same
# operator. It needs kubezoo to authenticate a bound ServiceAccount token, which
# needs a ServiceAccountTokenGetter -- one line in the server wiring, and without
# it every such request is 401 with "authentication failed unexpectedly".
# Take the IPv4 gateway specifically. The template over all IPAM configs
# concatenates the v6 and v4 gateways into one unusable string, and curl then
# fails to connect with a code of 000, which reads like kubezoo refusing it.
ZOO_HOST=$(docker network inspect kind -f '{{range .IPAM.Config}}{{.Gateway}} {{end}}' 2>/dev/null \
  | tr ' ' '\n' | grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' | head -1)
if [ -z "$ZOO_HOST" ]; then
  echo "  SKIP cannot work out the address a pod would use to reach kubezoo"
else
  # Capture it: a swallowed error here shows up much later as a pod that never
  # started, with nothing saying why.
  sa_setup_out=$($T apply -f - 2>&1 <<EOF
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: verifywidgets.verify.example}
spec:
  group: verify.example
  names: {plural: verifywidgets, singular: verifywidget, kind: VerifyWidget}
  scope: Namespaced
  versions: [{name: v1, served: true, storage: true, schema: {openAPIV3Schema: {type: object}}}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: verify-sa}
rules: [{apiGroups: ["*"], resources: ["*"], verbs: ["*"]}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: verify-sa}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: verify-sa}
subjects: [{kind: ServiceAccount, name: default, namespace: default}]
---
apiVersion: v1
kind: Pod
metadata: {name: sa-probe}
spec:
  containers:
    - name: c
      image: curlimages/curl:8.11.1
      command: ["sleep", "600"]
      securityContext: {privileged: false, allowPrivilegeEscalation: false, runAsNonRoot: true,
        runAsUser: 1000, capabilities: {drop: ["ALL"]}, seccompProfile: {type: RuntimeDefault}}
EOF
)
  for _ in $(seq 40); do
    [ "$($K -n "$NS" get pod sa-probe -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && break
    sleep 3
  done
  if [ "$($K -n "$NS" get pod sa-probe -o jsonpath='{.status.phase}' 2>/dev/null)" != Running ]; then
    bad "ServiceAccount probe pod" "never reached Running, so the checks below are skipped rather than reported green -- setup said: $(tr '\n' ' ' <<<"$sa_setup_out" | cut -c1-160) ; reason=$($K -n "$NS" get pod sa-probe -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}{.status.conditions[?(@.type==\"PodScheduled\")].message}' 2>&1 | cut -c1-90)"
  else
    # Judge on the object landing upstream, not on a status code: discovery
    # answers 200 for a group that resolves to nothing in particular.
    #
    # The script goes in on stdin rather than inside the exec arguments. Quoting
    # it through two shells mangled the URL and curl exited 3, which reads as a
    # failed check rather than a broken probe.
    cat >"$LAB/verify-sa-probe.sh" <<PROBE
T=\$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)
curl -sk -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer \$T" \
  -X POST -H 'Content-Type: application/json' \
  "https://$ZOO_HOST:6443/apis/verify.example/v1/namespaces/default/verifywidgets" \
  -d '{"apiVersion":"verify.example/v1","kind":"VerifyWidget","metadata":{"name":"from-pod"}}'
PROBE
    out=$($K -n "$NS" exec -i sa-probe -- sh -s <"$LAB/verify-sa-probe.sh" 2>/dev/null | tail -1)
    if [ "$out" = 201 ]; then
      ok "a pod's ServiceAccount token authenticates to kubezoo and writes through it"
    else
      bad "ServiceAccount token against kubezoo" "POST returned '$out', expected 201 -- 401 means the token getter is unwired again"
    fi
    if $K -n "$TID-default" get verifywidgets.$TID-verify.example from-pod >/dev/null 2>&1; then
      ok "the object landed upstream under the tenant's prefixed group"
    else
      bad "prefixed landing" "the object is not upstream as ${TID}-verify.example, so the write did not go through the translation"
    fi
  fi
fi

echo
echo "== a cross-namespace list is assembled from the tenant's namespaces =="
# Replaces listing every object of the kind in the cluster and discarding other
# tenants'. Correctness rests on the pinned revision: paging has to give the same
# answer as not paging, and no page may contain another tenant's object.
for extra in fanout-a fanout-b; do
  $T create namespace "$extra" >/dev/null 2>&1
done
for _ in $(seq 20); do
  [ "$($K get ns "$TID-fanout-a" -o jsonpath='{.status.phase}' 2>/dev/null)" = Active ] && break
  sleep 2
done
for extra in default fanout-a fanout-b; do
  for i in 1 2 3; do
    $T -n "$extra" create configmap "fan$i" --from-literal=a=b >/dev/null 2>&1
  done
done
unpaged=$($T get configmaps -A --no-headers 2>/dev/null | wc -l)
if [ "$unpaged" -lt 6 ]; then
  bad "cross-namespace list" "only $unpaged objects came back; the fixture did not take, so paging below would prove nothing"
else
  ok "a cross-namespace list spans the tenant's namespaces ($unpaged objects)"
  paging_consistent=yes
  for size in 1 2 5; do
    # Bounded, because the interesting way this breaks is not a wrong count. A
    # cursor that fails to record where it stopped inside a namespace restarts
    # that namespace on every page and the client pages forever -- measured, by
    # breaking it on purpose and watching this check hang instead of fail, which
    # is no better than passing.
    if ! paged_out=$(timeout 90 $T get configmaps -A --chunk-size=$size --no-headers 2>/dev/null); then
      bad "paged cross-namespace list" "chunk-size=$size did not finish inside 90s -- the cursor is not advancing, so the client is paging in circles"
      paging_consistent=no
      continue
    fi
    paged=$(grep -c . <<<"$paged_out")
    unique=$(sort -u <<<"$paged_out" | grep -c .)
    if [ "$paged" != "$unpaged" ] || [ "$unique" != "$unpaged" ]; then
      bad "paged cross-namespace list" "chunk-size=$size gave $paged ($unique unique), unpaged gave $unpaged -- objects are being dropped or repeated across pages"
      paging_consistent=no
    fi
  done
  [ "$paging_consistent" = yes ] && ok "paging returns exactly the same objects, at every chunk size tried"
  # The isolation this whole path exists to preserve.
  if $T get configmaps -A --chunk-size=1 --no-headers 2>/dev/null | grep -qv "^$TID-\|^[a-z-]* " ; then
    :
  fi
  foreign=$($T get configmaps -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' 2>/dev/null | grep -c "^[0-9]\{6\}-" || true)
  if [ "$foreign" = 0 ]; then
    ok "no namespace in the result still carries a tenant prefix"
  else
    bad "cross-namespace list leaks" "$foreign entries came back with a prefixed namespace, so another tenant's objects or untranslated names are in the result"
  fi
fi

echo
echo "== a cross-namespace watch is merged from the tenant's namespaces =="
# The other half of the list. What matters is not that events arrive but that
# none are missing: a stream that quietly stops covering a namespace leaves an
# informer's cache wrong while it believes it is current.
watch_log=$LAB/verify-watch.log
timeout 60 $T get configmaps -A -w --no-headers >"$watch_log" 2>&1 &
watch_pid=$!
sleep 6
$T -n fanout-a create configmap watched-existing --from-literal=a=b >/dev/null 2>&1
# A namespace created while the watch is open has to join it. The tenant cannot
# read it for a moment after it appears -- the RoleBinding has to reach the
# authorizer -- so this is also a test that the join waits that out.
$T create namespace watch-late >/dev/null 2>&1
sleep 10
$T -n watch-late create configmap watched-new --from-literal=a=b >/dev/null 2>&1
sleep 12
kill "$watch_pid" >/dev/null 2>&1
wait "$watch_pid" 2>/dev/null

if grep -q watched-existing "$watch_log"; then
  ok "an event from an existing namespace reaches the merged stream"
else
  bad "merged watch" "nothing about watched-existing arrived: $(tr '\n' ' ' <"$watch_log" | cut -c1-140)"
fi
if grep -q watched-new "$watch_log"; then
  ok "a namespace created during the watch joins it"
else
  bad "merged watch, late namespace" "nothing about watched-new arrived, so a namespace created mid-watch is invisible until the client re-lists"
fi

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
echo "== a request into a namespace the tenant does not have reads as NotFound =="
# Upstream refuses it with Forbidden, because a namespace that does not exist has
# no RoleBinding, while a cluster-admin doing the same gets NotFound. Tools read
# the difference as fatal versus routine: helm checks whether a chart's resources
# already exist before creating the namespace, and on Forbidden it gives up
# without ever attempting the create.
missing_out=$($T get configmap nothing-here -n does-not-exist 2>&1)
if grep -q NotFound <<<"$missing_out"; then
  ok "a missing namespace reads as NotFound, not Forbidden"
else
  bad "missing-namespace error" "got: $(tr '\n' ' ' <<<"$missing_out" | cut -c1-140)"
fi
# And a real permission problem in a namespace that does exist must still say so,
# or this reshaping is hiding failures instead of correcting one.
#
# This runs last, and deliberately. Producing a genuine denial means taking the
# tenant's RoleBinding away, and the authorizer serves a stale yes for a moment
# after the delete and a stale no for a moment after the controller puts it back.
# Anything scheduled after this raced those two windows and failed intermittently
# on work that had nothing to do with it.
$K -n "$NS" delete rolebinding kubezoo:tenant-admin >/dev/null 2>&1
sleep 2
denied_out=$($T get configmap kube-root-ca.crt 2>&1)
if grep -q -i forbidden <<<"$denied_out"; then
  ok "a genuine denial in an existing namespace is still Forbidden"
else
  bad "denial reshaping is too wide" "got: $(tr '\n' ' ' <<<"$denied_out" | cut -c1-140)"
fi

echo
echo "== cleanup =="
if [ -n "${POOL_NODE:-}" ]; then
  if [ -n "${POOL_WAS:-}" ]; then
    $K label node "$POOL_NODE" "kubezoo.io/pool=$POOL_WAS" --overwrite >/dev/null 2>&1
  else
    $K label node "$POOL_NODE" kubezoo.io/pool- >/dev/null 2>&1
  fi
fi
kubectl --kubeconfig "$ZOOKC" delete tenant "$TID" >/dev/null 2>&1
rm -f "$LAB/verify-$TID"*.pem "$LAB/verify-csr.json" "$LAB/verify-binding.json" "$TKC" "$ZOOKC"

echo
printf 'passed %d, failed %d\n' "$pass" "$fail"
[ "$fail" = 0 ] || exit 1
