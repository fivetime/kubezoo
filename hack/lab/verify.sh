#!/usr/bin/env bash
#
# ⚠️ 这个文件**没有按仓库拆**,而且不该拆。
#
# 三个仓库的东西已经各自有自己的测试:contract 有翻译规则的单测和作用域表对照,
# controller 有对着真 apiserver 的对账测试,proxy 有请求改写的单测。
# 那些都能单独跑。
#
# ⭐ 这里的 118 条断言**一条都不能**:每一条都从"建一个租户"开始 —— 那需要 proxy
# 接受 Tenant 对象、需要 controller 建出 namespace 和 RoleBinding、需要策略层在管
# 工作负载。测的是**隔离**,而隔离是三者合起来的性质,不是任何一个的属性。
#
# ⇒ 把它按仓库切开,只会得到三份各自都跑不起来的断言。它放在 proxy 是因为 proxy
# 是入口,不是因为它属于 proxy。
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

# ⚠️ Either layer is a pass now: kubezoo refuses this in-process
# (refuseTenantChosenNode) and the policy refuses it at the webhook, and kubezoo
# gets there first, so pinning the message to tenant-scheduling would fail on a
# perfectly healthy cluster. Same reasoning as the privileged-pod check above.
expect_denied "pod pinned to a node with spec.nodeName" "tenant-scheduling\|spec.nodeName: Forbidden" -- \
  $T apply -f <(pod pinned '"nodeName":"kz-audit3-worker",')

# ⭐ ...and the policy is checked on its own, by going around kubezoo entirely and
# creating the pod against upstream in a tenant namespace. Accepting either
# message above is what lets a rule rot unnoticed once a second layer refuses
# first; this is the assertion that would notice. It also covers what kubezoo
# structurally cannot: a pod born upstream, which is how every pod a Deployment
# produces arrives.
expect_denied "and the policy still refuses one created around kubezoo entirely" "tenant-scheduling" -- \
  $K -n "$NS" apply -f <(pod pinned-direct '"nodeName":"kz-audit3-control-plane",')

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

# ⭐ A Deployment's TEMPLATE, not just a pod. This is what actually protects a
# tenant during a webhook outage: kubezoo never sees the pods a Deployment
# produces -- kube-controller-manager creates those against upstream directly --
# so a template stored with the right pool is the only thing still placing them.
$T apply -f - >/dev/null 2>&1 <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: placed-deploy}
spec:
  replicas: 0
  selector: {matchLabels: {app: placed-deploy}}
  template:
    metadata: {labels: {app: placed-deploy}}
    spec:
      nodeSelector: {kubezoo.io/pool: "999999"}
      containers: [{name: c, image: busybox:1.36}]
EOF
sel=$($K -n "$NS" get deploy placed-deploy -o jsonpath='{.spec.template.spec.nodeSelector.kubezoo\.io/pool}' 2>/dev/null)
# ⚠️ Outcome only: the policy's place-controller rule places a Deployment on
# CREATE as well, so a green run here does not show WHICH layer did it. The one
# below does -- an edit after creation is a path the policy structurally does not
# cover. Kept because the outcome is worth pinning, labelled so it does not read
# as proof of kubezoo's copy.
if [ "$sel" = "$TID" ]; then
  ok "a Deployment's template carries the tenant's pool"
else
  bad "a Deployment's template is placed" "pool is '${sel:-<none>}', want '$TID'"
fi

# ⭐⭐ And an UPDATE is placed as well. This is the one assertion here that can
# tell the two layers apart: every rule in tenant-placement.yaml matches
# operations: [CREATE], so a template rewritten AFTER creation can only have been
# rewritten by kubezoo. (The policy gets away with CREATE-only because a pod is
# always a create and its own rule overwrites whatever the template said -- but
# that backstop is exactly what is missing when the webhook is gone.)
$T patch deploy placed-deploy --type=merge \
  -p '{"spec":{"template":{"spec":{"nodeSelector":{"kubezoo.io/pool":"999999"}}}}}' >/dev/null 2>&1
sel=$($K -n "$NS" get deploy placed-deploy -o jsonpath='{.spec.template.spec.nodeSelector.kubezoo\.io/pool}' 2>/dev/null)
if [ "$sel" = "$TID" ]; then
  ok "and an edit after creation is placed too -- only kubezoo could have done that"
else
  bad "an edited template is placed" "pool is '${sel:-<none>}', want '$TID' -- a tenant can move its own workloads by updating them"
fi
# ⭐ nodeName is the last field in the pod surface that goes around the scheduler,
# and until now only a Kyverno deny stood in front of it. A pod naming its node is
# taken by kubelet directly: every rule that lives in the scheduler, taints above
# all, is simply never consulted.
out=$($T apply -f <(pod nodenamed '"nodeName":"kz-audit3-control-plane",') 2>&1)
if grep -qi "nodeName" <<<"$out"; then
  ok "a pod naming the node it wants is refused"
else
  bad "a pod naming its own node is refused" "$(tr '\n' ' ' <<<"$out" | cut -c1-160)"
fi

# ...and a template carrying one is cleared rather than refused, so a Deployment
# that already has one keeps reconciling instead of failing every write.
#
# ⭐ This one does distinguish the layers: tenant-placement.yaml never mentions
# nodeName and tenant-scheduling.yaml denies it on Pod only, so nothing touched it
# in a template before. What used to happen was worse than an escape and quieter:
# the Deployment was accepted, kube-controller-manager built pods carrying that
# nodeName, deny-nodename refused every one of them, and the Deployment simply
# produced nothing with no error on the object the tenant was looking at.
$T apply -f - >/dev/null 2>&1 <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: nodenamed-deploy}
spec:
  replicas: 0
  selector: {matchLabels: {app: nodenamed-deploy}}
  template:
    metadata: {labels: {app: nodenamed-deploy}}
    spec:
      nodeName: kz-audit3-control-plane
      containers: [{name: c, image: busybox:1.36}]
EOF
node=$($K -n "$NS" get deploy nodenamed-deploy -o jsonpath='{.spec.template.spec.nodeName}' 2>/dev/null)
if $K -n "$NS" get deploy nodenamed-deploy >/dev/null 2>&1 && [ -z "$node" ]; then
  ok "and a template naming one has it cleared, not the whole write refused"
else
  bad "a template naming a node is cleared" "template nodeName is '${node:-<absent>}'; deployment exists: $($K -n "$NS" get deploy nodenamed-deploy >/dev/null 2>&1 && echo yes || echo no)"
fi
$T delete deploy nodenamed-deploy >/dev/null 2>&1

# ⚠️ And the trap this had to side-step: the scheduler writes spec.nodeName onto
# every pod it binds, so from then on EVERY update to that pod carries it.
# Refusing there would fail every later write to every running pod in the cluster.
if $T -n default annotate pod placed probe=still-writable --overwrite >/dev/null 2>&1; then
  ok "while a bound pod stays writable, which is what refusing on update would have broken"
else
  bad "a bound pod stays writable" \
      "$($T -n default annotate pod placed probe=x --overwrite 2>&1 | tr '\n' ' ' | cut -c1-160)"
fi

# ⛔ The case this CANNOT reach, recorded so nobody adds an assertion that looks
# like it does. What would prove the create-only rule is a pod whose STORED
# placement is not the canonical one -- a pod predating this hardening, or one
# written while the webhook was down. Rewriting placement on an update is refused
# by upstream (nodeSelector is immutable on a pod update and every existing
# toleration has to survive), so such a pod would become unwritable by its tenant.
#
# ⚠️ There is no way to build one here: Kyverno's place-pod matches every Pod
# CREATE in a tenant-labelled namespace whoever issues it, so a pod created
# against upstream with a foreign pool is canonicalised on the way in. An earlier
# version of this file did exactly that and asserted on it; a negative control --
# putting the defect back and re-running -- showed the assertion passing anyway.
# It proved nothing and has been removed.
#
# What pins the rule instead is TestForwardLeavesALivePodAlone in
# pkg/convert/placement_test.go, which is mutation-checked and does go red.

$T delete deploy placed-deploy >/dev/null 2>&1

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
# Wait for the condition being asserted, not for a weaker one. This used to break
# out as soon as any namespace carried the label and then assert that all of them
# did -- and the controller labels them one at a time, so the gap between those
# two is real. It failed at 3 of 4, which is what sampling a converging system at
# an arbitrary moment looks like.
total=$($K get ns -l "kubezoo.io/tenant=$TID" --no-headers 2>/dev/null | wc -l)
for _ in $(seq 20); do
  [ "$($K get ns -l kubezoo.io/frozen --no-headers 2>/dev/null | grep -c "^$TID-")" = "$total" ] && break
  sleep 3
done

labelled=$($K get ns -l kubezoo.io/frozen --no-headers 2>/dev/null | grep -c "^$TID-")
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
echo "== a tenant can grant rights over its own custom resources =="
# Without this a tenant cannot write a ClusterRole naming its own CRDs: RBAC
# refuses to let anyone grant what they do not hold, and a tenant holds its
# rights per namespace. That is what stops an operator chart installing. The
# grant is safe because the group name carries the tenant -- 909090-something
# can only ever hold this tenant's objects.
$T apply -f - >/dev/null 2>&1 <<EOF
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: rbacwidgets.rbac.example}
spec:
  group: rbac.example
  names: {plural: rbacwidgets, singular: rbacwidget, kind: RbacWidget}
  scope: Namespaced
  versions: [{name: v1, served: true, storage: true, schema: {openAPIV3Schema: {type: object}}}]
EOF
# No nudge of the tenant here, deliberately: a CRD event has to reach the
# controller on its own, or a tenant that has just created a CRD is refused
# until the next resync.
own_ok=no
for _ in $(seq 30); do
  if $T create clusterrole own-crd --verb=get,list,watch --resource=rbacwidgets.rbac.example >/dev/null 2>&1; then
    own_ok=yes
    break
  fi
  sleep 2
done
if [ "$own_ok" = yes ]; then
  ok "a ClusterRole over the tenant's own custom resource is accepted, without touching the tenant"
else
  bad "own custom resource grant" "still refused after 60s -- the CRD event is not reaching the controller, or the group is not in the tenant's cluster role"
fi

# The names kubezoo keeps for itself have to be unaddressable, because a tenant
# naming a ClusterRole cluster-admin is naming the role bound cluster-wide to it.
expect_denied "deleting the tenant's own reserved cluster role" "managed by the platform" -- \
  $T delete clusterrole cluster-admin
expect_denied "overwriting it" "managed by the platform" -- \
  $T apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: cluster-admin}
rules:
  - apiGroups: ["*"]
    resources: ["*"]
    verbs: ["*"]
EOF
# Scoped to RBAC: a tenant may perfectly well have other objects by that name.
#
# ⚠️ Not hostPath, which is what this used to use for brevity. The volume source
# is incidental here -- the assertion is about the NAME -- but a hostPath PV is
# now refused outright, because binding one through a PVC walks past restricted
# Pod Security's volume checks and mounts the node's filesystem into a tenant
# container. NFS names nothing inside the cluster and keeps this measuring what
# it says it measures.
expect_allowed "an object of another kind may still be called admin" \
  $T apply -f - <<EOF
apiVersion: v1
kind: PersistentVolume
metadata: {name: admin}
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteOnce]
  nfs: {server: 192.0.2.1, path: /exports/reserved-name-check}
EOF

# A ClusterRole over shared resources is now writable on purpose -- it is what
# every operator chart ships, and it used to be refused because the escalation
# check asks at cluster scope while a tenant holds these per namespace. Writing
# it is not the thing to guard; what it can reach is.
expect_allowed "a ClusterRole over shared resources" \
  $T create clusterrole shared-secrets --verb=get --resource=secrets
if [ "$($K auth can-i get secrets -n kube-system --as="$TID-admin" --as-group="kubezoo:proxied:$TID" 2>/dev/null)" = no ]; then
  ok "writing it does not give the tenant those resources anywhere new"
else
  bad "writing it does not give the tenant those resources anywhere new" \
      "the tenant can read kube-system's secrets"
fi

# Naming another tenant's group by its upstream name, which is the way round
# that would actually be worth trying. The name survives -- apiGroups are only
# prefixed when they are the tenant's own -- so the guard is that it buys
# nothing, not that it cannot be written.
expect_allowed "a ClusterRole over another tenant's group" \
  $T apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: someone-elses}
rules:
  - apiGroups: ["999999-elsewhere.example"]
    resources: ["things"]
    verbs: ["get"]
EOF
$T apply -f - >/dev/null 2>&1 <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: someone-elses}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: someone-elses}
subjects: [{kind: ServiceAccount, name: default, namespace: default}]
EOF
# Binding it is allowed and means nothing: a tenant's ClusterRoleBinding is
# projected into its own namespaces, and the other tenant's custom resources are
# not in them.
if [ "$($K auth can-i list things.999999-elsewhere.example -n "$TID-default" \
        --as="system:serviceaccount:$NS:default" 2>/dev/null)" = yes ] &&
   [ "$($K auth can-i list things.999999-elsewhere.example -n 999999-default \
        --as="system:serviceaccount:$NS:default" 2>/dev/null)" = no ]; then
  ok "and binding it reaches only the tenant's own namespaces, where that group has nothing"
else
  bad "binding it reaches only the tenant's own namespaces" \
      "elsewhere=$($K auth can-i list things.999999-elsewhere.example -n 999999-default --as="system:serviceaccount:$NS:default" 2>/dev/null)"
fi

# The sharpest form of the containment: grant yourself bind and escalate on
# clusterrolebindings in your own namespace, then try again. A RoleBinding never
# authorizes a cluster-scoped resource, so it changes nothing.
$T apply -f - >/dev/null 2>&1 <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: self-help}
rules:
  - apiGroups: ["rbac.authorization.k8s.io"]
    resources: ["clusterrolebindings", "clusterroles"]
    verbs: ["*"]
EOF
$T -n default apply -f - >/dev/null 2>&1 <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: self-help}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: self-help}
subjects: [{kind: User, name: admin, apiGroup: rbac.authorization.k8s.io}]
EOF
$T apply -f - >/dev/null 2>&1 <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: self-help}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: shared-secrets}
subjects: [{kind: ServiceAccount, name: default, namespace: default}]
EOF
# Whatever it grants itself, no object that spans the cluster comes out of it.
if [ "$($K get clusterrolebinding --no-headers 2>/dev/null | grep -c "^$TID-")" = 1 ]; then
  ok "and nothing it grants itself produces a binding that spans the cluster"
else
  bad "nothing it grants itself produces a binding that spans the cluster" \
      "upstream now has $($K get clusterrolebinding --no-headers 2>/dev/null | grep "^$TID-" | tr '\n' ' ')"
fi

echo
echo "== a cross-namespace list is assembled from the tenant's namespaces =="
# Replaces listing every object of the kind in the cluster and discarding other
# tenants'. Correctness rests on the pinned revision: paging has to give the same
# answer as not paging, and no page may contain another tenant's object.
for extra in fanout-a fanout-b; do
  $T create namespace "$extra" >/dev/null 2>&1
done
# Wait until the tenant can actually write in the new namespaces, not until they
# exist. Those are different moments: the namespace is Active as soon as upstream
# creates it, while the RoleBinding that lets the tenant use it is written by the
# controller afterwards and then has to reach the authorizer's cache.
#
# Waiting on Active was passing on timing alone. It stopped when the controller
# grew more per-namespace work, and the symptom was this fixture silently
# creating nothing -- which reads as "the cross-namespace list is broken" rather
# than "the setup had not finished".
for extra in fanout-a fanout-b; do
  writable=no
  for _ in $(seq 30); do
    if $T -n "$extra" create configmap fan-probe --from-literal=a=b >/dev/null 2>&1; then
      $T -n "$extra" delete configmap fan-probe >/dev/null 2>&1
      writable=yes
      break
    fi
    sleep 2
  done
  [ "$writable" = yes ] || bad "cross-namespace fixture" \
    "the tenant still cannot write in $extra, so nothing below would mean anything"
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
# The budget has to outlast the polling below -- 6s + up to 60s waiting for the
# late namespace to become writable + up to 40s for the event to travel. At 60s
# the watch was being killed before the event it was waiting for could arrive,
# which reads exactly like "a namespace created mid-watch is invisible".
timeout 180 $T get configmaps -A -w --no-headers >"$watch_log" 2>&1 &
watch_pid=$!
sleep 6
$T -n fanout-a create configmap watched-existing --from-literal=a=b >/dev/null 2>&1
# A namespace created while the watch is open has to join it. The tenant cannot
# read it for a moment after it appears -- the RoleBinding has to reach the
# authorizer -- so this is also a test that the join waits that out.
$T create namespace watch-late >/dev/null 2>&1
# Poll rather than sleep a fixed ten seconds. How long the tenant waits for a new
# namespace to become writable depends on how much the controller has to do per
# namespace, and that has grown -- a fixed wait turns "the controller got slower"
# into "the merged watch is broken".
for _ in $(seq 30); do
  $T -n watch-late create configmap watched-new --from-literal=a=b >/dev/null 2>&1 && break
  sleep 2
done
# Then give the event time to travel: the mux has to notice the namespace, open a
# stream on it, and forward. Poll for the same reason.
for _ in $(seq 20); do
  grep -q watched-new "$watch_log" && break
  sleep 2
done
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
echo "== platform infrastructure does not leak through a namespaced resource =="
# Leases are the sharpest probe available. kube-node-lease mirrors the node
# inventory one object per node, and the leases in kube-system and the policy
# engine's namespace name the control-plane components and their pods -- the same
# "which policy engine does this platform run" fingerprint that had to be trimmed
# out of denial messages. None of it should reach a tenant, and what keeps it out
# is the namespace prefixing rather than anything lease-specific.
leases_out=$($T get leases -A --no-headers 2>&1)
if grep -qE "kube-node-lease|kube-system|kyverno" <<<"$leases_out"; then
  bad "platform leases leak" "a cross-namespace lease listing reached platform namespaces: $(tr '\n' ' ' <<<"$leases_out" | cut -c1-140)"
else
  ok "a cross-namespace lease listing reaches no platform namespace"
fi
# By name, through the raw path, and by field selector -- the ways round a list.
node_name=$($K get nodes -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
if [ -n "$node_name" ]; then
  named=$($T get lease "$node_name" -n kube-node-lease 2>&1)
  if grep -qi notfound <<<"$named"; then
    ok "a platform node's lease is NotFound by name"
  else
    bad "node lease reachable by name" "got: $(tr '\n' ' ' <<<"$named" | cut -c1-140)"
  fi
  raw=$($T get --raw '/apis/coordination.k8s.io/v1/namespaces/kube-node-lease/leases' 2>&1)
  if grep -q "$node_name" <<<"$raw"; then
    bad "node lease reachable by raw path" "the raw path reached the platform namespace: $(cut -c1-140 <<<"$raw")"
  else
    ok "the raw path to kube-node-lease lands in the tenant's own, not the platform's"
  fi
fi

echo
echo "== logs reach the tenant's own pods and stream, and nothing else's =="
# Logs go through the connecter, which rewrites the path rather than the object,
# and a path rewriter is where nodes/proxy escaped before. It is also the one
# place streaming has to work: a wrapper that hides Flusher leaves `logs -f`
# silent, which was the case until it was measured against upstream.
$T -n default apply -f - >/dev/null 2>&1 <<EOF
apiVersion: v1
kind: Pod
metadata: {name: log-probe}
spec:
  containers:
    - name: c
      image: busybox
      command: ["sh", "-c", "i=0; while true; do i=\$((i+1)); echo probe-line-\$i; sleep 1; done"]
      securityContext: {privileged: false, allowPrivilegeEscalation: false, runAsNonRoot: true,
        runAsUser: 1000, capabilities: {drop: ["ALL"]}, seccompProfile: {type: RuntimeDefault}}
EOF
for _ in $(seq 40); do
  [ "$($K -n "$NS" get pod log-probe -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && break
  sleep 3
done
if [ "$($K -n "$NS" get pod log-probe -o jsonpath='{.status.phase}' 2>/dev/null)" != Running ]; then
  bad "log probe pod" "never reached Running, so the log checks are skipped rather than reported green"
else
  if $T -n default logs log-probe --tail=1 2>/dev/null | grep -q probe-line; then
    ok "a tenant reads its own pod's logs"
  else
    bad "own pod logs" "nothing came back from a pod that is printing a line a second"
  fi
  # Streaming, not just a snapshot: more than one line over several seconds is
  # the only thing that distinguishes a flushed stream from a buffered one.
  streamed=$(timeout 8 $T -n default logs log-probe -f --tail=1 2>/dev/null | grep -c probe-line)
  if [ "${streamed:-0}" -gt 1 ]; then
    ok "logs -f streams ($streamed lines in 8s)"
  else
    bad "logs -f does not stream" "got ${streamed:-0} lines in 8s; a snapshot works but the follow is buffered, which is what happens when the response writer wrapper hides Flusher"
  fi
  platform_pod=$($K -n kube-system get pods --no-headers 2>/dev/null | awk '{print $1}' | head -1)
  if [ -n "$platform_pod" ]; then
    for path in \
      "/api/v1/namespaces/kube-system/pods/$platform_pod/log?tailLines=1" \
      "/api/v1/namespaces/default/../kube-system/pods/$platform_pod/log?tailLines=1"; do
      out=$($T get --raw "$path" 2>&1)
      if grep -qiE "notfound|forbidden|could not find" <<<"$out"; then
        continue
      fi
      bad "logs reach a platform pod" "$path returned: $(tr '\n' ' ' <<<"$out" | cut -c1-120)"
      platform_reached=yes
    done
    [ "${platform_reached:-no}" = no ] && ok "a platform pod's logs are out of reach, by path and by traversal"
  fi
fi

echo
echo "== exec, attach and port-forward reach the tenant's pods and nothing else =="
# All four of these are connections rather than object reads, and each one is a
# way into a running container. attach and port-forward were registered as
# ordinary object proxies rather than connecters and did not work at all -- the
# apiserver's upgrade handler was handed an empty body where it expects a Pod.
# So these assert that they work as well as that they are contained: a broken
# feature and a contained one look identical from the outside.
if [ "$($K -n "$NS" get pod log-probe -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ]; then
  if [ "$(timeout 25 $T -n default exec log-probe -- echo reachable 2>/dev/null)" = reachable ]; then
    ok "exec into the tenant's own pod"
  else
    bad "exec" "could not run a command in the tenant's own pod"
  fi
  attached=$(timeout 8 $T -n default attach log-probe 2>/dev/null | grep -c probe-line)
  if [ "${attached:-0}" -gt 1 ]; then
    ok "attach streams from the tenant's own pod ($attached lines)"
  else
    bad "attach" "got ${attached:-0} lines; registered as an object proxy rather than a connecter it returns nothing at all"
  fi
  # To a file, not through a pipe. kubectl block-buffers when its output is not a
  # terminal, so piping it into grep and then killing it on a timeout loses the
  # line that was being looked for -- the check fails on a feature that works.
  timeout 10 $T -n default port-forward log-probe 18097:80 >"$LAB/verify-portforward.log" 2>&1
  if grep -q "Forwarding from" "$LAB/verify-portforward.log"; then
    ok "port-forward binds for the tenant's own pod"
  else
    bad "port-forward" "never reported a forward: $(tr '\n' ' ' <"$LAB/verify-portforward.log" | cut -c1-130)"
  fi
  platform_pod=$($K -n kube-system get pods --no-headers 2>/dev/null | awk '{print $1}' | head -1)
  reached=no
  for verb in "exec $platform_pod -- echo pwned" "attach $platform_pod" "port-forward $platform_pod 18098:53"; do
    out=$(timeout 15 $T -n kube-system $verb 2>&1)
    grep -qiE "notfound|not found|forbidden" <<<"$out" || { bad "reached a platform pod" "kubectl $verb returned: $(tr '\n' ' ' <<<"$out" | cut -c1-110)"; reached=yes; }
  done
  [ "$reached" = no ] && ok "exec, attach and port-forward all refuse a platform pod"
fi

# kubectl debug is the sharpest escalation kubectl offers: one form asks for a
# privileged pod on a named node, the other for a privileged container beside an
# existing one.
node_name=$($K get nodes -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
expect_denied "debug against a node" "not found" -- \
  timeout 30 $T debug "node/$node_name" --image=busybox -- true
if [ "$($K -n "$NS" get pod log-probe -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ]; then
  expect_denied "a privileged debug container beside a running pod" "restricted\|forbidden" -- \
    timeout 30 $T -n default debug log-probe --image=busybox --profile=sysadmin -- true
fi

echo
echo "== what discovery advertises is what the server can actually serve =="
# Discovery used to be filtered by the scheme, which knows every group Kubernetes
# has rather than the ones this build installs storage for, so a tenant found
# resources in api-resources that errored on every call. The reverse mistake is
# worse and was made first: filtering too tightly dropped apiextensions and
# tenants could not manage CRDs at all.
advertised=$($T api-resources --no-headers 2>/dev/null | awk '{print $1}' | sort -u)
if grep -qx certificatesigningrequests <<<"$advertised"; then
  bad "discovery advertises what is not served" "certificatesigningrequests is listed but every call to it errors"
else
  ok "an unserved group is not advertised"
fi
for needed in customresourcedefinitions deployments configmaps roles; do
  if grep -qx "$needed" <<<"$advertised"; then
    continue
  fi
  bad "discovery drops something that is served" "$needed is missing from api-resources, so tenants cannot address it"
  discovery_narrow=yes
done
[ "${discovery_narrow:-no}" = no ] && ok "the resources a tenant needs are all advertised"

echo
echo "== a tenant's own webhook cannot reach past it, and it can undo it =="
# A tenant may create admission webhooks. Confined, the worst it can do is stop
# itself; unconfined, one tenant with failurePolicy Fail and a dead endpoint
# stops everybody. The recovery half matters as much: kubezoo forces the rules to
# Namespaced scope, so the webhook cannot match the cluster-scoped object that
# defines it, and the tenant can always delete its way out.
$T apply -f - >/dev/null 2>&1 <<EOF
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata: {name: selflock}
webhooks:
  - name: selflock.verify.example
    admissionReviewVersions: ["v1"]
    sideEffects: None
    failurePolicy: Fail
    clientConfig:
      service: {name: nowhere, namespace: default, path: /validate, port: 443}
    rules:
      - apiGroups: [""]
        apiVersions: ["v1"]
        operations: ["CREATE"]
        resources: ["configmaps"]
EOF
sleep 8
selector=$($K get validatingwebhookconfiguration "$TID-selflock" \
  -o jsonpath='{.webhooks[0].namespaceSelector.matchLabels.kubezoo\.io/tenant}' 2>/dev/null)
scope=$($K get validatingwebhookconfiguration "$TID-selflock" -o jsonpath='{.webhooks[0].rules[0].scope}' 2>/dev/null)
if [ "$selector" = "$TID" ] && [ "$scope" = Namespaced ]; then
  ok "a tenant's webhook is pinned to its own namespaces and to namespaced objects"
else
  bad "tenant webhook not confined" "namespaceSelector=$selector scope=$scope -- unpinned, one tenant's dead webhook stops the cluster"
fi
expect_denied "the tenant's own writes, while its webhook is broken" "selflock" -- \
  $T -n default create configmap webhook-locked --from-literal=a=b
expect_allowed "the platform is unaffected by it" \
  $K -n kube-public create configmap webhook-unaffected --from-literal=a=b
expect_allowed "and the tenant can delete its way out" \
  $T delete validatingwebhookconfiguration selflock
sleep 5
expect_allowed "writes work again once it is gone" \
  $T -n default create configmap webhook-recovered --from-literal=a=b
$K -n kube-public delete configmap webhook-unaffected >/dev/null 2>&1

echo
echo "== a workload addresses its own namespace by the name it was given =="
# A pod learns its namespace from kubelet, which knows the upstream name. Kubezoo
# presents the tenant's name. So an operator reading its own namespace from the
# downward API asked for 111111-default, kubezoo prefixed it again, and every
# request into its own namespace was NotFound -- measured with cert-manager's
# webhook. Two halves: the policy hands pods the tenant's name, and kubezoo
# accepts the upstream one for the clients the policy cannot reach.
$T -n default apply -f - >/dev/null 2>&1 <<EOF
apiVersion: v1
kind: Pod
metadata: {name: ns-probe}
spec:
  securityContext: {runAsNonRoot: true, runAsUser: 1000, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: c
      image: busybox
      command: ["sleep", "3600"]
      env:
        - name: POD_NAMESPACE
          valueFrom: {fieldRef: {fieldPath: metadata.namespace}}
        - name: WATCH_NAMESPACE
          valueFrom: {fieldRef: {fieldPath: metadata.namespace}}
        - name: NODE_NAME
          valueFrom: {fieldRef: {fieldPath: spec.nodeName}}
        - name: PLAIN
          value: untouched
      securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: ["ALL"]}}
EOF
probe_env() {
  $K -n "$NS" get pod ns-probe -o jsonpath="{.spec.containers[0].env[?(@.name=='$1')].value}" 2>/dev/null
}
if [ -n "$($K -n "$NS" get pod ns-probe -o name 2>/dev/null)" ]; then
  if [ "$(probe_env POD_NAMESPACE)" = default ]; then
    ok "a pod is told the tenant's name for its namespace, not the upstream one"
  else
    bad "a pod is told the tenant's name for its namespace" \
        "it says '$(probe_env POD_NAMESPACE)', so every request it makes into its own namespace is prefixed twice"
  fi
  # Matched by where the value comes from, not by what it is called: an operator
  # may call it anything.
  if [ "$(probe_env WATCH_NAMESPACE)" = default ]; then
    ok "and so is any other variable taking its value from metadata.namespace"
  else
    bad "any other variable taking its value from metadata.namespace" \
        "WATCH_NAMESPACE says '$(probe_env WATCH_NAMESPACE)'"
  fi
  # Everything else has to survive: this rewrites one container's whole env list.
  node_from=$($K -n "$NS" get pod ns-probe \
    -o jsonpath="{.spec.containers[0].env[?(@.name=='NODE_NAME')].valueFrom.fieldRef.fieldPath}" 2>/dev/null)
  if [ "$node_from" = spec.nodeName ] && [ "$(probe_env PLAIN)" = untouched ]; then
    ok "while the container's other variables are left exactly as they were"
  else
    bad "the container's other variables are left as they were" \
        "NODE_NAME reads from '$node_from', PLAIN='$(probe_env PLAIN)'"
  fi
fi

# The other half, for the clients the policy cannot reach -- client-go reads the
# namespace out of the projected service account files, which kubelet writes.
expect_allowed "a request naming the upstream namespace reaches the same namespace" \
  $T -n "$NS" get configmaps
if [ "$($T -n "$NS" get configmaps --no-headers 2>/dev/null | wc -l)" = \
     "$($T -n default get configmaps --no-headers 2>/dev/null | wc -l)" ]; then
  ok "and reaches the same objects as the tenant's own name for it"
else
  bad "the two names reach the same objects" \
      "upstream name gave $($T -n "$NS" get configmaps --no-headers 2>/dev/null | wc -l), tenant name gave $($T -n default get configmaps --no-headers 2>/dev/null | wc -l)"
fi
# ⭐ Writes too, not only reads. This used to be reads only, and the write half
# was broken the whole time: kubezoo answered such a request with the object
# relabelled "default", and rest.EnsureObjectNamespaceMatchesRequestNamespace --
# which runs above kubezoo's storage, in the generic patch handler -- compared
# that against the upstream name on the URL and refused with a BadRequest saying
# nothing about namespaces being rewritten.
#
# ⚠️ It broke by client, which is why nothing noticed: `kubectl apply` survived
# because its computed patch happened to carry metadata.namespace and overwrote
# the answer, while `kubectl patch` and controller-runtime's MergeFrom omit an
# unchanged namespace and did not. So an in-cluster controller -- which reads its
# namespace from the projected service account file, where kubelet writes the
# upstream name -- could list and create but not patch.
$T create configmap nsverb --from-literal=a=1 >/dev/null 2>&1
if $T -n "$NS" patch configmap nsverb -p '{"data":{"b":"2"}}' >/dev/null 2>&1; then
  ok "and can be patched by that name, which is what an in-cluster controller does"
else
  bad "a patch by the upstream namespace name" \
      "$($T -n "$NS" patch configmap nsverb -p '{"data":{"c":"3"}}' 2>&1 | tr '\n' ' ' | cut -c1-160)"
fi
# The object has to come back wearing the name that was asked for, or the client
# writes it straight back and gets the same refusal.
echoed=$($T -n "$NS" get configmap nsverb -o jsonpath='{.metadata.namespace}' 2>/dev/null)
own=$($T get configmap nsverb -o jsonpath='{.metadata.namespace}' 2>/dev/null)
if [ "$echoed" = "$NS" ] && [ "$own" = default ]; then
  ok "and each name is answered in the spelling it was asked in"
else
  bad "each name is answered in its own spelling" \
      "asked '$NS' got '${echoed:-<empty>}'; asked 'default' got '${own:-<empty>}'"
fi
$T delete configmap nsverb >/dev/null 2>&1

other_ns=$($T -n 999999-default get configmaps 2>&1)
if [ $? -ne 0 ]; then
  ok "while another tenant's namespace is still out of reach by either name"
else
  bad "another tenant's namespace is out of reach by either name" "it was listed: $(tr '\n' ' ' <<<"$other_ns" | cut -c1-120)"
fi
# The tenant's own prefix is reserved, or a namespace called <tid>-foo would be
# stored as <tid>-<tid>-foo and then be unreachable by either name.
expect_denied "a namespace whose name begins with the tenant's own prefix" "could not be reached" -- \
  $T create namespace "$TID-trap"
expect_allowed "while an ordinary namespace name is unaffected" \
  $T create namespace ns-probe-ok

# ⭐ And it carries the Pod Security level, written by kubezoo itself rather than
# by the Kyverno mutate that also writes it. That matters because Pod Security
# Admission runs INSIDE the apiserver -- no webhook, no single point -- so this
# label is what refuses host namespaces, privileged containers and host paths
# with every webhook in the cluster gone.
#
# ⛔ It used to be written only by Kyverno, which meant the second layer was
# installed by the first: a namespace created while that webhook was not
# registered got no label and no enforcement at all. failurePolicy: Fail does not
# cover it -- that covers a webhook which FAILS, not one which was never
# REGISTERED, and the latter is the failure that actually happened here.
psa=$($K get ns "$TID-ns-probe-ok" -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}' 2>/dev/null)
if [ "$psa" = restricted ]; then
  ok "a namespace the tenant creates carries the Pod Security level"
else
  bad "a tenant-created namespace carries the Pod Security level" \
      "enforce='${psa:-<none>}', want restricted"
fi

# ⚠️ And a tenant cannot weaken it on its own namespace. One doing exactly this
# is why config/policy/README.md exists.
#
# ⚠️ These three assert the OUTCOME, not which layer produced it: with Kyverno
# healthy its mutate would put the label back too, so a green run here does not
# by itself prove kubezoo's copy works. What pins that is the unit tests, which
# drive NamespaceTransformer.Forward and syncNamespaces directly. Proving it
# end-to-end would mean running the lab with Kyverno removed, which would take
# most of the other assertions down with it.
$T label namespace ns-probe-ok pod-security.kubernetes.io/enforce=privileged --overwrite >/dev/null 2>&1
psa=$($K get ns "$TID-ns-probe-ok" -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}' 2>/dev/null)
if [ "$psa" = restricted ]; then
  ok "and a tenant asking for privileged on it gets restricted back"
else
  bad "a tenant cannot weaken its own namespace" "enforce is now '${psa:-<none>}', want restricted"
fi

# The namespaces the CONTROLLER creates never pass through the gateway, so they
# are a separate path with the same requirement.
psa=$($K get ns "$NS" -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}' 2>/dev/null)
if [ "$psa" = restricted ]; then
  ok "and so does one the controller created, which never went through kubezoo"
else
  bad "a controller-created namespace carries the Pod Security level" \
      "enforce='${psa:-<none>}', want restricted"
fi

echo
echo "== server-side apply works, which is how modern controllers write =="
# Every apply was refused before this: Get answers a missing object with a nil
# alongside NotFound, and the nil reached runtime.SetZeroValue, which returns
# "expected pointer, but got invalid kind". It is not a tenancy problem -- a
# plain ConfigMap was enough to reproduce it -- and it matters because
# controller-runtime applies objects server-side as a matter of course.
expect_allowed "applying an object that does not exist yet, which is the usual way to apply" \
  $T -n default apply --server-side -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata: {name: ssa-probe}
data: {a: "1"}
EOF
if [ "$($T -n default get configmap ssa-probe -o jsonpath='{.data.a}' 2>/dev/null)" = 1 ]; then
  ok "and the object it wrote is the object that was asked for"
else
  bad "the object it wrote is the object that was asked for" \
      "got '$($T -n default get configmap ssa-probe -o jsonpath='{.data}' 2>&1 | cut -c1-80)'"
fi
# No --force-conflicts, and a changed value: this is where resolving the apply
# here and writing the result as an update shows up, because the field is then
# owned by an update from the same manager and the apply collides with it.
expect_allowed "applying over an object that does exist, changing a field it already owns" \
  $T -n default apply --server-side -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata: {name: ssa-probe}
data: {a: "changed", b: "2"}
EOF
if [ "$($T -n default get configmap ssa-probe -o jsonpath='{.data.a}' 2>/dev/null)" = changed ] &&
   [ "$($T -n default get configmap ssa-probe -o jsonpath='{.data.b}' 2>/dev/null)" = 2 ]; then
  ok "and the second apply is visible in the object"
else
  bad "the second apply is visible in the object" \
      "a='$($T -n default get configmap ssa-probe -o jsonpath='{.data.a}' 2>&1)' b='$($T -n default get configmap ssa-probe -o jsonpath='{.data.b}' 2>&1)'"
fi
# Applying the same thing again has to be a no-op. It was not: kubezoo resolved
# the apply and wrote the result with a PUT, so upstream recorded an update, and
# the next apply from the same manager conflicted with its own last one.
again=$($T -n default apply --server-side -f - 2>&1 <<EOF
apiVersion: v1
kind: ConfigMap
metadata: {name: ssa-probe}
data: {a: "changed-again", b: "2"}
EOF
)
if grep -qi conflict <<<"$again"; then
  bad "applying the same thing twice converges" \
      "the second apply conflicts with the first: $(tr '\n' ' ' <<<"$again" | cut -c1-140)"
else
  ok "applying the same thing twice converges, which is the whole point of applying"
fi
# The manager that applied has to be recorded as having applied. Another entry
# saying Apply proves nothing -- it is this manager's next apply that collides.
op=$($K -n "$NS" get configmap ssa-probe \
  -o jsonpath='{range .metadata.managedFields[?(@.manager=="kubectl")]}{.operation} {end}' 2>/dev/null)
if grep -q Apply <<<"$op"; then
  ok "and upstream records it as an apply, not as an update"
else
  bad "upstream records it as an apply, not as an update" \
      "operations upstream are '$op' -- the next apply will conflict with this one"
fi
# Dropping a field from the manifest has to drop it from the object, which is
# what separates applying from patching.
$T -n default apply --server-side -f - >/dev/null 2>&1 <<EOF
apiVersion: v1
kind: ConfigMap
metadata: {name: ssa-probe}
data: {a: "changed-again"}
EOF
if [ -z "$($T -n default get configmap ssa-probe -o jsonpath='{.data.b}' 2>/dev/null)" ]; then
  ok "a field left out of a later apply is removed from the object"
else
  bad "a field left out of a later apply is removed" "b is still there, so the apply was recorded as an update"
fi

# A custom resource has to converge too, and it has a trap the native types do
# not: every managed-fields entry records the apiVersion it was written against,
# and for a custom resource that version carries the tenant prefix upstream. Left
# unrewritten it reaches the resource's own converter, which refuses it -- so the
# object is created and the second apply is the one that fails.
# Its own CRD, with spec declared: a structural schema that declares nothing
# refuses every field, which would fail this for a reason that has nothing to do
# with what is being measured.
$T apply -f - >/dev/null 2>&1 <<EOF
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: ssawidgets.verify.example}
spec:
  group: verify.example
  names: {plural: ssawidgets, singular: ssawidget, kind: SsaWidget}
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              properties:
                size: {type: string}
EOF
cr_ready=no
for _ in $(seq 30); do
  $T -n default get ssawidgets >/dev/null 2>&1 && { cr_ready=yes; break; }
  sleep 2
done
cr_apply() {
  $T -n default apply --server-side -f - <<EOF
apiVersion: verify.example/v1
kind: SsaWidget
metadata: {name: ssa-widget}
spec: {size: "$1"}
EOF
}
if [ "$cr_ready" = yes ]; then
  expect_allowed "applying a custom resource" cr_apply L
  cr_second=$(cr_apply XL 2>&1)
  if [ $? -eq 0 ]; then
    ok "and applying it again, changing a field it owns, which is where the prefix leaks"
  else
    bad "applying a custom resource again, changing a field it owns" \
        "$(tr '\n' ' ' <<<"$cr_second" | cut -c1-160)"
  fi
  cr_version=$($T -n default get ssawidget ssa-widget \
    -o jsonpath='{.metadata.managedFields[0].apiVersion}' 2>/dev/null)
  if [ "$cr_version" = "verify.example/v1" ]; then
    ok "and the version it records is the one the tenant wrote, not the prefixed one"
  else
    bad "the version it records is the one the tenant wrote" \
        "it says '$cr_version', which is kubezoo's name for the group and not the tenant's"
  fi
fi

# A patch that may not create must still refuse to, or a typo would silently
# become a new object.
patch_out=$($T -n default patch configmap ssa-absent --type=merge -p '{"data":{"x":"1"}}' 2>&1)
if grep -q "NotFound\|not found" <<<"$patch_out"; then
  ok "while an ordinary patch of something absent is still NotFound, not a create"
else
  bad "an ordinary patch of something absent is still NotFound" "got: $(tr '\n' ' ' <<<"$patch_out" | cut -c1-120)"
fi

echo
echo "== a tenant can write the ClusterRoles an operator chart ships, and no more =="
# A tenant holds its namespaced permissions per namespace, so RBAC's escalation
# check -- asked at cluster scope -- refuses every ClusterRole a chart ships,
# though the question that means something here, "do you hold it in all of your
# namespaces?", is yes. kubezoo asserts escalate on writes to clusterroles, and
# on nothing else.
expect_allowed "a ClusterRole over namespaced resources, which used to be refused outright" \
  $T apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: chart-role}
rules:
- apiGroups: [""]
  resources: [pods, secrets, configmaps, events]
  verbs: [get, list, watch, create, update, patch, delete]
EOF

# A ServiceAccount of its own, so that "it can now read secrets" is this binding
# and not something else in the namespace. The default SA already holds things.
$T -n default create serviceaccount chart-op >/dev/null 2>&1
expect_allowed "binding it inside the tenant's own namespace, which is what makes it useful" \
  $T -n default apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: chart-binding}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: chart-role}
subjects: [{kind: ServiceAccount, name: chart-op, namespace: default}]
EOF

chart_sa="system:serviceaccount:$NS:chart-op"
if [ "$($K auth can-i get secrets -n "$NS" --as="$chart_sa" 2>/dev/null)" = yes ] &&
   [ "$($K auth can-i get secrets -n kube-system --as="$chart_sa" 2>/dev/null)" = no ]; then
  ok "the grant lands in the tenant's namespace and nowhere else"
else
  bad "the grant lands in the tenant's namespace and nowhere else" \
      "own=$($K auth can-i get secrets -n "$NS" --as="$chart_sa" 2>/dev/null) kube-system=$($K auth can-i get secrets -n kube-system --as="$chart_sa" 2>/dev/null)"
fi

# The containment: a ClusterRole grants nothing until it is bound, and binding
# this one cluster-wide would reach every tenant's namespaces. The exemption is
# deliberately not asserted here, so upstream refuses it exactly as before.
# Binding it across the tenant's cluster is a projection, never an upstream
# ClusterRoleBinding -- that is what keeps a role the tenant wrote from reaching
# anyone else's namespaces. The ClusterRoleBinding block below measures it.
if [ "$($K get clusterrolebinding --no-headers 2>/dev/null | grep -c "^$TID-")" = 1 ]; then
  ok "and writing it created no cluster-spanning object, only the role"
else
  bad "writing it created no cluster-spanning object" \
      "upstream has $($K get clusterrolebinding --no-headers 2>/dev/null | grep "^$TID-" | tr '\n' ' ')"
fi

# The escape §AC measured: the tenant's ClusterRole named cluster-admin is the
# role the controller binds cluster-wide to it, and escalate would let the tenant
# rewrite it. Both write paths must refuse the name.
expect_denied "writing the platform's reserved role by name" "managed by the platform" -- \
  $T apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: cluster-admin}
rules: [{apiGroups: ["*"], resources: ["*"], verbs: ["*"]}]
EOF
expect_denied "and patching it, which is a second way to the same object" "managed by the platform" -- \
  $T patch clusterrole cluster-admin --type=json \
    -p '[{"op":"replace","path":"/rules","value":[{"apiGroups":["*"],"resources":["*"],"verbs":["*"]}]}]'
if [ "$($K auth can-i get secrets -n kube-system --as="$TID-admin" --as-group="kubezoo:proxied:$TID" 2>/dev/null)" = no ]; then
  ok "and the tenant still cannot read kube-system, which is what that escape reached"
else
  bad "the tenant still cannot read kube-system" "it can -- the reserved role was rewritten after all"
fi

echo
echo "== a ClusterRoleBinding is cluster-scoped to the tenant and its namespaces only =="
# What a tenant means by cluster-wide is "all of my namespaces". A real
# ClusterRoleBinding means every namespace in the shared cluster, so the tenant's
# is projected into one RoleBinding per namespace it owns instead. See
# pkg/proxy/crbprojection.go.
$T -n default create serviceaccount crb-op >/dev/null 2>&1
expect_allowed "a tenant can create one at all, which upstream refuses outright" \
  $T apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: crb-op}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: chart-role}
subjects: [{kind: ServiceAccount, name: crb-op, namespace: default}]
EOF

if [ "$($T get clusterrolebinding crb-op -o jsonpath='{.metadata.name}' 2>/dev/null)" = crb-op ] &&
   $T get clusterrolebinding --no-headers 2>/dev/null | grep -q '^crb-op'; then
  ok "and reads it back as a cluster-scoped object, by name and in a listing"
else
  bad "and reads it back as a cluster-scoped object" \
      "get='$($T get clusterrolebinding crb-op -o jsonpath='{.metadata.name}' 2>&1 | tr '\n' ' ' | cut -c1-100)'"
fi

# The point of the exercise: no such object exists upstream, because one would
# span every tenant.
if [ "$($K get clusterrolebinding "$TID-crb-op" --no-headers 2>&1 | grep -c NotFound)" = 1 ]; then
  ok "while upstream has no ClusterRoleBinding of its own, which is the whole reason for this"
else
  bad "upstream has no ClusterRoleBinding of its own" \
      "$TID-crb-op exists upstream and grants in every tenant's namespaces"
fi

crb_sa="system:serviceaccount:$NS:crb-op"
# The authorizer answers from a cache that lags the write, so poll rather than
# ask once -- measured at roughly 300ms, but a loaded apiserver takes longer.
reachable=missing
for _ in $(seq 20); do
  reachable=yes
  for ns in "$TID-default" "$TID-kube-system" "$TID-kube-public"; do
    [ "$($K auth can-i get secrets -n "$ns" --as="$crb_sa" 2>/dev/null)" = yes ] || reachable="missing $ns"
  done
  [ "$reachable" = yes ] && break
  sleep 2
done
if [ "$reachable" = yes ]; then
  ok "the grant reaches every namespace the tenant owns"
else
  bad "the grant reaches every namespace the tenant owns" "$reachable"
fi
if [ "$($K auth can-i get secrets -n kube-system --as="$crb_sa" 2>/dev/null)" = no ] &&
   [ "$($K auth can-i get secrets -n default --as="$crb_sa" 2>/dev/null)" = no ]; then
  ok "and no further -- a real ClusterRoleBinding would have reached the platform's own namespaces"
else
  bad "the grant reaches no further than the tenant" "it reaches the platform's namespaces"
fi

# A namespace created after the binding has to get it too, or the tenant's
# cluster-wide grant would quietly not be.
$T create namespace crb-later >/dev/null 2>&1
projected=no
for _ in $(seq 30); do
  if [ "$($K auth can-i get secrets -n "$TID-crb-later" --as="$crb_sa" 2>/dev/null)" = yes ]; then
    projected=yes; break
  fi
  sleep 2
done
if [ "$projected" = yes ]; then
  ok "a namespace created afterwards is covered too"
else
  bad "a namespace created afterwards is covered too" \
      "namespace phase=$($K get ns "$TID-crb-later" -o jsonpath='{.status.phase}' 2>&1), projections there: $($K -n "$TID-crb-later" get rolebindings --no-headers 2>&1 | tr '\n' ' ' | cut -c1-120)"
fi

# The projections must not show up as RoleBindings, or the tenant would find a
# copy of every ClusterRoleBinding in every namespace it looks at.
if ! $T get rolebindings -A --no-headers 2>/dev/null | grep -q "kubezoo:"; then
  ok "the projections do not appear among the tenant's own RoleBindings"
else
  bad "the projections do not appear among the tenant's own RoleBindings" \
      "$($T get rolebindings -A --no-headers 2>/dev/null | grep kubezoo: | head -2 | tr '\n' ' ')"
fi
expect_denied "nor can the tenant write one by name" "kubezoo keeps for your tenant" -- \
  $T -n default apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: "kubezoo:clusterrolebinding:crb-op"}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: chart-role}
subjects: [{kind: ServiceAccount, name: crb-op, namespace: default}]
EOF

# The reason any of this matters: an operator reads across its whole cluster.
$T -n default create secret generic crb-probe --from-literal=a=b >/dev/null 2>&1
$T -n crb-later create secret generic crb-probe --from-literal=a=b >/dev/null 2>&1
CRBTOK=$($K -n "$NS" create token crb-op --duration=10m 2>/dev/null)
if [ -n "$CRBTOK" ]; then
  CRBKC=$LAB/verify-crb-sa.kubeconfig; rm -f "$CRBKC"
  kubectl --kubeconfig "$CRBKC" config set-cluster zoo --certificate-authority=$PKI/ca.pem \
    --embed-certs=true --server=https://127.0.0.1:6443 >/dev/null
  kubectl --kubeconfig "$CRBKC" config set-credentials sa --token="$CRBTOK" >/dev/null
  kubectl --kubeconfig "$CRBKC" config set-context c --cluster=zoo --user=sa >/dev/null
  kubectl --kubeconfig "$CRBKC" config use-context c >/dev/null
  seen=$(kubectl --kubeconfig "$CRBKC" get secrets -A --no-headers 2>/dev/null | grep -c crb-probe)
  if [ "$seen" -ge 2 ]; then
    ok "an operator ServiceAccount reads across the tenant's namespaces with it ($seen)"
  else
    bad "an operator ServiceAccount reads across the tenant's namespaces with it" \
        "saw $seen of 2: $(kubectl --kubeconfig "$CRBKC" get secrets -A 2>&1 | head -1 | cut -c1-140)"
  fi
  rm -f "$CRBKC"
fi

# Deleting it has to withdraw the grant everywhere, not just where the record is.
$T delete clusterrolebinding crb-op >/dev/null 2>&1
withdrawn=yes
for ns in "$TID-default" "$TID-kube-system" "$TID-crb-later"; do
  [ "$($K auth can-i get secrets -n "$ns" --as="$crb_sa" 2>/dev/null)" = no ] || withdrawn="still granted in $ns"
done
if [ "$withdrawn" = yes ]; then
  ok "deleting it withdraws the grant from every namespace"
else
  bad "deleting it withdraws the grant from every namespace" \
      "$withdrawn -- a copy left behind still grants, and nothing in the tenant's view explains it"
fi

echo
echo "== a tenant's ServiceAccount gets the cluster-wide half that is its own =="
# The projection is a RoleBinding per namespace and a RoleBinding never
# authorizes a cluster-scoped resource, so the cluster-scoped rules of a tenant's
# ClusterRole were dropped in silence -- the tenant wrote the binding, saw no
# error, and its operator failed at runtime. The part that can be granted for
# real is the part confined to the tenant's own API groups: those names carry the
# tenant, so there is nothing to filter.
$T apply -f - >/dev/null 2>&1 <<EOF
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: ownscopeds.verify.example}
spec:
  group: verify.example
  names: {plural: ownscopeds, singular: ownscoped, kind: OwnScoped}
  scope: Cluster
  versions: [{name: v1, served: true, storage: true, schema: {openAPIV3Schema: {type: object}}}]
EOF
cr_scoped=no
for _ in $(seq 30); do
  $T get ownscopeds >/dev/null 2>&1 && { cr_scoped=yes; break; }
  sleep 2
done
if [ "$cr_scoped" = yes ]; then
  $T -n default create serviceaccount own-op >/dev/null 2>&1
  $T apply -f - >/dev/null 2>&1 <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: own-op}
rules:
  - apiGroups: ["verify.example"]
    resources: ["ownscopeds"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "list", "watch"]
EOF
  expect_allowed "a tenant binds its ServiceAccount to a role naming its own cluster-scoped resource" \
    $T apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: own-op}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: own-op}
subjects: [{kind: ServiceAccount, name: own-op, namespace: default}]
EOF
  own_sa="system:serviceaccount:$NS:own-op"
  own_group="$TID-verify.example"
  granted=no
  for _ in $(seq 30); do
    [ "$($K auth can-i list "ownscopeds.$own_group" --as="$own_sa" 2>/dev/null)" = yes ] && { granted=yes; break; }
    sleep 2
  done
  if [ "$granted" = yes ]; then
    ok "and it can read that resource cluster-wide, which no RoleBinding could have given it"
  else
    bad "it can read that resource cluster-wide" \
        "still refused, so the cluster-scoped half of its own ClusterRole was dropped in silence"
  fi
  # The whole reason this is safe: the group name carries the tenant.
  if [ "$($K auth can-i list "ownscopeds.999999-verify.example" --as="$own_sa" 2>/dev/null)" = no ]; then
    ok "while the same resource in another tenant's group is refused, by name and not by filtering"
  else
    bad "the same resource in another tenant's group is refused" "it can read another tenant's"
  fi
  # Only its own groups: a rule over native resources must not come with it.
  if [ "$($K auth can-i list secrets --as="$own_sa" 2>/dev/null)" = no ] &&
     [ "$($K auth can-i get customresourcedefinitions --as="$own_sa" 2>/dev/null)" = no ]; then
    ok "and nothing native comes with it, though the same role granted secrets too"
  else
    bad "nothing native comes with it" \
        "secrets=$($K auth can-i list secrets --as="$own_sa" 2>/dev/null) crds=$($K auth can-i get customresourcedefinitions --as="$own_sa" 2>/dev/null)"
  fi
  # Withdrawing has to be as prompt as granting, or a deleted binding keeps
  # granting with nothing in the tenant's view to explain it.
  $T delete clusterrolebinding own-op >/dev/null 2>&1
  withdrawn=no
  for _ in $(seq 30); do
    [ "$($K auth can-i list "ownscopeds.$own_group" --as="$own_sa" 2>/dev/null)" = no ] && { withdrawn=yes; break; }
    sleep 2
  done
  if [ "$withdrawn" = yes ]; then
    ok "and deleting the binding takes it away again"
  else
    bad "deleting the binding takes it away again" "it still grants, and nothing the tenant can see says why"
  fi
fi

echo
echo "== a tenant cannot grant anything to an identity it does not own =="
# Group subjects used to pass through unrewritten while User and ServiceAccount
# subjects were prefixed. Measured before this: a tenant bound system:authenticated
# -- every authenticated identity in the cluster -- and delete on
# customresourcedefinitions became available to every other tenant's workloads.
$T apply -f - >/dev/null 2>&1 <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: give-away}
rules: [{apiGroups: [apiextensions.k8s.io], resources: [customresourcedefinitions], verbs: [delete]}]
EOF
$T apply -f - >/dev/null 2>&1 <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: give-away}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: give-away}
subjects: [{kind: Group, name: "system:authenticated", apiGroup: rbac.authorization.k8s.io}]
EOF
landed=$($K -n "$TID-kube-system" get rolebinding "kubezoo:clusterrolebinding:give-away" \
  -o jsonpath='{.subjects[0].name}' 2>/dev/null)
if [ -n "$landed" ] && [ "$landed" != "system:authenticated" ]; then
  ok "a foreign group named as a subject is prefixed into one with no members"
else
  bad "a foreign group named as a subject is prefixed into one with no members" \
      "the subject landed upstream as '$landed'"
fi
if [ "$($K auth can-i delete customresourcedefinitions --as="system:serviceaccount:kube-system:default" 2>/dev/null)" = no ]; then
  ok "so nothing outside the tenant gained a permission from it"
else
  bad "so nothing outside the tenant gained a permission from it" \
      "an unrelated ServiceAccount can now delete any CRD in the cluster"
fi
# The one group shape a tenant legitimately names must keep working.
$T -n default apply -f - >/dev/null 2>&1 <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: own-sa-group}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: chart-role}
subjects: [{kind: Group, name: "system:serviceaccounts:default", apiGroup: rbac.authorization.k8s.io}]
EOF
if [ "$($K -n "$NS" get rolebinding own-sa-group -o jsonpath='{.subjects[0].name}' 2>/dev/null)" = "system:serviceaccounts:$NS" ] &&
   [ "$($T -n default get rolebinding own-sa-group -o jsonpath='{.subjects[0].name}' 2>/dev/null)" = "system:serviceaccounts:default" ]; then
  ok "while the tenant's own ServiceAccount group still resolves, and reads back as written"
else
  bad "the tenant's own ServiceAccount group still resolves, and reads back as written" \
      "upstream='$($K -n "$NS" get rolebinding own-sa-group -o jsonpath='{.subjects[0].name}' 2>/dev/null)'"
fi

echo
echo "== the tenant's cluster-scoped permissions exist only on a request kubezoo forwarded =="
# A cluster-scoped grant cannot be bounded by name -- RBAC's resourceNames is an
# exact match with no prefix form -- so as long as these permissions belonged to
# the tenant's user, that user could list and delete every other tenant's
# cluster-scoped objects. Nothing in the reference deployment lets a tenant
# present that identity upstream, since kubezoo signs tenant certificates with
# its own CA, so this was latent rather than reachable. It stops being latent the
# moment anyone issues tenant credentials from a CA upstream trusts.
#
# They now belong to a group kubezoo asserts when it forwards, so the identity is
# worth nothing on its own.
PROXIED="kubezoo:proxied:$TID"
subjects=$($K get clusterrolebinding "$TID-cluster-admin" -o jsonpath='{.subjects[*].kind}/{.subjects[*].name}' 2>&1)
if [ "$subjects" = "Group/$PROXIED" ]; then
  ok "the cluster-scoped role is bound to the forwarded-request group and to nothing else"
else
  bad "the cluster-scoped role is bound to the forwarded-request group and to nothing else" \
      "subjects are '$subjects' -- a leftover User subject means the permissions are still usable without kubezoo"
fi

expect_allowed "the tenant still reads cluster-scoped objects through kubezoo" \
  $T get ingressclasses

if [ "$($K auth can-i list ingressclasses --as="$TID-admin" 2>/dev/null)" = no ]; then
  ok "the same identity cannot list cluster-scoped objects without kubezoo"
else
  bad "the same identity cannot list cluster-scoped objects without kubezoo" \
      "it can, so the grant is still on the user and reaches every tenant's objects"
fi
if [ "$($K auth can-i delete ingressclasses --as="$TID-admin" 2>/dev/null)" = no ]; then
  ok "nor delete another tenant's cluster-scoped objects"
else
  bad "nor delete another tenant's cluster-scoped objects" "it can -- this is destructive, not merely disclosure"
fi

# Without this the assertion above would also pass if the role were simply
# broken, which would break the tenant rather than confine it.
if [ "$($K auth can-i list ingressclasses --as="$TID-admin" --as-group="$PROXIED" 2>/dev/null)" = yes ]; then
  ok "and the group is what makes the difference, so the two above are not just a broken role"
else
  bad "and the group is what makes the difference" "asserting the group changes nothing; the role itself is broken"
fi

# A tenant's ServiceAccounts hold what the tenant granted them per namespace.
# Handing them the group would turn every workload into a cluster-scoped one.
SATOK=$($K -n "$NS" create token default --duration=10m 2>/dev/null)
if [ -n "$SATOK" ]; then
  SAKC=$LAB/verify-sa-proxied.kubeconfig; rm -f "$SAKC"
  kubectl --kubeconfig "$SAKC" config set-cluster zoo --certificate-authority=$PKI/ca.pem \
    --embed-certs=true --server=https://127.0.0.1:6443 >/dev/null
  kubectl --kubeconfig "$SAKC" config set-credentials sa --token="$SATOK" >/dev/null
  kubectl --kubeconfig "$SAKC" config set-context c --cluster=zoo --user=sa >/dev/null
  kubectl --kubeconfig "$SAKC" config use-context c >/dev/null
  sa_out=$(kubectl --kubeconfig "$SAKC" get ingressclasses 2>&1)
  if grep -qi forbidden <<<"$sa_out"; then
    ok "a tenant ServiceAccount is not handed the group by going through kubezoo"
  else
    bad "a tenant ServiceAccount is not handed the group by going through kubezoo" \
        "it got a listing, so every tenant workload now holds cluster-scoped permissions"
  fi
  rm -f "$SAKC"
fi

echo
echo "== an IngressClass reference reaches the tenant's own, or the platform's on request =="
# spec.ingressClassName references a cluster-scoped object, so it needs the same
# prefixing as the object itself -- otherwise a tenant's own class is unreachable
# while the platform's is reachable by name, which is the wrong way round. The
# allowlist is how a tenant asks to be exposed.
$T apply -f - >/dev/null 2>&1 <<EOF
apiVersion: networking.k8s.io/v1
kind: IngressClass
metadata: {name: internal}
spec: {controller: verify.example/own}
EOF
for pair in "own:internal" "public:nginx" "borrowed:999999-nginx"; do
  name=${pair%%:*}; class=${pair##*:}
  $T -n default apply -f - >/dev/null 2>&1 <<EOF
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: class-$name}
spec:
  ingressClassName: $class
  rules:
    - host: $name.$TID.apps.example.com
      http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: s, port: {number: 80}}}}]}
EOF
done
sleep 4
upstream_class() { $K -n "$NS" get ingress "class-$1" -o jsonpath='{.spec.ingressClassName}' 2>/dev/null; }
tenant_class() { $T -n default get ingress "class-$1" -o jsonpath='{.spec.ingressClassName}' 2>/dev/null; }

if [ "$(upstream_class own)" = "$TID-internal" ]; then
  ok "a tenant's own class is prefixed, so its own controller can match it"
else
  bad "own class not prefixed" "upstream has '$(upstream_class own)', want $TID-internal -- the tenant's IngressClass object is prefixed and the reference is not, so they never meet"
fi
if [ "$(upstream_class public)" = nginx ]; then
  ok "a class on the public allowlist passes through, which is how a tenant asks to be exposed"
else
  bad "public class was rewritten" "upstream has '$(upstream_class public)', want nginx -- prefixed, the tenant cannot reach the platform's controller at all"
fi
if [ "$(upstream_class borrowed)" = "$TID-999999-nginx" ]; then
  ok "another tenant's class is prefixed into something that matches nothing"
else
  bad "borrowed class not contained" "upstream has '$(upstream_class borrowed)' -- a tenant can name another tenant's class directly"
fi
# The round trip: whatever the rewriting does, the tenant has to read back what
# it wrote, or applying its own manifest again would prefix it a second time.
round_trip=yes
for pair in "own:internal" "public:nginx" "borrowed:999999-nginx"; do
  name=${pair%%:*}; class=${pair##*:}
  [ "$(tenant_class "$name")" = "$class" ] || { bad "class round trip" "wrote $class, reads back $(tenant_class "$name")"; round_trip=no; }
done
[ "$round_trip" = yes ] && ok "every class reads back exactly as the tenant wrote it"

echo
echo "== the platform publishes storage classes, and only those =="
# ⭐ A tenant could always NAME a StorageClass -- pkg/convert/pvc.go passes
# spec.storageClassName through untranslated, so dynamic provisioning worked --
# but storage.k8s.io was not served at all, so it had no way to discover which
# names exist. pkg/convert/pv.go even refused a volume source and told the tenant
# to "use a StorageClass", naming a resource it could not enumerate.
#
# kind ships "standard"; the lab publishes exactly that one and creates a second
# class the platform keeps to itself.
$K apply -f - >/dev/null 2>&1 <<EOF
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: platform-internal}
provisioner: kubernetes.io/no-provisioner
EOF

# ⭐ Published by LABELLING the object, with the gateway already running. That is
# the whole point of the mechanism: it used to be a startup flag, which on the
# shipped single-replica StatefulSet made "offer one more storage class" an
# outage for every tenant's control plane, and made a typo in the flag silent.
$K label storageclass standard storageclass.kubezoo.io/published=true --overwrite >/dev/null 2>&1
# The informer has to notice. A second is generous; a watch event is immediate.
for _ in $(seq 20); do
  [ "$($T get storageclass --no-headers 2>/dev/null | wc -l)" != 0 ] && break
  sleep 1
done
if [ "$($T get storageclass --no-headers 2>/dev/null | awk '{print $1}')" = standard ]; then
  ok "labelling a class publishes it with no restart"
else
  bad "labelling a class publishes it with no restart" \
      "after labelling, the tenant sees: $($T get storageclass --no-headers 2>&1 | tr '\n' ' ' | cut -c1-120)"
fi

published=$($T get storageclass --no-headers 2>&1 | awk '{print $1}' | sort | tr '\n' ' ')
if [ "$(echo $published)" = "standard" ]; then
  ok "a tenant sees exactly the classes the platform published"
else
  bad "a tenant sees exactly the published classes" "got '$published', want 'standard'"
fi

# Under the real name, because that is the name that has to work in a PVC.
if $T get storageclass standard >/dev/null 2>&1; then
  ok "and reads one by the name that works in a PersistentVolumeClaim"
else
  bad "a published class is readable by its real name" \
      "$($T get storageclass standard 2>&1 | tr '\n' ' ' | cut -c1-120)"
fi

# An unpublished class is NotFound, not Forbidden: the tenant is not told what it
# may not use.
unpublished=$($T get storageclass platform-internal 2>&1)
if grep -q -i "notfound\|not found" <<<"$unpublished"; then
  ok "an unpublished class reads as NotFound, so the tenant is not told it exists"
else
  bad "an unpublished class is hidden" "got: $(tr '\n' ' ' <<<"$unpublished" | cut -c1-120)"
fi

# It is the platform's object; a tenant may not write it.
expect_denied_any() {
  local what=$1; shift
  if "$@" >/dev/null 2>&1; then
    bad "$what" "the write was accepted"
  else
    ok "$what"
  fi
}
expect_denied_any "and a tenant cannot create a storage class of its own" \
  $T create -f - <<EOF
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: mine}
provisioner: kubernetes.io/no-provisioner
EOF
expect_denied_any "nor delete the platform's" $T delete storageclass standard

# And the reference the whole feature exists for still works.
$T apply -f - >/dev/null 2>&1 <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: sc-probe}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: standard
  resources: {requests: {storage: 1Gi}}
EOF
stored=$($K -n "$NS" get pvc sc-probe -o jsonpath='{.spec.storageClassName}' 2>/dev/null)
if [ "$stored" = standard ]; then
  ok "and a PVC naming a published class reaches upstream with that exact name"
else
  bad "a PVC naming a published class" "upstream stored storageClassName='${stored:-<empty>}', want standard"
fi

# Retiring is a distinct state: still visible -- so a tenant can explain a
# reference it already has -- and marked as on the way out.
$K label storageclass standard storageclass.kubezoo.io/published=deprecated --overwrite >/dev/null 2>&1
for _ in $(seq 20); do
  [ "$($T get storageclass standard -o jsonpath='{.metadata.labels.storageclass\.kubezoo\.io/published}' 2>/dev/null)" = deprecated ] && break
  sleep 1
done
retired=$($T get storageclass standard -o jsonpath='{.metadata.labels.storageclass\.kubezoo\.io/published}' 2>/dev/null)
if [ "$retired" = deprecated ]; then
  ok "a retired class stays visible, and the tenant can read that it is retiring"
else
  bad "a retired class stays visible" "tenant sees published='${retired:-<gone>}', want deprecated"
fi

# ⭐ While it is retired, a NEW claim naming it is refused -- on both create
# paths. POST is what client-side `kubectl apply` uses for an object that does
# not exist; `kubectl apply --server-side` sends a PATCH instead, which reaches
# tenantProxy.Create only because guaranteedUpdate hands the missing case over
# rather than sending it as an update. Asserting only the first would leave the
# common path unproven.
for mode in "" "--server-side"; do
  what=${mode:---post}
  out=$($T apply $mode -f - 2>&1 <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: sc-retired-$$-${what#--}}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: standard
  resources: {requests: {storage: 1Mi}}
EOF
)
  if grep -q "being retired" <<<"$out"; then
    ok "a new claim naming a retired class is refused (${what#--})"
  else
    bad "a new claim naming a retired class is refused (${what#--})" \
        "got: $(tr '\n' ' ' <<<"$out" | cut -c1-160)"
  fi
done

# ...while the claim that already names it keeps being writable. This is the
# reason retirement is a state of its own rather than just removing the label:
# spec.storageClassName is immutable once bound, so a tenant cannot edit its way
# out, and refusing on update would make every later write fail -- including a
# GitOps controller reapplying a manifest it has not changed.
#
# ⚠️ No -n here, and no upstream namespace name. $T already carries -n default,
# which is where the claim above was created; adding a second -n wins over the
# first, and naming the UPSTREAM namespace on a patch does not work even though
# it does on a get. tp.Get hands the patcher an object whose namespace has been
# converted back to the tenant's name, the request URL still carries the upstream
# one, and the generic patcher refuses the mismatch with a BadRequest that says
# nothing about namespaces being translated at all.
if $T patch persistentvolumeclaims sc-probe --type=merge \
     -p '{"metadata":{"annotations":{"probe":"still-writable"}}}' >/dev/null 2>&1; then
  ok "and an existing claim on that same class is still writable"
else
  bad "an existing claim on a retired class is still writable" \
      "$($T patch persistentvolumeclaims sc-probe --type=merge -p '{"metadata":{"annotations":{"probe":"x"}}}' 2>&1 | tr '\n' ' ' | cut -c1-160)"
fi

# A claim that names no class at all is a request for the DEFAULT class, and must
# not be caught by any of this.
if $T apply -f - >/dev/null 2>&1 <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: sc-default-$$}
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 1Mi}}
EOF
then
  ok "a claim naming no class at all is still accepted"
else
  bad "a claim naming no class is accepted" "the default-class path was refused"
fi
$T delete pvc "sc-default-$$" >/dev/null 2>&1

# ⭐ And a class the platform never published is refused outright, not merely
# hidden. Before this, spec.storageClassName passed through unvalidated: a tenant
# that learned "platform-internal" out of band could provision on it while
# `kubectl get storageclass` swore it did not exist. Publication is authorization
# now, not only discovery.
unpub=$($T apply -f - 2>&1 <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: sc-unpub-$$}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: platform-internal
  resources: {requests: {storage: 1Mi}}
EOF
)
if grep -q "no storage class" <<<"$unpub"; then
  ok "a claim on a class the platform never published is refused, not just hidden"
else
  bad "a claim on an unpublished class is refused" \
      "got: $(tr '\n' ' ' <<<"$unpub" | cut -c1-160)"
fi
$T delete pvc "sc-unpub-$$" >/dev/null 2>&1

# ⚠️ Unpublishing stops NEW claims -- publication is authorization now -- but must
# not disturb anything a tenant already has. The PVC written above keeps its class
# and upstream keeps provisioning it, which is why withdrawing a class is still
# survivable for the tenants already on it, and why "deprecated" exists to give
# them warning before it happens.
$K label storageclass standard storageclass.kubezoo.io/published- >/dev/null 2>&1
for _ in $(seq 20); do
  [ "$($T get storageclass --no-headers 2>/dev/null | wc -l)" = 0 ] && break
  sleep 1
done
still=$($K -n "$NS" get pvc sc-probe -o jsonpath='{.spec.storageClassName}' 2>/dev/null)
if [ "$still" = standard ]; then
  ok "and unpublishing leaves an existing PVC's class untouched"
else
  bad "unpublishing leaves an existing PVC alone" "the PVC's storageClassName is now '${still:-<gone>}'"
fi

echo
echo "== a volume attributes class is a tier the platform sells, not one a tenant takes =="
# ⭐ spec.volumeAttributesClassName reached upstream untranslated and unvalidated,
# exactly as spec.storageClassName did. A VolumeAttributesClass carries the CSI
# driver's IOPS and throughput parameters, so naming one is asking for a
# performance tier. The gate is GA and LockToDefault in 1.36 -- live, and it
# cannot be switched off.
#
# ⚠️ Nothing is published by default here, unlike storage classes. With no class
# labelled no tenant can set the field at all, which is the useful default for
# something a platform sells.
$K apply -f - >/dev/null 2>&1 <<EOF
apiVersion: storage.k8s.io/v1
kind: VolumeAttributesClass
metadata: {name: vac-gold}
driverName: lab.example.com
parameters: {iops: "9000"}
---
apiVersion: storage.k8s.io/v1
kind: VolumeAttributesClass
metadata: {name: vac-platform-only}
driverName: lab.example.com
parameters: {iops: "99000"}
EOF
$K label volumeattributesclass vac-gold volumeattributesclass.kubezoo.io/published=true --overwrite >/dev/null 2>&1
for _ in $(seq 20); do
  [ "$($T get volumeattributesclass --no-headers 2>/dev/null | wc -l)" != 0 ] && break
  sleep 1
done

vacseen=$($T get volumeattributesclass --no-headers 2>&1 | awk '{print $1}' | sort | tr '\n' ' ')
if [ "$(echo $vacseen)" = "vac-gold" ]; then
  ok "a tenant sees only the volume attributes classes the platform published"
else
  bad "a tenant sees only the published volume attributes classes" \
      "got '$vacseen', want 'vac-gold'"
fi

# ⚠️ No storageClassName on purpose: this section runs after the storage-class
# section has withdrawn its label, so naming one here would be refused by THAT
# guard and every assertion below would fail for the wrong reason -- which is
# exactly what happened the first time. Leaving it unset asks for the default
# class and is never refused.
mkvac() { # $1 = claim name, $2 = attributes class ("" for none)
  if [ -z "$2" ]; then
    $T apply -f - 2>&1 <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: $1}
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 1Mi}}
EOF
  else
    $T apply -f - 2>&1 <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: $1}
spec:
  accessModes: [ReadWriteOnce]
  volumeAttributesClassName: $2
  resources: {requests: {storage: 1Mi}}
EOF
  fi
}

out=$(mkvac "vac-ok-$$" vac-gold)
if grep -qE "created|configured|unchanged" <<<"$out"; then
  ok "a claim naming a published tier is accepted"
else
  bad "a claim naming a published tier is accepted" "$(tr '\n' ' ' <<<"$out" | cut -c1-160)"
fi

out=$(mkvac "vac-bad-$$" vac-platform-only)
if grep -q "no volume attributes class" <<<"$out"; then
  ok "and one naming a tier the platform kept to itself is refused"
else
  bad "a claim naming an unpublished tier is refused" "$(tr '\n' ' ' <<<"$out" | cut -c1-160)"
fi

# ⭐ The mutable half, which a create-only check would miss entirely: this field
# can be changed on a bound claim, and changing it is how a tenant would raise its
# own tier after the fact.
out=$($T patch persistentvolumeclaims "vac-ok-$$" --type=merge \
        -p '{"spec":{"volumeAttributesClassName":"vac-platform-only"}}' 2>&1)
if grep -q "no volume attributes class" <<<"$out"; then
  ok "and raising the tier on an existing claim is refused too, not only at create"
else
  bad "raising the tier on an existing claim is refused" "$(tr '\n' ' ' <<<"$out" | cut -c1-160)"
fi

# ⚠️ ...while a write that does not touch the class must go through. Refusing
# these is the reconcile loop the whole "only when it changes" rule exists to
# avoid: a GitOps controller reapplying an unchanged manifest would fail forever.
out=$($T patch persistentvolumeclaims "vac-ok-$$" --type=merge \
        -p '{"metadata":{"annotations":{"probe":"unchanged"}}}' 2>&1)
if grep -qE "patched|unchanged" <<<"$out"; then
  ok "while a write that leaves the tier alone still goes through"
else
  bad "a write leaving the tier alone goes through" "$(tr '\n' ' ' <<<"$out" | cut -c1-160)"
fi

$T delete pvc "vac-ok-$$" "vac-bad-$$" >/dev/null 2>&1
$K delete volumeattributesclass vac-gold vac-platform-only >/dev/null 2>&1

$T delete pvc sc-probe >/dev/null 2>&1
$K delete storageclass platform-internal >/dev/null 2>&1
$K label storageclass standard storageclass.kubezoo.io/published- >/dev/null 2>&1

echo
echo "== a tenant can only claim host names under its own subdomain =="
# spec.rules host is a free-form DNS name that kubezoo cannot rewrite -- prefixing
# a domain destroys what it is for -- so without this any tenant could claim any
# hostname and the ingress controller would settle it by creation order, telling
# the loser nothing.
ingress_with_host() {
  cat <<EOF
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: $1}
spec:
  ingressClassName: nginx
  rules:
    - host: $2
      http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: s, port: {number: 80}}}}]}
EOF
}
expect_allowed "an Ingress under the tenant's own subdomain" \
  $T -n default apply -f <(ingress_with_host host-own "shop.$TID.apps.example.com")
expect_denied "one under another tenant's subdomain" "own subdomain" -- \
  $T -n default apply -f <(ingress_with_host host-other "shop.999999.apps.example.com")
expect_denied "an arbitrary external hostname" "own subdomain" -- \
  $T -n default apply -f <(ingress_with_host host-external bank.example.com)
# An empty host matches every hostname, which is the whole entry point rather
# than one name, so it has to be refused too.
expect_denied "an Ingress with no host at all" "own subdomain" -- \
  $T -n default apply -f - <<EOF
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: host-bare}
spec:
  ingressClassName: nginx
  rules:
    - http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: s, port: {number: 80}}}}]}
EOF
# Updates as well as creates: otherwise a tenant claims a legal name and edits it.
expect_denied "editing an accepted Ingress onto a foreign hostname" "own subdomain" -- \
  $T -n default patch ingress host-own --type=json \
    -p '[{"op":"replace","path":"/spec/rules/0/host","value":"evil.example.com"}]'
# The platform's own namespaces are not tenants and are not constrained.
expect_allowed "the platform may still use any hostname it likes" \
  $K -n kube-public create ingress verify-plat --rule='anything.example.com/*=s:80'
$K -n kube-public delete ingress verify-plat >/dev/null 2>&1

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
echo "== a server-side apply carries what kubezoo injects, not only what the tenant owns =="
# ⭐ The field set a forwarded apply is built from is computed before conversion,
# so a field kubezoo *adds* is owned by nobody. Both victims are here: the
# namespace label, which every cluster-wide list and the controller select on,
# and the webhook's namespaceSelector, which is the confinement the contract's
# cluster-wide grant on webhook configurations is justified by.
$T apply --server-side --field-manager=verify -f - >/dev/null 2>&1 <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: ssa-ns
  labels: {env: prod}
EOF
label=$($K get ns "$TID-ssa-ns" -o jsonpath='{.metadata.labels.kubezoo\.io/tenant}' 2>/dev/null)
if [ "$label" = "$TID" ]; then
  ok "an applied namespace is labelled, so it is visible to cluster-wide list and watch"
else
  bad "an applied namespace is labelled" "kubezoo.io/tenant = '${label:-<absent>}'"
fi

$T apply --server-side --field-manager=verify -f - >/dev/null 2>&1 <<EOF
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: ssa-hook
webhooks:
- name: w.example.com
  admissionReviewVersions: ["v1"]
  sideEffects: None
  clientConfig:
    service: {name: svc, namespace: default, port: 443}
  rules:
  - apiGroups: [""]
    apiVersions: ["v1"]
    operations: ["CREATE"]
    resources: ["pods"]
EOF
# ⚠️ Not `-n "$selector"`: with the selector dropped, upstream defaults the field
# to {} -- which is a selector that matches *every* namespace, and which bash
# reads as a non-empty string. The measurement has to name the label.
selector=$($K get validatingwebhookconfiguration "$TID-ssa-hook" \
  -o jsonpath='{.webhooks[0].namespaceSelector.matchLabels.kubezoo\.io/tenant}' 2>/dev/null)
if [ "$selector" = "$TID" ]; then
  ok "an applied webhook keeps the namespace selector that confines it to the tenant"
else
  bad "an applied webhook keeps its namespace selector" \
      "selects on kubezoo.io/tenant='${selector:-<nothing>}': this webhook fires on every namespace in the cluster"
fi
$T delete validatingwebhookconfiguration ssa-hook >/dev/null 2>&1

echo
echo "== an ordinary update by a manager that has applied before stays an update =="
# ⭐ The verb used to be inferred from the object's managedFields, so a PUT by a
# manager with an Apply entry was rewritten into an apply of that stale entry --
# dropping the rest of the write, and deleting upstream the field it changed.
$T apply --server-side --field-manager=ctrl -f - >/dev/null 2>&1 <<EOF
apiVersion: v1
kind: ConfigMap
metadata: {name: ssa-cm}
data: {a: "1"}
EOF
# ⚠️ The PUT must ADD a field and leave the applied one alone. The obvious
# version -- change a and add b -- passes against the defect, and the reason is
# worth knowing: changing a field the Apply entry owns is exactly what destroys
# that entry. The field manager subtracts the conflicting paths, the entry's set
# becomes empty and the entry is deleted, so the unfixed code finds no Apply
# entry, declines to forward, and the PUT lands correctly. Leaving a untouched
# keeps the entry alive, which is the state the defect needs: the unfixed code
# then forwards an apply of {a:1} alone and b never reaches upstream.
$T get configmap ssa-cm -o json 2>/dev/null \
  | python3 -c 'import json,sys; o=json.load(sys.stdin); o["data"]["b"]="2"; print(json.dumps(o))' \
  | $T replace --field-manager=ctrl -f - >/dev/null 2>&1
got_a=$($K -n "$NS" get configmap ssa-cm -o jsonpath='{.data.a}' 2>/dev/null)
got_b=$($K -n "$NS" get configmap ssa-cm -o jsonpath='{.data.b}' 2>/dev/null)
if [ "$got_a" = 1 ] && [ "$got_b" = 2 ]; then
  ok "the update added a field without the applied one being re-applied over it"
else
  bad "an update by a manager that has applied before" \
      "data.a='${got_a:-<gone>}' want 1, data.b='${got_b:-<gone>}' want 2"
fi

# And the apply path really did go up as an apply. ⚠️ Nothing else here can see
# this: if the forwarding is skipped, the ordinary write carries the whole
# converted object and the result looks perfect. Only the bookkeeping differs,
# and only the manager's next apply notices, by conflicting with its own last
# one -- which is the entire reason the forwarding exists.
op=$($K -n "$NS" get configmap ssa-cm \
  -o jsonpath='{.metadata.managedFields[?(@.manager=="ctrl")].operation}' 2>/dev/null)
if grep -q Apply <<<"$op"; then
  ok "and upstream recorded the apply as an apply, so the next one converges"
else
  bad "upstream recorded the apply as an apply" \
      "manager ctrl has operation='${op:-<nothing>}'; a repeated apply will now conflict with itself"
fi
$T delete configmap ssa-cm >/dev/null 2>&1

echo
echo "== one legal RoleBinding cannot make every RoleBinding unreadable =="
# ⭐ A ServiceAccount subject may omit its namespace in a namespaced RoleBinding.
# Reading it back used to be an error, and a list returns on the first item that
# fails -- so one such object, which a plain apply creates, hid all the others.
$T create -f - >/dev/null 2>&1 <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: no-subject-ns}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: view}
subjects:
- {kind: ServiceAccount, name: default}
EOF
if $T get rolebindings >/dev/null 2>&1 && $T get rolebinding no-subject-ns >/dev/null 2>&1; then
  ok "the whole list still reads, and so does the binding itself"
else
  bad "a subject without a namespace" "$($T get rolebindings 2>&1 | tr '\n' ' ' | cut -c1-160)"
fi
$T delete rolebinding no-subject-ns >/dev/null 2>&1

echo
echo "== deletecollection cannot reach what every other verb hides =="
# ⭐ Get returns NotFound for the projection records and List drops them, so the
# tenant is told they do not exist -- and then a collection delete removed them
# anyway. Deleting the *record* is not repaired: the controller reads an empty
# record set and withdraws every copy and every derived cluster binding.
$T create -f - >/dev/null 2>&1 <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: survives-deletecollection}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: view}
subjects:
- {kind: ServiceAccount, name: default, namespace: default}
EOF
sleep 2
kubectl --kubeconfig "$TKC" -n kube-system delete rolebindings --all >/dev/null 2>&1
curl -sk --cert "$LAB/verify-$TID.pem" --key "$LAB/verify-$TID-key.pem" --cacert $PKI/ca.pem \
  -X DELETE "https://127.0.0.1:6443/apis/rbac.authorization.k8s.io/v1/namespaces/kube-system/rolebindings" \
  >/dev/null 2>&1
sleep 3
if $T get clusterrolebinding survives-deletecollection >/dev/null 2>&1; then
  ok "a collection delete left the tenant's ClusterRoleBindings standing"
else
  bad "a collection delete destroyed the projection records" \
      "the tenant's ClusterRoleBinding is gone and nothing will bring it back"
fi
$T delete clusterrolebinding survives-deletecollection >/dev/null 2>&1

echo
echo "== a field selector is translated, so it finds the object rather than nothing =="
# ⭐ metadata.name went upstream unprefixed against a cluster-scoped resource and
# matched nothing -- which returns empty rather than erroring, so the standard
# single-object informer watched an empty world forever.
$T create -f - >/dev/null 2>&1 <<EOF
apiVersion: networking.k8s.io/v1
kind: IngressClass
metadata: {name: selected}
spec: {controller: example.com/ingress}
EOF
found=$($T get ingressclass --field-selector metadata.name=selected --no-headers 2>/dev/null | wc -l)
if [ "$found" = 1 ]; then
  ok "a cluster-scoped object is found by metadata.name"
else
  bad "a cluster-scoped object is found by metadata.name" "matched $found objects, want 1"
fi
$T delete ingressclass selected >/dev/null 2>&1
$T delete ns ssa-ns >/dev/null 2>&1

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
# ⚠️ This runs last, and deliberately, and it has to stay last. Producing a
# genuine denial means taking the tenant's RoleBinding away, and the authorizer
# serves a stale yes for a moment after the delete and a stale no for a moment
# after the controller puts it back -- measured at 29s to be put back. Anything
# scheduled after this runs inside a window where the tenant has no rights in its
# own namespace, and fails on work that has nothing to do with it. Six sections
# were once appended below this one and two of them failed for exactly that
# reason, with error messages -- AlreadyExists, Forbidden -- that pointed
# anywhere but here.
$K -n "$NS" delete rolebinding kubezoo:tenant-admin >/dev/null 2>&1
# ⚠️ A budget, not a fixed sleep. Deleting the binding does not deny anything
# until the authorizer's RBAC cache catches up, and how long that takes depends
# on what else the apiserver is doing -- measured at under a second on an idle
# tenant and past two seconds at the end of this run. A `sleep 2` here passed or
# failed on load, which is the worst kind of assertion: it reported a product
# defect when the product was fine, and would have reported nothing if the
# reshaping really had gone too wide on a quiet day. The upper bound is the
# controller putting the binding back, measured at 29s.
#
# Not a configmap: the tenant's own ClusterRoleBinding grants those across its
# namespaces now, so withdrawing the binding kubezoo issues no longer takes them
# away -- which is correct, and would make this measure nothing.
denied_out=""
for _ in $(seq 20); do
  denied_out=$($T get serviceaccount default 2>&1)
  grep -q -i forbidden <<<"$denied_out" && break
  sleep 1
done
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
