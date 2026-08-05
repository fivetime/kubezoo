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
# ⛔ A QUOTA FIXTURE MUST NOT LEAVE ITS MESS, AND USUALLY MUST GO LAST.
# Learned the hard way, twice, in one afternoon. A section that fills a tenant to
# a limit takes hostages: every later assertion that creates one of the capped
# things fails, and the failures point ANYWHERE but at the fixture -- a
# projection missing from a namespace that was never created, an operator
# ServiceAccount seeing one namespace instead of two, a ServiceAccount denied a
# grant it demonstrably has.
#
# Two distinct traps, and the second is not fixed by fixing the first:
#   1. Objects left behind hold the tenant at its limit. Delete them, and WAIT
#      for the count to come back down.
#   2. Churn outlives the objects. Creating and deleting twenty ClusterRoleBindings
#      is hundreds of writes through the projection, and the upstream authorizer
#      serves stale answers while its RBAC cache catches up. Deleting and waiting
#      did not help; only moving the section to the end did.
#
# ⚠️ And a cap set in up.sh has to leave headroom for everything this file does,
# not just for the section testing it.
#
# ⭐ A SEPARATE TRAP, AND NOT A CONSEQUENCE OF THAT CHURN: a tenant's PERMISSIONS
# do not arrive with its namespaces. Waiting for a namespace to go Active is
# waiting on the wrong object -- the RoleBinding inside it, and the tenant's
# cluster-scoped grants, land afterwards, and the authorizer lags further still.
#
# Reproduced on a cluster created from scratch seconds earlier, so this is the
# path's own asynchrony and not accumulated state. It cost two runs to see,
# because it moves: first a Forbidden on creating a namespace, then, once that
# was retried, a Forbidden on creating a pod INSIDE the new namespace.
#
# Retry the first write of each scope rather than waiting on an object. The
# symptom is a Forbidden that reads exactly like a genuine authorization
# decision, so an assertion that accepts any refusal will pass on it.
#
# ⭐⭐ THE HOSTAGE RULE IS NOT ABOUT QUOTAS. It is about any section that changes
# shared fixture STATE and intends to put it back. Written above as "a quota
# fixture", it got read as being about quotas -- and then a section that
# suspended $TID in order to check that suspensions are audited did precisely the
# same damage by another route: the tenant's cluster-scoped grant had not been
# widened back when the next section ran, and that reported as the
# ClusterRoleBinding cap failing to refuse. Same shape, same misdirection,
# different resource.
#
# ⚠️ "I restore it afterwards" is not an answer, because restoring is
# asynchronous and the probe that waits for it is usually wrong -- see the next
# rule. Give the section its own tenant; it costs one create and one delete.
#
# ⭐ A WAIT PROBE MUST DISTINGUISH THE TWO STATES. Three times in one session a
# loop waited on something that was true before AND after: a `get` while waiting
# for a ReadOnly suspension to lift (ReadOnly allows reads by design), a
# namespace going Active while waiting for permissions that arrive separately, a
# label read back that nothing had actually removed. A probe that succeeds in
# both states is not a wait, it is decoration -- and the cost lands on whatever
# runs next, under its name rather than this one's.
#
# ⭐ AND A SETUP STEP MUST NEVER FAIL QUIETLY. When the second namespace above
# silently failed to appear, the run did not report one broken fixture; it
# reported a missing ResourceQuota and a pod refused by RBAC instead of by quota,
# and neither named the step that had actually broken. Check every setup command
# and report it under its own name.
#
# ⭐ ANY ASSERTION CLAIMING TO VERIFY KUBEZOO'S OWN LAYER NEEDS A NEGATIVE CONTROL.
# With the policies healthy both layers usually produce the same outcome, so a
# green assertion proves nothing about which one did the work. One assertion here
# was found to be entirely vacuous that way -- see the note in the placement
# section. What can distinguish the two: a path the policy structurally does not
# cover (it matches operations: [CREATE] only), or a request that goes around
# kubezoo straight to upstream. Everything else belongs in a unit test.
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
# ⚠️ Through the ZOO. Tenants live in kubezoo's own etcd and are not an upstream
# resource, so deleting one with $K is a no-op that reports nothing -- and the
# wait below then spends its whole budget on namespaces nobody asked to go.
kubectl --kubeconfig "$ZOOKC" delete tenant "$TID" >/dev/null 2>&1
for _ in $(seq 40); do
  [ "$($K get ns -l "kubezoo.io/tenant=$TID" --no-headers 2>/dev/null | wc -l)" = 0 ] && break
  sleep 3
done
# ⛔ And say so when they do not go, instead of carrying on. Deleting a tenant is
# ASYNCHRONOUS: the request returns at once and the controller tears the
# namespaces down afterwards. up.sh wipes kubezoo's etcd, so a run started while
# the previous teardown is still in flight removes the Tenant object mid-way and
# the controller never finishes -- the namespaces survive with everything in
# them, and the next run fails with AlreadyExists on objects it has never heard
# of, in sections that have nothing to do with any of this. Measured: nine
# failures across six unrelated sections, from two runs three minutes apart.
leftover=$($K get ns -l "kubezoo.io/tenant=$TID" --no-headers 2>/dev/null | wc -l)
if [ "$leftover" != 0 ]; then
  echo "FATAL: $leftover namespaces of a previous $TID are still here after 120s." >&2
  echo "       A tenant teardown from an earlier run did not finish -- most likely up.sh wiped" >&2
  echo "       kubezoo's etcd while it was in flight. Delete them and run again:" >&2
  echo "         kubectl --context kind-$CLUSTER delete ns -l kubezoo.io/tenant=$TID" >&2
  exit 1
fi

# ⛔ And a tenant is not only its namespaces. The guard above covered the half of
# the leftovers that carries a label; the cluster-scoped half carries only the
# tenant's name in its own, and nothing was looking at it.
#
# Measured, today: a killed run left ten ClusterRoles, a ClusterRoleBinding and
# an IngressClass behind. Clearing the namespaces by hand was not enough -- the
# next run failed with "clusterroles ... already exists" and with a CRD grant
# that never arrived, in two sections that have nothing to do with either. That
# is the same misreading the comment above was written about, one scope up.
#
# ⚠️ Deleted rather than made fatal, and the difference is real: a terminating
# namespace cannot be helped along, so stopping is the only honest move there.
# These are ordinary objects nobody owns any more, and a delete finishes. But it
# SAYS SO when it finds any -- a run that quietly cleans up after a previous one
# every time is a run whose teardown is broken, and that has to stay visible.
#
# ⭐ CRDs are matched separately: a tenant's CRD is prefixed by GROUP, so its
# name is <plural>.<tenant>-<group> and a name-prefix match misses every one.
stale_cluster_scoped=""
for kind in clusterroles clusterrolebindings ingressclasses priorityclasses \
            runtimeclasses storageclasses volumeattributesclasses persistentvolumes; do
  found=$($K get "$kind" -o name --no-headers 2>/dev/null | grep -E "/$TID-" || true)
  [ -n "$found" ] && stale_cluster_scoped="$stale_cluster_scoped $found"
done
found=$($K get crds -o name --no-headers 2>/dev/null | grep -E "[/.]$TID-" || true)
[ -n "$found" ] && stale_cluster_scoped="$stale_cluster_scoped $found"
if [ -n "$stale_cluster_scoped" ]; then
  echo "NOTE cluster-scoped leftovers of a previous $TID were deleted before starting:"
  echo "     $(tr ' ' '\n' <<<"$stale_cluster_scoped" | sed '/^$/d' | tr '\n' ' ')"
  # shellcheck disable=SC2086
  $K delete $stale_cluster_scoped --wait=true >/dev/null 2>&1
fi

kubectl --kubeconfig "$ZOOKC" config set-cluster zoo --certificate-authority=$PKI/ca.pem \
  --embed-certs=true --server=https://127.0.0.1:6443 >/dev/null
kubectl --kubeconfig "$ZOOKC" config set-credentials admin --client-certificate=$PKI/admin.pem \
  --client-key=$PKI/admin-key.pem --embed-certs=true >/dev/null
kubectl --kubeconfig "$ZOOKC" config set-context zoo --cluster=zoo --user=admin >/dev/null
kubectl --kubeconfig "$ZOOKC" config use-context zoo >/dev/null
# ⚠️ Still no spec.quota, and the reason has changed rather than gone away. The
# quota path DOES run in this lab now, which is exactly why this tenant must
# stay outside it: every assertion below creates objects through this tenant, so
# a quota here would make an unrelated test fail the moment it created one pod
# too many, and the failure would point at whatever that test was about. The
# quota chain gets its own tenant, further down.
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

    # ⭐ The name the pod uses to ASK and the name the ANSWER carries have to be
    # the same string. They were not: the namespace file is filled by upstream's
    # ServiceAccount plugin from metadata.namespace, so the pod believed
    # <tid>-default while every object came back saying default.
    #
    # This breaks nothing visible. util.UpstreamNamespace makes the request
    # succeed either way, so a curl probe reports 200 and a tenant sees data.
    # What fails is a client that indexes objects by their own namespace and
    # then looks them up by the name it read from that file -- controller-runtime,
    # which is every operator built with kubebuilder that watches its own
    # namespace. Its cache never matches: no error, no log, no events.
    #
    # ⚠️ Asserted on the two strings AGREEING, not on the file's contents alone:
    # a probe that only read the file would still pass if the answer changed
    # spelling instead, and it is the disagreement that does the damage.
    cat >"$LAB/verify-sa-ns.sh" <<'PROBE'
S=/var/run/secrets/kubernetes.io/serviceaccount
NS=$(cat $S/namespace 2>/dev/null)
BODY=$(curl -sk -H "Authorization: Bearer $(cat $S/token)" \
  "https://ZOOHOST:6443/api/v1/namespaces/$NS/pods?limit=1" 2>&1)
# ⚠️ The apiserver pretty-prints, so it is "namespace": "x" with a space --
# an expression written for the compact form matches nothing and the probe
# reports no answer (it did, twice). Requiring at least one space also skips
# the escaped \"namespace\":\"x\" inside last-applied-configuration, which
# would otherwise be the first match and is the TENANT's spelling either way,
# so it would agree with the file no matter what the real answer said.
ANS=$(echo "$BODY" | grep -oE '"namespace": +"[^"]*"' | head -1 | sed 's/.*"\(.*\)"$/\1/')
echo "believes=[$NS] answers=[$ANS] raw=[$(echo "$BODY" | tr -d '\n' | head -c 300)]"
PROBE
    sed -i "s|ZOOHOST|$ZOO_HOST|" "$LAB/verify-sa-ns.sh"
    ns_out=$($K -n "$NS" exec -i sa-probe -- sh -s <"$LAB/verify-sa-ns.sh" 2>&1 | tail -1)
    ns_believes=$(printf '%s' "$ns_out" | sed -n 's/.*believes=\[\([^]]*\)\].*/\1/p')
    ns_answers=$(printf '%s' "$ns_out" | sed -n 's/.*answers=\[\([^]]*\)\].*/\1/p')
    if [ -z "$ns_believes" ] || [ -z "$ns_answers" ]; then
      bad "in-pod namespace agreement" "the probe produced no usable answer, so this is reported as failed rather than skipped: $(printf '%s' "$ns_out" | cut -c1-200)"
    elif [ "$ns_believes" = "$ns_answers" ]; then
      ok "a pod's own name for its namespace is the one its objects come back with ($ns_believes)"
    else
      bad "in-pod namespace agreement" "the pod believes it is in '$ns_believes' while its objects say '$ns_answers' -- an operator watching its own namespace builds a cache it can never hit, silently"
    fi
    # And the tenant's spelling specifically, not merely a consistent one: an
    # answer carrying the upstream name would agree with the file and still be
    # the wrong view to hand a tenant's operator.
    if [ "$ns_believes" = "default" ]; then
      ok "and that name is the tenant's own, not the upstream one"
    elif [ -n "$ns_believes" ]; then
      bad "in-pod namespace spelling" "the pod sees '$ns_believes', want 'default' -- the tenant's view is what its operator's code was written against"
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
#
# ⚠️ The claimRef is required now and is incidental here too -- see the volume
# section further down for why a PersistentVolume without one is refused.
expect_allowed "an object of another kind may still be called admin" \
  $T apply -f - <<EOF
apiVersion: v1
kind: PersistentVolume
metadata: {name: admin}
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteOnce]
  claimRef: {namespace: default, name: reserved-name-check}
  nfs: {server: 192.0.2.1, path: /exports/reserved-name-check}
EOF

# ⛔ A PersistentVolume is CLUSTER-SCOPED and the binder never looks at tenancy:
# it matches on access modes, class, size and topology, and only provisions
# dynamically when nothing matched -- so a static volume PRE-EMPTS the
# provisioner. Offered without a claimRef, a tenant's volume is a volume ANY
# tenant's claim can bind to, and whoever binds it mounts storage the offering
# tenant controls.
#
# ⭐ Refusing the class name would not work: a tenant's own claim may only name a
# published class, so its own static volume must carry one too. The legitimate
# use and the attack are the same write. What separates them is the claimRef --
# FindMatchingVolume skips a volume reserved for a different claim.
expect_denied "a PersistentVolume offered to any tenant's claim is refused" "claimRef" -- \
  $T apply -f - <<EOF
apiVersion: v1
kind: PersistentVolume
metadata: {name: unreserved}
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteOnce]
  storageClassName: standard
  nfs: {server: 192.0.2.1, path: /exports/anyone}
EOF

# ...while one reserved for the tenant's own claim is fine, and lands upstream
# pointing at the tenant's namespace rather than the name the tenant wrote.
expect_allowed "while one reserved for its own claim is accepted" \
  $T apply -f - <<EOF
apiVersion: v1
kind: PersistentVolume
metadata: {name: reserved}
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteOnce]
  storageClassName: standard
  claimRef: {namespace: default, name: mine}
  nfs: {server: 192.0.2.1, path: /exports/mine}
EOF
pv_ns=$($K get pv "$TID-reserved" -o jsonpath='{.spec.claimRef.namespace}' 2>/dev/null)
if [ "$pv_ns" = "$NS" ]; then
  ok "and the reservation names the tenant's own namespace upstream"
else
  bad "the reservation is prefixed" "upstream claimRef namespace is '${pv_ns:-<none>}', want '$NS'"
fi
$T delete pv reserved >/dev/null 2>&1

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

# ⭐ And a tenant cannot own unboundedly many. This is a ceiling on an amplifier,
# not a billing control: a cross-namespace list is assembled by reading each of
# the tenant's namespaces in turn -- one upstream request each, in a loop -- so
# every `kubectl get pods` a tenant runs costs as many upstream requests as it
# owns namespaces, against the apiserver every tenant shares.
#
# ⭐ Single-layer by construction: no policy counts anything, so a pass here can
# only have come from kubezoo.
#
# ⚠️ The namespaces made here are deleted immediately afterwards, and the cap is
# set well above what the rest of this file needs. The first version used a tight
# cap and left them behind: three later sections that create a namespace of their
# own failed, in ways that read like product defects -- a projection missing from
# a namespace that was never created, an operator ServiceAccount seeing one
# namespace instead of two. A quota fixture that does not clean up turns every
# later namespace-creating assertion into a hostage.
#
# Counted from where the tenant is now rather than from zero, since the controller
# makes four and the checks above have made more.
nscap_before=$($T get namespaces --no-headers 2>/dev/null | wc -l)
nscap_out=""
nscap_made=""
for i in $(seq 1 $((16 - nscap_before + 1))); do
  nscap_out=$($T create namespace "nscap-$i" 2>&1) || break
  nscap_made="$nscap_made nscap-$i"
done
if grep -q "limit is 16" <<<"$nscap_out"; then
  ok "a tenant cannot own more namespaces than the platform allows"
else
  bad "the namespace cap refuses" "got: $(tr '\n' ' ' <<<"$nscap_out" | cut -c1-160)"
fi

# ⚠️ ...while writing to the ones it already has still works. A cap that also
# froze existing namespaces would leave a tenant over the limit unable to delete
# the workloads inside them, which is the one thing it needs to do to get back
# under it.
if $T label namespace ns-probe-ok probe=still-writable --overwrite >/dev/null 2>&1; then
  ok "and writing to a namespace it already owns still works while it is at the cap"
else
  bad "an owned namespace stays writable at the cap" \
      "$($T label namespace ns-probe-ok probe=x --overwrite 2>&1 | tr '\n' ' ' | cut -c1-160)"
fi
# ⛔ Deleted before anything else runs. See the note above: leaving them costs
# three later assertions, and the failures do not point here.
for ns in $nscap_made; do $T delete namespace "$ns" --wait=false >/dev/null 2>&1; done
for _ in $(seq 30); do
  [ "$($T get namespaces --no-headers 2>/dev/null | wc -l)" -le "$nscap_before" ] && break
  sleep 2
done

for _ in $(seq 30); do
  [ "$($T get namespaces --no-headers 2>/dev/null | wc -l)" -le "$nscap_before" ] && break
  sleep 2
done

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
  # ⚠️ Retried, and it has to be THIS read that is retried rather than the
  # `auth can-i` above. That loop asks the UPSTREAM authorizer with --as; this
  # goes through kubezoo with a ServiceAccount token, which authenticates here
  # and is impersonated onward. Two different paths with two different caches --
  # so the first answering yes says nothing about the second, and the assertion
  # failed intermittently while its supposed wait had already passed.
  #
  # ⭐ It is a positive assertion about a grant that arrives asynchronously, so
  # the only sound probe is the read itself.
  seen=0
  for _ in $(seq 20); do
    seen=$(kubectl --kubeconfig "$CRBKC" get secrets -A --no-headers 2>/dev/null | grep -c crb-probe)
    [ "$seen" -ge 2 ] && break
    sleep 2
  done
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
echo "== a tenant's CLUSTER-SCOPED custom resources are reachable by its own workloads =="
# ⛔ The projection is one RoleBinding per namespace, and a RoleBinding can never
# carry a cluster-scoped grant. So an operator needing its OWN cluster-scoped
# custom resource -- ClusterIssuer, ClusterPolicy, ClusterSecretStore -- installs
# cleanly and then cannot run. cert-manager stops exactly there.
#
# ⭐ What makes a REAL cluster-wide binding safe here is not filtering, it is the
# group: `<tid>-something.io` can only ever hold that tenant's objects, because
# pkg/convert rewrites spec.group on the way in and refuses any other. So there
# is nothing to keep being right later -- the other tenants' objects are not in
# that group at all.
$T apply -f - >/dev/null 2>&1 <<EOF
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: clusterwidgets.cw.example}
spec:
  group: cw.example
  names: {plural: clusterwidgets, singular: clusterwidget, kind: ClusterWidget}
  scope: Cluster
  versions: [{name: v1, served: true, storage: true, schema: {openAPIV3Schema: {type: object}}}]
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: cs-op}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: cs-op}
rules:
- apiGroups: ["cw.example"]
  resources: ["clusterwidgets"]
  verbs: ["get", "list", "watch"]
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: cs-op}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: cs-op}
subjects: [{kind: ServiceAccount, name: cs-op, namespace: default}]
EOF
# ⚠️ Retried: the derivation is a controller reconcile, not part of the write.
# That is deliberate -- it needs a privilege the tenant does not have, so it
# cannot happen on the request path -- but it means it arrives afterwards.
derived=0
for _ in $(seq 20); do
  derived=$($K get clusterrolebinding -l kubezoo.io/clusterscoped=true --no-headers 2>/dev/null | grep -c "cs-op" || true)
  [ "${derived:-0}" -ge 1 ] && break
  sleep 3
done
if [ "${derived:-0}" -ge 1 ]; then
  ok "a real cluster-wide binding is derived for the tenant's own API group"
else
  # ⛔ Carry the evidence into the message. The derivation reads a record, then
  # its ClusterRole, then filters the rules -- three places it can come up
  # empty, and "none found" says which of them only by accident. The controller
  # logs at V(4) and the lab runs it at default verbosity, so its own account of
  # this is invisible.
  bad "a real cluster-wide binding is derived for the tenant's own API group" \
      "record=[$($K -n "$TID-kube-system" get rolebindings -l kubezoo.io/clusterrolebinding=true \
         -o jsonpath='{range .items[*]}{.metadata.name}->{.roleRef.kind}/{.roleRef.name} {end}' 2>&1 | cut -c1-120)] \
role-rules=[$($K get clusterrole "$TID-cs-op" -o jsonpath='{.rules[*].apiGroups[*]}' 2>&1 | cut -c1-80)]"
fi

# ⭐⭐ THE SECURITY ASSERTION. The role above also grants secrets in the CORE
# group. If that rule were derived too, this tenant's ServiceAccount would read
# every secret in the cluster -- every other tenant's included. The filter must
# keep the first rule and drop the second.
derived_role=$($K get clusterrole -l kubezoo.io/clusterscoped=true -o name 2>/dev/null | grep cs-op | head -1)
if [ -z "$derived_role" ]; then
  bad "only the tenant's own API groups are derived" "no derived clusterrole to inspect"
else
  derived_groups=$($K get "$derived_role" -o jsonpath='{.rules[*].apiGroups[*]}' 2>/dev/null)
  if grep -qE "(^| )$TID-cw\.example( |$)" <<<"$derived_groups" && \
     ! grep -qE '(^| )("" ?|\*)( |$)' <<<" $derived_groups "; then
    ok "and ONLY the tenant's own API groups are in it, not the core group it also asked for"
  else
    bad "only the tenant's own API groups are derived" \
        "the derived role grants [$derived_groups] cluster-wide -- anything but $TID-* reaches other tenants"
  fi
fi

# And it actually works: the SA reads its own cluster-scoped CR through kubezoo.
CSTOK=$($K -n "$NS" create token cs-op --duration=10m 2>/dev/null)
if [ -n "$CSTOK" ]; then
  CSKC=$LAB/verify-cs-sa.kubeconfig; rm -f "$CSKC"
  kubectl --kubeconfig "$CSKC" config set-cluster zoo --certificate-authority=$PKI/ca.pem \
    --embed-certs=true --server=https://127.0.0.1:6443 >/dev/null
  kubectl --kubeconfig "$CSKC" config set-credentials sa --token="$CSTOK" >/dev/null
  kubectl --kubeconfig "$CSKC" config set-context c --cluster=zoo --user=sa >/dev/null
  kubectl --kubeconfig "$CSKC" config use-context c >/dev/null
  # Retried: the grant arrives through the upstream authorizer, which lags.
  cs_ok=no
  for _ in $(seq 20); do
    kubectl --kubeconfig "$CSKC" get clusterwidgets >/dev/null 2>&1 && { cs_ok=yes; break; }
    sleep 2
  done
  if [ "$cs_ok" = yes ]; then
    ok "the tenant's ServiceAccount can list its own cluster-scoped custom resource"
  else
    bad "the tenant's ServiceAccount can list its own cluster-scoped custom resource" \
        "$(kubectl --kubeconfig "$CSKC" get clusterwidgets 2>&1 | head -1 | cut -c1-140)"
  fi
  rm -f "$CSKC"
fi

# ⭐⭐ The other half of the same grant, and it is asked of the UPSTREAM
# authorizer at CLUSTER scope rather than through kubezoo. That distinction is
# the assertion: `get secrets -A` through kubezoo means "every namespace of
# MINE", which the projection grants and should -- the first version of this
# asked that and failed on a tenant behaving exactly as intended. What must not
# exist is a grant with no namespace at all, because that one reaches every
# other tenant.
cs_sa="system:serviceaccount:$TID-default:cs-op"
if [ "$($K auth can-i list secrets --as="$cs_sa" 2>/dev/null)" = no ]; then
  ok "and the same ServiceAccount holds no cluster-wide secrets, which the role also asked for"
else
  bad "the derived grant is confined to the tenant's own API groups" \
      "$cs_sa can list secrets at cluster scope -- the core-group rule was derived too, so every tenant's secrets are readable"
fi
# ⚠️ And the positive control for that probe: the same SA must still hold the
# grant it is supposed to have, at cluster scope, on the tenant's own group.
# Without this, `can-i` answering no to everything would read as success.
if [ "$($K auth can-i list clusterwidgets.$TID-cw.example --as="$cs_sa" 2>/dev/null)" = yes ]; then
  ok "while it does hold the cluster-wide grant on its own group"
else
  bad "the derived grant reaches the tenant's own group at cluster scope" \
      "$cs_sa cannot list its own cluster-scoped custom resource upstream, so the derivation granted nothing"
fi

$T delete clusterrolebinding cs-op >/dev/null 2>&1
gone=no
for _ in $(seq 15); do
  [ "$($K get clusterrolebinding -l kubezoo.io/clusterscoped=true --no-headers 2>/dev/null | grep -c cs-op || true)" = 0 ] \
    && { gone=yes; break; }
  sleep 2
done
if [ "$gone" = yes ]; then
  ok "deleting the binding withdraws the derived cluster-wide grant too"
else
  bad "deleting the binding withdraws the derived cluster-wide grant too" \
      "the derived binding is still there, so the tenant cannot take back a grant it made"
fi
$T delete clusterrole cs-op >/dev/null 2>&1
$T delete crd clusterwidgets.cw.example >/dev/null 2>&1

echo
echo "== a tenant's WORKLOAD reaches kubezoo, and sees the tenant's view =="
# ⛔⛔ THE PATH THIS PLATFORM SELLS, and there WAS already a probe for it that
# covered neither of the two things that were broken. The sa-probe section above
# runs curl inside a Pod and writes a custom resource through kubezoo -- it looks
# like this path is covered. It is not:
#
#   curl -sk  https://$ZOO_HOST:6443/...
#         ^^              ^
#    skips the cert    an address the LAB computes
#
# so it exercises neither the certificate a Pod is given nor the address the
# policy injects. A real workload has no choice about either: it validates with
# the upstream cluster's CA bundle and connects to KUBERNETES_SERVICE_HOST.
#
# ⚠️ Both were broken for the entire life of this lab -- the address placeholder
# was never substituted, and the serving certificate was signed by kubezoo's own
# CA rather than the one Pods carry -- and every assertion here stayed green. It
# took installing cert-manager for real to find it.
#
# ⭐ So this probe deliberately does neither: no -k, and the address comes from
# the environment rather than from this script.
#
# ⭐ The response is what proves which server answered: through kubezoo the
# tenant's namespace is called `default`, and upstream it is `909090-default`.
# Reachability alone would pass against the wrong one.
$T -n default apply -f - >/dev/null 2>&1 <<EOF
apiVersion: v1
kind: ServiceAccount
metadata: {name: pod-probe}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: pod-probe}
# ⚠️ get, by name. A LIST answers with whatever else is in the namespace first
# and the marker fell past the probe's own truncation -- the request has to be
# for the one object the assertion is about.
rules: [{apiGroups: [""], resources: ["configmaps"], verbs: ["get"]}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: pod-probe}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: pod-probe}
subjects: [{kind: ServiceAccount, name: pod-probe, namespace: default}]
EOF
$T -n default create configmap pod-probe-marker --from-literal=a=b >/dev/null 2>&1
$T apply -f - >/dev/null 2>&1 <<EOF
apiVersion: v1
kind: Pod
metadata: {name: api-probe}
spec:
  serviceAccountName: pod-probe
  restartPolicy: Never
  containers:
  - name: c
    image: curlimages/curl:8.11.1
    command: ["sh","-c"]
    args:
    - |
      curl -sS --max-time 20 --cacert /var/run/secrets/kubernetes.io/serviceaccount/ca.crt -H "Authorization: Bearer \$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)" "https://\$KUBERNETES_SERVICE_HOST:\$KUBERNETES_SERVICE_PORT/api/v1/namespaces/default/configmaps/pod-probe-marker" 2>&1 | head -c 600
      echo
      sleep 240
    securityContext:
      privileged: false
      allowPrivilegeEscalation: false
      runAsNonRoot: true
      runAsUser: 1000
      capabilities: {drop: ["ALL"]}
      seccompProfile: {type: RuntimeDefault}
EOF
probe_out=""
for _ in $(seq 40); do
  # ⚠️ Read through $K, upstream. What is asserted is what the POD saw; reading
  # it through kubezoo would drag the log subresource into a check that is not
  # about it, and a failure there would report as this failing.
  probe_out=$($K -n "$NS" logs api-probe 2>/dev/null)
  [ -n "$probe_out" ] && break
  sleep 3
done
# ⛔ The raw response, unfiltered. The first version piped it through
# `grep '"name"'`, so an authorization failure -- which is a Status object with
# no such field -- arrived as an EMPTY string and reported as "no output after
# 120s". The evidence was in hand and thrown away by the probe itself.
if [ -z "$probe_out" ]; then
  bad "a tenant Pod reaches kubezoo at all" \
      "nothing after 120s. pod=$($K -n "$NS" get pod api-probe -o jsonpath='{.status.phase}' 2>&1) \
host=$($K -n "$NS" get pod api-probe -o jsonpath='{.spec.containers[0].env[?(@.name==\"KUBERNETES_SERVICE_HOST\")].value}' 2>&1)"
elif grep -q "KUBEZOO_ADDRESS_PLACEHOLDER" <<<"$probe_out"; then
  bad "a tenant Pod reaches kubezoo at all" \
      "the address placeholder was never substituted, so no tenant workload can reach any API server"
elif grep -qiE "certificate|x509|SSL" <<<"$probe_out"; then
  bad "a tenant Pod trusts the certificate kubezoo serves it" \
      "$(tr '\n' ' ' <<<"$probe_out" | cut -c1-160) -- a Pod validates with the UPSTREAM CA, so kubezoo needs an SNI certificate signed by it"
elif ! grep -q "pod-probe-marker" <<<"$probe_out"; then
  bad "a tenant Pod is answered by kubezoo" \
      "$(tr '\n' ' ' <<<"$probe_out" | cut -c1-200)"
else
  ok "a tenant Pod reaches kubezoo with its projected token and the CA it was given"
  # ⭐ WHICH server answered. Through kubezoo the object's namespace is the
  # tenant's own name for it; upstream it carries the tenant prefix. Reaching
  # something and reaching the RIGHT something are different claims.
  if ! grep -q "$TID-default" <<<"$probe_out"; then
    ok "and answered in the tenant's own terms, so it reached kubezoo and not the upstream"
  else
    bad "a tenant Pod is answered in the tenant's own terms" \
        "the response names $TID-default, which is the upstream's name for it"
  fi
fi
$T -n default delete pod api-probe --force --grace-period=0 >/dev/null 2>&1
$T -n default delete configmap pod-probe-marker >/dev/null 2>&1
$T -n default delete serviceaccount pod-probe >/dev/null 2>&1

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
  - apiGroups: ["admissionregistration.k8s.io"]
    resources: ["validatingwebhookconfigurations"]
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
  # ⭐⭐ The NATIVE half (task #99). The same role also asks for
  # validatingwebhookconfigurations -- a resource that is not the tenant's by
  # name, so nothing about the object says who may see it. cainjector needs
  # exactly this and cannot get it any other way: resourceNames does not apply to
  # list or watch.
  #
  # ⚠️ Asked THROUGH KUBEZOO, unlike everything above. The grant lives on a group
  # kubezoo adds when it forwards, so `--as=<the ServiceAccount>` -- which is how
  # the assertions above ask -- deliberately does NOT see it. That is the design:
  # an identity reaching upstream by any other route carries nothing.
  SATOK=$($K -n "$NS" create token own-op --duration=10m 2>/dev/null)
  if [ -n "$SATOK" ]; then
    SAKC=$LAB/verify-own-sa.kubeconfig; rm -f "$SAKC"
    kubectl --kubeconfig "$SAKC" config set-cluster zoo --certificate-authority=$PKI/ca.pem \
      --embed-certs=true --server=https://127.0.0.1:6443 >/dev/null
    kubectl --kubeconfig "$SAKC" config set-credentials sa --token="$SATOK" >/dev/null
    kubectl --kubeconfig "$SAKC" config set-context c --cluster=zoo --user=sa >/dev/null
    kubectl --kubeconfig "$SAKC" config use-context c >/dev/null

    # A webhook configuration of the tenant's own, and one of the PLATFORM's.
    # The platform's carries no tenant prefix, which is the only thing that
    # decides -- so it is the control for every claim below.
    $T apply -f - >/dev/null 2>&1 <<EOF
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata: {name: sa-visible}
webhooks: []
EOF
    $K apply -f - >/dev/null 2>&1 <<EOF
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata: {name: platform-only-probe}
webhooks: []
EOF
    listed=""
    for _ in $(seq 30); do
      listed=$(kubectl --kubeconfig "$SAKC" get validatingwebhookconfigurations --no-headers 2>&1)
      grep -q "sa-visible" <<<"$listed" && break
      sleep 2
    done
    sa_native=no
    if grep -q "sa-visible" <<<"$listed"; then
      sa_native=yes
      ok "the ServiceAccount can list a NATIVE cluster-scoped resource through kubezoo"
    else
      # ⛔ Carry the evidence. The grant is written by the controller, from the
      # intersection of the role and what the tenant may hold, onto a group
      # kubezoo asserts -- three places it can come up empty, and the Forbidden
      # alone says which only by accident.
      bad "the ServiceAccount can list a native cluster-scoped resource through kubezoo" \
          "$(tr '\n' ' ' <<<"$listed" | cut -c1-120) | derived=[$($K get clusterrolebinding \
             -l kubezoo.io/clusterscoped=true -o jsonpath='{range .items[*]}{.metadata.name}->{.subjects[*].name} {end}' 2>&1 | cut -c1-160)]"
    fi
    # ⭐ The assertion that matters. Listing cannot be narrowed by name in RBAC,
    # so upstream returns every webhook configuration there is; what the tenant
    # sees is decided by kubezoo filtering on the way back.
    # ⚠️ Gated on the list having WORKED. Both assertions below pass for free
    # when it did not: an error message contains neither name, and a delete is
    # refused along with everything else. The first version reported two passes
    # on a run where nothing was granted at all.
    if [ "$sa_native" = yes ]; then
    if grep -q "platform-only-probe" <<<"$listed"; then
      bad "and sees only its own" \
          "the platform's own webhook configuration is visible to a tenant workload"
    else
      ok "and sees only its own, not the platform's"
    fi
    # ⚠️ Reads only. The same role asked for get/list/watch and nothing more, but
    # the intersection is what decides -- a write must still be refused.
    if kubectl --kubeconfig "$SAKC" delete validatingwebhookconfiguration sa-visible >/dev/null 2>&1; then
      bad "and holds reads only" "it deleted a webhook configuration; the derived grant carried a write"
    else
      ok "and holds reads only, not the writes it did not ask for"
    fi
    fi
    $K delete validatingwebhookconfiguration platform-only-probe >/dev/null 2>&1
    $T delete validatingwebhookconfiguration sa-visible >/dev/null 2>&1
    rm -f "$SAKC"
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

# ⛔ spec.tls[].hosts is a SECOND list of host names, and the hostname policy did
# not mention tls at all -- it checked spec.rules[].host and stopped there. That
# list decides which names the ingress controller presents a certificate for, so
# leaving it open let a tenant name a domain it does not own: the controller
# answers the TLS handshake for it, and where cert-manager is wired in a
# certificate may be requested for it.
#
# ⭐ Single-layer: nothing else looks at tls hosts, so a pass here is the policy.
expect_denied "an Ingress naming a foreign domain in spec.tls is refused" "tenant-ingress-hostnames" -- \
  $T -n default apply -f - <<EOF
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: tls-foreign}
spec:
  tls: [{hosts: ["shop.someone-else.com"], secretName: s}]
  rules:
    - host: ok.$TID.apps.example.com
      http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: s, port: {number: 80}}}}]}
EOF

# ⚠️ ...while the ordinary shapes stay legal. An empty tls list is plain HTTP and
# a tls entry without hosts means "use the default certificate"; requiring hosts
# would refuse most Ingresses ever written.
expect_allowed "while its own subdomain in spec.tls is accepted" \
  $T -n default apply -f - <<EOF
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: tls-own}
spec:
  tls: [{hosts: ["ok.$TID.apps.example.com"], secretName: s}, {secretName: default-cert}]
  rules:
    - host: ok.$TID.apps.example.com
      http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: s, port: {number: 80}}}}]}
EOF

# ⚠️ And an Ingress with no rules at all, which is legal -- defaultBackend only.
# The existing expression reads object.spec.rules with no has() guard, and this
# policy runs with failurePolicy: Fail, so a CEL error on a missing field is a
# denial. Whether that is what happens is measured here rather than assumed.
expect_allowed "and an Ingress with only a defaultBackend, which names no host at all" \
  $T -n default apply -f - <<EOF
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: default-backend-only}
spec:
  ingressClassName: nginx
  defaultBackend: {service: {name: s, port: {number: 80}}}
EOF
$T -n default delete ingress tls-own default-backend-only >/dev/null 2>&1

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

# ⛔ A THIRD list of host names, and it is not in the spec at all. The
# server-alias annotation goes into the SAME nginx server_name as
# spec.rules[].host -- upstream rootfs/etc/nginx/template/nginx.tmpl:639:
#   server_name {{ buildServerName $server.Hostname }} {{ range $server.Aliases }}...
# so everything the two rules above refuse could be taken straight back with one
# annotation. This is not "annotations are unfiltered" in general; it is a bypass
# of THIS policy, which is why it is asserted here rather than with the others.
ingress_with_alias() {
  cat <<EOF
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: $1
  annotations: {nginx.ingress.kubernetes.io/server-alias: "$2"}
spec:
  ingressClassName: nginx
  rules:
    - host: shop.$TID.apps.example.com
      http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: s, port: {number: 80}}}}]}
EOF
}
# ⭐ The positive control comes first: an alias under the tenant's own subdomain
# is ordinary use, and without it the three refusals below would all pass just as
# well if the annotation were banned outright -- a different, worse policy that
# this section could not tell apart.
expect_allowed "an alias under the tenant's own subdomain" \
  $T -n default apply -f <(ingress_with_alias alias-own "www.$TID.apps.example.com")
expect_denied "an alias naming a foreign domain" "server-alias" -- \
  $T -n default apply -f <(ingress_with_alias alias-foreign bank.example.com)
# ⚠️ The value is a COMMA-SEPARATED list (upstream ValidateArrayOfServerName).
# A check written against the whole string would accept this one: only the LAST
# entry is legal.
expect_denied "and one hidden in a comma-separated list" "server-alias" -- \
  $T -n default apply -f <(ingress_with_alias alias-list "bank.example.com, www.$TID.apps.example.com")
# ⚠️ Upstream accepts regexes here, and an nginx regex server_name is not
# implicitly anchored -- so "ends with the tenant's suffix" does not mean the
# same thing for a regex as it does for a name. This one ends with the suffix
# exactly and still has to be refused.
expect_denied "and a regex alias, even one ending in the tenant's own suffix" "server-alias" -- \
  $T -n default apply -f <(ingress_with_alias alias-regex "~.$TID.apps.example.com")
$T -n default delete ingress alias-own >/dev/null 2>&1

echo
echo "== the ingress controller's high-risk annotations belong to the platform =="
# Annotations are the ingress controller's main configuration surface, and they
# are interpreted BY THE PLATFORM'S OWN CONTROLLER, in one nginx.conf shared with
# every other tenant. kubezoo passes them through untouched -- pkg/convert/ingress.go
# rewrites the class and nothing else -- so the refusal is a policy, and its list
# is upstream's own Risk: Critical/High classification rather than one this
# repository invented.
ingress_annotated() {
  cat <<EOF
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: $1
  annotations: {"$2": "$3"}
spec:
  ingressClassName: nginx
  rules:
    - host: shop.$TID.apps.example.com
      http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: s, port: {number: 80}}}}]}
EOF
}
# ⭐ Positive control first, for the same reason as above: a section that refused
# every annotation would pass every check below it.
expect_allowed "an ordinary annotation the tenant is meant to have" \
  $T -n default apply -f <(ingress_annotated ann-ok nginx.ingress.kubernetes.io/rewrite-target /)
expect_denied "a configuration-snippet, which is raw nginx config" "reserved to the platform" -- \
  $T -n default apply -f <(ingress_annotated ann-snippet nginx.ingress.kubernetes.io/configuration-snippet "more_set_headers 'x: y';")
# auth-url makes the controller open a connection of the tenant's choosing from
# the platform's own network namespace -- the same shape as the ExternalName and
# probe-host findings, with a different component doing the dialling.
expect_denied "an auth-url, which the platform's controller would dial" "reserved to the platform" -- \
  $T -n default apply -f <(ingress_annotated ann-auth nginx.ingress.kubernetes.io/auth-url "http://169.254.169.254/latest/meta-data/")
expect_denied "and a mirror-target, which copies traffic anywhere" "reserved to the platform" -- \
  $T -n default apply -f <(ingress_annotated ann-mirror nginx.ingress.kubernetes.io/mirror-target "http://elsewhere.example.com/collect")
# Updates as well as creates: an Ingress accepted without the annotation must not
# be able to grow one afterwards.
expect_denied "and adding one to an Ingress that was already accepted" "reserved to the platform" -- \
  $T -n default patch ingress ann-ok --type=merge \
    -p '{"metadata":{"annotations":{"nginx.ingress.kubernetes.io/auth-url":"http://10.0.0.1/"}}}'
$T -n default delete ingress ann-ok >/dev/null 2>&1

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
echo "== tenant-wide quota =="
# The chain under test has four components and no two of them are in the same
# repository or process:
#
#   Tenant.spec.quota  --kubezoo-controller-->  ClusterResourceQuota (upstream)
#     --quota reconciler-->  a ResourceQuota in EVERY tenant namespace, each
#     carrying the FULL allowance, plus CRQ.status.used summed across them
#     --admission webhook-->  pod CREATE refused
#
# ⭐ The aggregate lives in the webhook, not in the per-namespace objects. Each
# namespace's ResourceQuota holds the whole allowance, so upstream's own quota
# admission alone would let a tenant have the allowance N times over, once per
# namespace. What makes it tenant-wide is webhook.go substituting the cluster
# quota's summed status for the namespace's own before evaluating.
#
# That is why the assertion below spends its budget in a SECOND namespace: a
# test that fills one namespace and stops proves only what upstream already
# does. Crossing the namespace boundary is the whole claim, and it is its own
# negative control -- if the substitution were dropped, the second namespace
# would admit, because its own used is zero.
QTID=909091
QNS_A=$QTID-default
QNS_B=$QTID-second
QKC=$LAB/verify-$QTID.kubeconfig
# ⚠️ Through the zoo, not through $K. Tenants are served by kubezoo out of its
# own etcd and are not an upstream resource at all, so deleting one with the
# upstream client is a no-op that says nothing, and the wait below then spends
# its entire budget watching namespaces nobody has asked to go away. The tenant
# setup at the top of this file has the same shape and the same problem; it gets
# away with it because up.sh wipes kubezoo's etcd on every run.
kubectl --kubeconfig "$ZOOKC" delete tenant "$QTID" >/dev/null 2>&1
for _ in $(seq 40); do
  [ "$($K get ns -l "kubezoo.io/tenant=$QTID" --no-headers 2>/dev/null | wc -l)" = 0 ] && break
  sleep 3
done
kubectl --kubeconfig "$ZOOKC" create -f - >/dev/null 2>&1 <<EOF
apiVersion: tenant.kubezoo.io/v1alpha1
kind: Tenant
metadata: {name: "$QTID"}
spec: {quota: {hard: {pods: "2"}}}
EOF
cat >"$LAB/verify-q-csr.json" <<EOF
{"CN":"$QTID-admin","key":{"algo":"rsa","size":2048},"names":[{"OU":"$QTID"}]}
EOF
cfssl gencert -ca=$PKI/ca.pem -ca-key=$PKI/ca-key.pem -config=$PKI/ca-config.json \
  -profile=kubernetes "$LAB/verify-q-csr.json" 2>/dev/null | cfssljson -bare "$LAB/verify-$QTID"
kubectl --kubeconfig "$QKC" config set-cluster zoo --certificate-authority=$PKI/ca.pem \
  --embed-certs=true --server=https://127.0.0.1:6443 >/dev/null
kubectl --kubeconfig "$QKC" config set-credentials t --client-certificate="$LAB/verify-$QTID.pem" \
  --client-key="$LAB/verify-$QTID-key.pem" --embed-certs=true >/dev/null
kubectl --kubeconfig "$QKC" config set-context t --cluster=zoo --user=t >/dev/null
kubectl --kubeconfig "$QKC" config use-context t >/dev/null
Q="kubectl --kubeconfig $QKC"

# ⚠️ Every step below reports its own failure, and none of them is allowed to
# fail quietly. The first version silenced all three, and when the second
# namespace did not come up the run reported two assertion failures -- a missing
# ResourceQuota and a pod refused by RBAC instead of by quota -- neither of which
# named the step that had actually broken. A setup step that fails silently does
# not produce one clear failure, it produces several misleading ones.
quota_setup_ok=1
for _ in $(seq 30); do
  [ "$($K get ns "$QNS_A" -o jsonpath='{.status.phase}' 2>/dev/null)" = Active ] && break
  sleep 3
done
if [ "$($K get ns "$QNS_A" -o jsonpath='{.status.phase}' 2>/dev/null)" != Active ]; then
  bad "quota fixture: the tenant's first namespace comes up" \
    "$QNS_A is $($K get ns "$QNS_A" -o jsonpath='{.status.phase}' 2>&1 | head -c 80) after 90s -- if it is Terminating, the previous run's namespaces had not finished going away"
  quota_setup_ok=0
fi
# ⚠️ Retried, because the tenant's namespace being Active does NOT mean the
# tenant may yet create another one. The two things arrive from different
# places: the namespace from kubezoo-controller, the cluster-scope permission
# from the RBAC this file has been churning all run. The upstream authorizer
# serves stale answers while its cache catches up -- the same trap the header
# records for the namespace-cap section, and this section sits after all of that
# churn by design, because a quota fixture has to go last.
#
# The symptom was a Forbidden on `create namespace` that looked like a genuine
# authorization result and was not.
if [ "$quota_setup_ok" = 1 ]; then
  ns_out=""
  for _ in $(seq 20); do
    ns_out=$($Q create ns second 2>&1) && break
    sleep 3
  done
  if ! $Q get ns second >/dev/null 2>&1; then
    bad "quota fixture: the tenant can create a second namespace" \
      "still failing after 60s: $(tr '\n' ' ' <<<"$ns_out" | cut -c1-160)"
    quota_setup_ok=0
  fi
fi
if [ "$quota_setup_ok" = 1 ]; then
  for _ in $(seq 20); do
    [ "$($K get ns "$QNS_B" -o jsonpath='{.status.phase}' 2>/dev/null)" = Active ] && break
    sleep 3
  done
  if [ "$($K get ns "$QNS_B" -o jsonpath='{.status.phase}' 2>/dev/null)" != Active ]; then
    bad "quota fixture: the tenant's second namespace comes up" "$QNS_B never became Active"
    quota_setup_ok=0
  fi
fi

# The tenant controller decides ONCE, at startup, whether the upstream cluster
# serves clusterresourcequotas, and keeps a nil client forever if it does not.
# So a missing CRQ here does not mean the reconciler is broken -- it usually
# means the quota component started after the controller. Say so, because the
# symptom is otherwise indistinguishable from the feature not working.
if [ "$quota_setup_ok" = 1 ]; then
  crq=$(for _ in $(seq 20); do
    n=$($K get clusterresourcequota -o name 2>/dev/null | grep "$QTID" | head -1)
    [ -n "$n" ] && { echo "$n"; break; }; sleep 3
  done)
  if [ -z "$crq" ]; then
    bad "the tenant's spec.quota reaches a ClusterResourceQuota" \
      "none appeared for $QTID -- if kubezoo-controller.log says 'does not serve clusterresourcequotas', the quota component started too late"
  else
    ok "the tenant's spec.quota reaches a ClusterResourceQuota"
  fi
  
  derived=0
  for _ in $(seq 20); do
    a=$($K -n "$QNS_A" get resourcequota -l clusterresourcequota.quota.kubezoo.io/autoupdate=true -o name 2>/dev/null | wc -l)
    b=$($K -n "$QNS_B" get resourcequota -l clusterresourcequota.quota.kubezoo.io/autoupdate=true -o name 2>/dev/null | wc -l)
    [ "$a" -ge 1 ] && [ "$b" -ge 1 ] && { derived=1; break; }
    sleep 3
  done
  if [ "$derived" = 1 ]; then
    ok "a labelled ResourceQuota is derived into every tenant namespace"
  else
    bad "a labelled ResourceQuota is derived into every tenant namespace" "got $a in $QNS_A, $b in $QNS_B"
  fi
  
  # ⭐ Prove the second namespace admits a pod BEFORE the quota is spent, and do
  # it with the same spec that will be refused later. Two things at once:
  #
  #  - It is the before half of a before/after control. Without it, the refusal
  #    at the end is just "a pod was refused in a namespace", and the run that
  #    produced this comment showed exactly how that goes wrong -- the pod was
  #    refused by RBAC, not by quota, and only the policy-name check caught it.
  #  - It waits out that RBAC. A tenant's access to a namespace it has just
  #    created does not arrive with the namespace; the RoleBinding inside it
  #    lands separately, and the authorizer lags further still.
  probe_out=""
  for _ in $(seq 20); do
    probe_out=$($Q -n second create -f <(pod qprobe '') 2>&1) && break
    sleep 3
  done
  if $Q -n second get pod qprobe >/dev/null 2>&1; then
    ok "the tenant's second namespace admits this pod while quota is free"
    # Out of the way before the quota is counted -- otherwise it is a third pod.
    $Q -n second delete pod qprobe --force --grace-period=0 >/dev/null 2>&1
  else
    bad "the tenant's second namespace admits this pod while quota is free" \
      "$(tr '\n' ' ' <<<"$probe_out" | cut -c1-160)"
  fi

  expect_allowed "the tenant may create pods up to its quota (1/2)" \
    $Q -n default create -f <(pod qpod1 '')
  expect_allowed "the tenant may create pods up to its quota (2/2)" \
    $Q -n default create -f <(pod qpod2 '')
  
  # Poll the SUMMED usage, not the pods: the number the webhook reads is written
  # by two independent loops -- upstream's quota controller fills in the namespace
  # object's status.used, and only then does the reconciler sum it into the
  # cluster quota. Creating the third pod before that lands would measure nothing.
  used=""
  for _ in $(seq 30); do
    used=$($K get "$crq" -o jsonpath='{.status.used.pods}' 2>/dev/null)
    [ "$used" = 2 ] && break
    sleep 3
  done
  if [ "$used" = 2 ]; then
    ok "usage is summed across the tenant's namespaces into the cluster quota"
  else
    bad "usage is summed across the tenant's namespaces into the cluster quota" "status.used.pods=${used:-<empty>} after 90s"
  fi
  
  # ⭐ The claim. Namespace B has consumed nothing of its own.
  #
  # Matched on "exceeded quota" rather than accepting any refusal, and that is not
  # pedantry here: namespace B is freshly created, and a pod in it can be refused
  # by placement or by PSA for reasons having nothing to do with quota, which
  # would leave this assertion passing while proving nothing.
  expect_denied "a third pod is refused in a DIFFERENT namespace, on the tenant-wide total" \
    "exceeded quota" -- $Q -n second create -f <(pod qpod3 '')
  
  # Both halves of #97, now against a quota system that is actually running. The
  # unit tests drive the guard and the reconciler directly; these say the objects
  # they act on are the objects that exist here.
  qname=$($K -n "$QNS_A" get resourcequota -l clusterresourcequota.quota.kubezoo.io/autoupdate=true -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
  expect_denied "the tenant may not edit the ResourceQuota derived from its quota" \
    "maintained by the platform" -- \
    $Q -n default patch resourcequota "$qname" --type=merge -p '{"spec":{"hard":{"pods":"99"}}}'
  
  # ⚠️ The strip has to be confirmed, not assumed. An empty $qname, or any other
  # reason the label command does not land, leaves the label sitting at "true"
  # from beginning to end -- and the loop below then reports a restoration that
  # never happened. This assertion is only about the reconciler if something
  # actually removed the label first.
  if ! $K -n "$QNS_A" label resourcequota "$qname" \
       clusterresourcequota.quota.kubezoo.io/autoupdate- >/dev/null 2>&1; then
    bad "a stripped autoupdate label is restored by the reconciler" \
      "could not strip the label to begin with (resourcequota name was '${qname:-<empty>}')"
    qname=""
  fi
  if [ -n "$qname" ]; then
    restored=""
    for _ in $(seq 20); do
      restored=$($K -n "$QNS_A" get resourcequota "$qname" \
        -o jsonpath='{.metadata.labels.clusterresourcequota\.quota\.kubezoo\.io/autoupdate}' 2>/dev/null)
      [ "$restored" = true ] && break
      sleep 3
    done
    if [ "$restored" = true ]; then
      ok "a stripped autoupdate label is restored by the reconciler"
    else
      bad "a stripped autoupdate label is restored by the reconciler" "label is ${restored:-<gone>} after 60s"
    fi
  fi

fi
kubectl --kubeconfig "$ZOOKC" delete tenant "$QTID" >/dev/null 2>&1
rm -f "$LAB/verify-$QTID"*.pem "$LAB/verify-q-csr.json" "$QKC"

echo
echo "== a NetworkPolicy peer cannot reach past the tenant =="
# ⛔ A peer selects namespaces by LABEL, cluster-wide, and nothing translated it
# -- not kubezoo, not the policies, not kubetron (checked: zero references in all
# three). So `namespaceSelector: {}` meant every namespace IN THE CLUSTER, and a
# tenant narrowing ingress to "my namespaces" was in fact opening its pods to
# every other tenant, with nothing to say so: the policy was accepted and did
# exactly what it literally said.
#
# ⭐ Single-layer by construction, so a pass here can only be kubezoo.
$T apply -f - >/dev/null 2>&1 <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: np-probe}
spec:
  podSelector: {}
  ingress:
    - from:
        - namespaceSelector: {}
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: default}}
EOF
# Read the STORED object, which is what actually gets enforced.
np_all=$($K -n "$NS" get networkpolicy np-probe \
  -o jsonpath='{.spec.ingress[0].from[0].namespaceSelector.matchLabels.kubezoo\.io/tenant}' 2>/dev/null)
if [ "$np_all" = "$TID" ]; then
  ok "an empty namespaceSelector is confined to the tenant's own namespaces"
else
  bad "an empty namespaceSelector is confined" \
      "the stored selector carries tenant='${np_all:-<none>}', so it still reaches every namespace in the cluster"
fi

np_named=$($K -n "$NS" get networkpolicy np-probe \
  -o jsonpath='{.spec.ingress[0].from[1].namespaceSelector.matchLabels.kubernetes\.io/metadata\.name}' 2>/dev/null)
if [ "$np_named" = "$NS" ]; then
  ok "and naming its own namespace reaches its own, not the platform's of that name"
else
  bad "a named namespace is prefixed" "the stored selector names '${np_named:-<none>}', want '$NS'"
fi

# ⚠️ And the tenant reads back what it wrote, or its next apply re-sends the
# selector, kubezoo confines it again, and the object it sees never matches the
# object it wrote.
np_back=$($T get networkpolicy np-probe \
  -o jsonpath='{.spec.ingress[0].from[1].namespaceSelector.matchLabels.kubernetes\.io/metadata\.name}' 2>/dev/null)
np_leak=$($T get networkpolicy np-probe -o json 2>/dev/null | grep -c "kubezoo.io/tenant" || true)
if [ "$np_back" = default ] && [ "$np_leak" = 0 ]; then
  ok "while the tenant reads back exactly the selector it wrote"
else
  bad "the selector round-trips" "tenant sees name='${np_back:-<none>}' (want default), tenant-label leaks=$np_leak (want 0)"
fi
$T delete networkpolicy np-probe >/dev/null 2>&1

echo
echo "== a tenant cannot claim traffic to an address it does not own =="
# ⛔ A Service carrying spec.externalIPs makes the data plane on EVERY node
# intercept traffic to those addresses and deliver it to that Service's
# endpoints, with no check that the writer has any claim to them. A tenant could
# take another tenant's service, the platform's DNS or apiserver, or any address
# outside the cluster. This is CVE-2020-8554.
#
# ⭐ Single-layer: no policy has a rule whose kinds include Service, and
# pkg/convert had no service.go at all, so a pass here can only be kubezoo.
#
# ⚠️ Also what proves the guard is WIRED -- it hangs off three call sites (Create,
# Update, guaranteedUpdate) and the unit test calls none of them.
extip_out=$($T apply -f - 2>&1 <<EOF
apiVersion: v1
kind: Service
metadata: {name: extip-probe}
spec:
  externalIPs: ["10.99.99.99"]
  ports: [{port: 80}]
  selector: {app: nothing}
EOF
)
if grep -q "externalIPs" <<<"$extip_out"; then
  ok "a Service claiming an external IP is refused"
else
  bad "a Service claiming an external IP is refused" \
      "$(tr '\n' ' ' <<<"$extip_out" | cut -c1-160)"
fi

# ...and the same Service without it is fine, so the refusal is about the field
# rather than about Services.
if $T apply -f - >/dev/null 2>&1 <<EOF
apiVersion: v1
kind: Service
metadata: {name: extip-probe}
spec:
  ports: [{port: 80}]
  selector: {app: nothing}
EOF
then
  ok "while the same Service without one is accepted"
else
  bad "a Service without an external IP is accepted" "it was refused"
fi

# ⭐ And the update path, which is a separate call site: adding the field to an
# existing Service must be refused too, or the guard is create-only and a tenant
# just writes twice.
extip_out=$($T patch service extip-probe --type=merge \
  -p '{"spec":{"externalIPs":["10.99.99.99"]}}' 2>&1)
if grep -q "externalIPs" <<<"$extip_out"; then
  ok "and adding one to a Service that already exists is refused as well"
else
  bad "adding an external IP by update is refused" \
      "$(tr '\n' ' ' <<<"$extip_out" | cut -c1-160)"
fi
$T delete service extip-probe >/dev/null 2>&1

echo
echo "== a tenant asks for DEVICES by class, and only the classes it was offered =="
# ⛔ A DeviceClass selects hardware -- which GPUs, at which tier -- and a
# ResourceClaim reaches it BY NAME from inside a tenant's namespace. Same shape
# as storageClassName, ingressClassName and volumeAttributesClassName, all three
# of which arrived as "the field is passed through, so naming the platform's own
# object simply works" and were fixed afterwards. This one was wired as the API
# group was opened, so there is no version in which it works.
$K apply -f - >/dev/null 2>&1 <<EOF
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: lab-offered
  labels: {deviceclass.kubezoo.io/published: "true"}
spec: {}
---
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata: {name: lab-withheld}
spec: {}
EOF
dc_seen=""
for _ in $(seq 20); do
  dc_seen=$($T get deviceclasses --no-headers 2>&1)
  grep -q lab-offered <<<"$dc_seen" && break
  sleep 2
done
if grep -q "lab-offered" <<<"$dc_seen"; then
  ok "a published device class is discoverable by the tenant"
else
  bad "a published device class is discoverable" "$(tr '\n' ' ' <<<"$dc_seen" | cut -c1-160)"
fi
# ⚠️ The control. Without it the assertion above passes just as well if every
# device class were visible.
if grep -q "lab-withheld" <<<"$dc_seen"; then
  bad "an unpublished device class stays invisible" \
      "the tenant can enumerate hardware tiers it was not offered"
else
  ok "while one the platform did not publish stays invisible"
fi

expect_allowed "a tenant may claim a device of a class it was offered" \
  $T -n default apply -f - <<EOF
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata: {name: offered-claim}
spec:
  devices:
    requests:
    - name: d
      exactly: {deviceClassName: lab-offered, allocationMode: ExactCount, count: 1}
EOF

# ⭐⭐ The claim that matters: naming a class the platform kept is refused, not
# silently served.
expect_denied "and may not claim one of a class it was not offered" \
  "not offered to this tenant" -- \
  $T -n default apply -f - <<EOF
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata: {name: withheld-claim}
spec:
  devices:
    requests:
    - name: d
      exactly: {deviceClassName: lab-withheld, allocationMode: ExactCount, count: 1}
EOF

# ⚠️ firstAvailable is a LIST of alternatives and every entry is a class the
# claim may end up using. Checking only the first would let an unpublished one
# through as the fallback -- which is how a tenant would reach for it.
expect_denied "nor as a fallback among alternatives" \
  "not offered to this tenant" -- \
  $T -n default apply -f - <<EOF
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata: {name: fallback-claim}
spec:
  devices:
    requests:
    - name: d
      firstAvailable:
      - {name: a, deviceClassName: lab-offered, allocationMode: ExactCount, count: 1}
      - {name: b, deviceClassName: lab-withheld, allocationMode: ExactCount, count: 1}
EOF

# ⛔ ResourceSlice is the platform's hardware inventory: spec.nodeName,
# spec.nodeSelector and the devices of each machine. Nodes were withdrawn from
# tenants for exactly that, and this is the same thing under another name.
if $T get resourceslices >/dev/null 2>&1; then
  bad "a tenant cannot read the platform's device inventory" \
      "resourceslices is served to tenants -- that is every machine's name and hardware"
else
  ok "and cannot read the platform's per-node device inventory at all"
fi
$T -n default delete resourceclaim offered-claim >/dev/null 2>&1
$K delete deviceclass lab-offered lab-withheld >/dev/null 2>&1

echo
echo "== a tenant cannot open a port on the platform's nodes =="
# ⛔ A tenant has no node concept: it owns none of the machines, decides where
# nothing lands, and cannot address one. A port opened on EVERY node is therefore
# outside everything this layer maintains -- reachable by whoever reaches the
# node network, past the tenant's own NetworkPolicies -- and it comes out of a
# finite range shared first-come-first-served with every other tenant.
#
# ⭐ Single-layer, like the external IP above: no policy has a rule matching
# Services, so a pass here can only be kubezoo.
np_out=$($T apply -f - 2>&1 <<EOF
apiVersion: v1
kind: Service
metadata: {name: np-probe}
spec:
  type: NodePort
  ports: [{port: 80}]
  selector: {app: nothing}
EOF
)
if grep -q "spec.type" <<<"$np_out"; then
  ok "a Service of type NodePort is refused"
else
  bad "a Service of type NodePort is refused" "$(tr '\n' ' ' <<<"$np_out" | cut -c1-160)"
fi

# ⭐ The type is not the only way to ask. A node port can be NAMED on a Service of
# any other type, and refusing only the type would leave that open -- which is
# also the difference between reading spec.type and reading the ports.
np_out=$($T apply -f - 2>&1 <<EOF
apiVersion: v1
kind: Service
metadata: {name: np-probe}
spec:
  type: LoadBalancer
  ports: [{port: 80, nodePort: 30099}]
  selector: {app: nothing}
EOF
)
if grep -q "nodePort" <<<"$np_out"; then
  ok "and naming a node port on a Service of another type is refused too"
else
  bad "naming a node port on another type is refused" \
      "$(tr '\n' ' ' <<<"$np_out" | cut -c1-160)"
fi

# ...and the same Service without either is fine, so the refusal is about the
# node port rather than about Services or about LoadBalancer.
if $T apply -f - >/dev/null 2>&1 <<EOF
apiVersion: v1
kind: Service
metadata: {name: np-probe}
spec:
  ports: [{port: 80}]
  selector: {app: nothing}
EOF
then
  ok "while the same Service without one is accepted"
else
  bad "a Service without a node port is accepted" "it was refused"
fi

# ⭐ And the update path, a separate call site: converting an existing Service to
# NodePort must be refused too, or the guard is create-only and a tenant writes
# twice.
np_out=$($T patch service np-probe --type=merge -p '{"spec":{"type":"NodePort"}}' 2>&1)
if grep -q "spec.type" <<<"$np_out"; then
  ok "and converting an existing Service to NodePort is refused as well"
else
  bad "converting an existing Service to NodePort is refused" \
      "$(tr '\n' ' ' <<<"$np_out" | cut -c1-160)"
fi
$T delete service np-probe >/dev/null 2>&1

echo
echo "== what kubezoo refuses is written down, and so is the platform's own decision =="
# ⭐ Placed HERE on purpose, right after a refusal. The section above just had a
# request turned away by a kubezoo guard -- which means it never reached the
# upstream cluster, so the upstream audit log has nothing to say about it.
#
# ⛔ That is the gap this covers. kubezoo impersonates the tenant on every verb
# it forwards, so what a tenant MANAGES to do is already attributable upstream.
# What it TRIES and is stopped from doing is not, and neither are writes to
# Tenant objects, which live in kubezoo's own store. In an investigation those
# are the interesting halves: what was attempted, and who ordered the freeze.
audit_log=$LAB/kubezoo-audit.log
if [ ! -s "$audit_log" ]; then
  bad "kubezoo writes an audit log" "$audit_log is missing or empty -- --audit-policy-file/--audit-log-path did not take"
else
  ok "kubezoo writes an audit log"

  # The refusal from the section above: the tenant's identity, the verb, and a
  # 403 that upstream never saw.
  refused=$(grep '"objectRef"' "$audit_log" 2>/dev/null \
    | grep '"resource":"services"' | grep "\"username\":\"$TID-admin\"" \
    | grep '"code":4' | head -1)
  if [ -n "$refused" ]; then
    ok "a request kubezoo refused is recorded, with the tenant that sent it"
  else
    bad "a refused request is recorded" \
      "no services request from $TID-admin with a 4xx in the audit log, so a tenant's blocked attempts leave no trace"
  fi

  # ⚠️ And the bodies are NOT there. The policy logs metadata for everything, so
  # a refusal is visible without the audit log becoming a second copy of what
  # tenants send. A rule reordered above the secrets rule would break this
  # quietly, which is why it is asserted rather than assumed.
  if grep -q '"requestObject"' <<<"$refused"; then
    bad "the refusal is recorded without its body" \
      "the audit entry carries requestObject; the policy is logging bodies where it should log metadata"
  else
    ok "and recorded without the body the tenant sent"
  fi

  # The platform's own decision, in full. Tenant objects never leave kubezoo, so
  # this is the only place a freeze is written down at all.
  #
  # ⛔ ON ITS OWN TENANT, not on $TID, and this is the header's hostage rule in a
  # new costume. Suspending the tenant every later section uses is exactly the
  # same mistake as filling it to a quota: the first version did that, restored
  # it afterwards, and still handed the ClusterRoleBinding section a tenant whose
  # cluster-scoped grant had not been widened back yet -- which reported as that
  # cap failing to refuse, pointing nowhere near this code.
  #
  # ⭐ And nothing here needs a provisioned tenant. The audit record is written
  # when kubezoo serves the patch, whether or not the controller has got as far
  # as making namespaces -- so this costs one create and one delete, and waits
  # for nothing.
  audit_tid=909092
  kubectl --kubeconfig "$ZOOKC" create -f - >/dev/null 2>&1 <<EOF
apiVersion: tenant.kubezoo.io/v1alpha1
kind: Tenant
metadata: {name: "$audit_tid"}
spec: {}
EOF
  kubectl --kubeconfig "$ZOOKC" patch tenant "$audit_tid" --type=merge \
    -p '{"spec":{"suspension":{"mode":"ReadOnly","reason":"audit assertion"}}}' >/dev/null 2>&1
  sleep 1
  suspend=$(grep '"resource":"tenants"' "$audit_log" 2>/dev/null \
    | grep '"verb":"patch"' | grep "\"name\":\"$audit_tid\"" | tail -1)
  if [ -n "$suspend" ] && grep -q '"requestObject"' <<<"$suspend"; then
    ok "suspending a tenant is recorded in full, with who did it"
  else
    bad "suspending a tenant is recorded in full" \
      "no patch of tenant $audit_tid carrying requestObject; who froze a tenant and what they set would be unknowable"
  fi
  kubectl --kubeconfig "$ZOOKC" delete tenant "$audit_tid" >/dev/null 2>&1
fi

echo
echo "== the platform caps how many custom resource definitions a tenant may own =="
# ⛔ A ceiling on a SHARED-STRUCTURE amplifier, and a heavier one than the
# namespace cap because it does not go away between requests. Every tenant CRD is
# a real CRD upstream, so it enters that cluster's discovery document and its
# OpenAPI -- which every client of the cluster downloads, tenant or not -- and
# kubezoo keeps an informer over all of them with a type converter each.
#
# ⚠️ This section creates CRDs and does not delete them all, so it takes the same
# kind of hostage as a quota fixture. It runs near the end for that reason, and
# it counts what already exists first rather than assuming it starts from zero --
# earlier sections create CRDs of their own.
crd_before=$($K get crd --no-headers 2>/dev/null | grep -c "^${TID}-" || true)
crd_cap=${MAX_CRDS_PER_TENANT:-6}
crd_made=0
crd_refusal=""
for i in $(seq $((crd_cap + 2))); do
  crd_out=$($T apply -f - 2>&1 <<EOF
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: capwidget${i}s.cap${i}.example}
spec:
  group: cap${i}.example
  names: {plural: capwidget${i}s, singular: capwidget${i}, kind: CapWidget${i}}
  scope: Namespaced
  versions: [{name: v1, served: true, storage: true, schema: {openAPIV3Schema: {type: object}}}]
EOF
)
  if [ $? -eq 0 ]; then
    crd_made=$((crd_made + 1))
  else
    crd_refusal=$crd_out
    break
  fi
done
if grep -q "custom resource definitions and the limit is" <<<"$crd_refusal"; then
  ok "a tenant cannot own more custom resource definitions than the platform allows"
else
  bad "a tenant cannot own more custom resource definitions than the platform allows" \
      "made $crd_made past a starting count of $crd_before with a cap of $crd_cap; last answer: $(tr '\n' ' ' <<<"${crd_refusal:-<all accepted>}" | cut -c1-160)"
fi

# ⭐ The half that makes the cap survivable, and the reason it is CREATE-only:
# a tenant at its limit must still be able to delete, or it has no way back
# under. Refusing every write would leave it stuck at the ceiling forever.
if $T delete crd capwidget1s.cap1.example >/dev/null 2>&1; then
  ok "and a tenant at the limit can still delete one to get back under it"
else
  bad "a tenant at the limit can still delete one" "the delete was refused, so the cap is a trap"
fi
for i in $(seq $((crd_cap + 2))); do
  $T delete crd "capwidget${i}s.cap${i}.example" >/dev/null 2>&1
done

echo
echo "== the platform caps how many cluster role bindings a tenant may own =="
# ⛔ THIS RUNS SECOND-TO-LAST, immediately before the section that takes the
# tenant's RoleBinding away -- which has to stay last, so this cannot be after it. Filling a
# tenant to its ClusterRoleBinding limit means creating and then deleting roughly
# twenty of them, and each one is a RoleBinding in every namespace the tenant owns
# -- hundreds of writes through the projection. The upstream authorizer serves
# stale answers while its RBAC cache catches up with that churn, so a section
# scheduled after this one can find a ServiceAccount denied a grant it
# demonstrably has.
#
# ⚠️ Measured, not assumed. This section originally sat in the middle of the file
# and took "an operator ServiceAccount reads across the tenant's namespaces" down
# with it -- twice, once with the guard wired and once without, which is what
# showed the fixture rather than the negative control was at fault. Deleting the
# bindings and waiting for the count to come back down did NOT fix it: the count
# was never the problem.

# ⭐ The second multiplier. A tenant's ClusterRoleBinding is stored as one
# RoleBinding in EVERY namespace it owns, so the object count is bindings times
# namespaces -- capping namespaces alone leaves this dimension unbounded. RBAC
# authorization walks every binding in a namespace, so a large count slows every
# authorization decision made there, not only the tenant's own.
#
# ⭐ Single-layer, like the namespace cap: no policy counts anything.
#
# ⚠️ This is also what proves the guard is WIRED. The unit tests call it
# directly, so removing the call from Create leaves them green -- exactly the
# "defined but never called" shape this repository has been bitten by before.
crbcap_before=$($T get clusterrolebindings --no-headers 2>/dev/null | wc -l)
crbcap_out=""
crbcap_made=""
for i in $(seq 1 $((24 - crbcap_before + 1))); do
  crbcap_out=$($T apply -f - 2>&1 <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: crbcap-$i}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: view}
subjects: [{kind: ServiceAccount, name: default, namespace: default}]
EOF
) || break
  grep -qE "created|configured|unchanged" <<<"$crbcap_out" || break
  crbcap_made="$crbcap_made crbcap-$i"
done
if grep -q "limit is 24" <<<"$crbcap_out"; then
  ok "a tenant cannot own more cluster role bindings than the platform allows"
else
  bad "the cluster role binding cap refuses" \
      "got: $(tr '\n' ' ' <<<"$crbcap_out" | cut -c1-160)"
fi
# ⛔ Deleted AND waited for, not just deleted. This fixture fills the tenant to
# exactly the cap, so until the count comes back down every later section that
# creates a ClusterRoleBinding of its own is refused -- and the failure surfaces
# as that section's own assertion, saying nothing about a quota. It cost the
# "operator ServiceAccount reads across the tenant's namespaces" check twice
# before this loop existed, once with the guard wired and once without, which is
# what showed it was the fixture rather than the negative control.
for crb in $crbcap_made; do $T delete clusterrolebinding "$crb" >/dev/null 2>&1; done
for _ in $(seq 30); do
  [ "$($T get clusterrolebindings --no-headers 2>/dev/null | wc -l)" -le "$crbcap_before" ] && break
  sleep 2
done

echo
echo "== storage a tenant can actually use: a real CSI driver, end to end =="
# ⭐ LAST, and that placement is the hostage rule: this section relabels a shared
# StorageClass, so anything after it would inherit whichever state it left
# behind. It puts the label back at the end, but "puts it back" is not a
# guarantee -- a section that dies half way does not.
#
# ⭐ Why a real driver rather than kind's local-path: local-path binds
# WaitForFirstConsumer and nothing here ever provisioned from it, so kubezoo's
# dynamic-provisioning path had NEVER RUN. The first time it did, the tenant
# could not read, list or delete its own claim: the provisioner creates the
# PersistentVolume directly upstream as pvc-<uid>, carrying no tenant prefix,
# and pkg/convert/pvc.go refused any name without one -- failing the whole
# object, so `kubectl get pvc` returned an error instead of a list.
CSI_SC=csi-hostpath-sc
if ! $K get storageclass "$CSI_SC" >/dev/null 2>&1; then
  bad "CSI storage class" "$CSI_SC is missing -- up.sh installs the driver and creates it; the whole section below is skipped rather than reported green"
else

csi_pvc() { cat <<PVC
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: $1}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $CSI_SC
  resources: {requests: {storage: 64Mi}}
PVC
}
# $3 is the claim to mount, defaulting to the one the section creates.
csi_pod() { cat <<POD
apiVersion: v1
kind: Pod
metadata: {name: $1}
spec:
  securityContext: {fsGroup: 2000}
  containers:
    - name: c
      image: busybox
      command: ["sh","-c","$2"]
      volumeMounts: [{name: v, mountPath: /data}]
      securityContext: {privileged: false, allowPrivilegeEscalation: false, runAsNonRoot: true,
        runAsUser: 1000, capabilities: {drop: ["ALL"]}, seccompProfile: {type: RuntimeDefault}}
  volumes:
    - name: v
      persistentVolumeClaim: {claimName: ${3:-csi-data}}
POD
}

# --- unpublished: invisible, and not merely undiscoverable ------------------
$K label storageclass "$CSI_SC" storageclass.kubezoo.io/published- >/dev/null 2>&1
sleep 3
csi_seen=$($T get storageclass --no-headers 2>/dev/null | awk '{print $1}' | tr '\n' ' ')
case " $csi_seen " in
  *" $CSI_SC "*) bad "an unpublished class is hidden" "the tenant sees: $csi_seen" ;;
  *)             ok  "an unpublished CSI class is not in the tenant's list" ;;
esac
# ⚠️ Matched on the words the refusal actually uses. An assertion that only
# checks "was it refused" cannot tell the class check apart from anything else
# that might refuse the same write -- quota, policy, a typo in the manifest.
expect_denied "a PVC naming an unpublished class" "no storage class .* is available" -- \
  $T apply -f <(csi_pvc csi-refused)

# --- published: binds for real, mounts for real ------------------------------
$K label storageclass "$CSI_SC" storageclass.kubezoo.io/published=true --overwrite >/dev/null
for _ in $(seq 20); do $T get storageclass "$CSI_SC" >/dev/null 2>&1 && break; sleep 2; done
if $T get storageclass "$CSI_SC" >/dev/null 2>&1; then
  ok "a published class is visible under the name a PVC has to use"
else
  bad "published class visibility" "$($T get storageclass "$CSI_SC" 2>&1 | tr '\n' ' ' | cut -c1-140)"
fi

csi_out=$($T apply -f <(csi_pvc csi-data) 2>&1) \
  || bad "creating a claim on a published class" "$(tr '\n' ' ' <<<"$csi_out" | cut -c1-160)"
for _ in $(seq 40); do
  [ "$($T get persistentvolumeclaims csi-data -o jsonpath='{.status.phase}' 2>/dev/null)" = Bound ] && break
  sleep 3
done
csi_phase=$($T get persistentvolumeclaims csi-data -o jsonpath='{.status.phase}' 2>&1)
if [ "$csi_phase" = Bound ]; then
  ok "the tenant's claim really binds, provisioned by a real CSI driver"
else
  bad "the claim binds" "phase='$(cut -c1-120 <<<"$csi_phase")' -- an error here rather than a phase means the claim itself cannot be read, which is the pvc.go bug"
fi
# ⚠️ Asserted separately from the phase: the failure was that reading the claim
# ERRORED, so a check that only looked for "not Bound" would have reported the
# wrong thing. The volume name is what could not be converted.
csi_vol=$($T get persistentvolumeclaims csi-data -o jsonpath='{.spec.volumeName}' 2>&1)
csi_up=$($K -n "$NS" get pvc csi-data -o jsonpath='{.spec.volumeName}' 2>/dev/null)
if [ -n "$csi_up" ] && [ "$csi_vol" = "$csi_up" ]; then
  ok "and the tenant reads back the volume's real name, which is the only name it has"
else
  bad "reading a dynamically provisioned claim" "tenant got '$(cut -c1-120 <<<"$csi_vol")', upstream says '$csi_up'"
fi

$T apply -f <(csi_pod csi-writer 'echo tenant-data > /data/f && sleep 3600') >/dev/null 2>&1
for _ in $(seq 40); do
  [ "$($T get pod csi-writer -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && break
  sleep 3
done
if [ "$($T get pod csi-writer -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ]; then
  ok "a tenant Pod mounting that volume reaches Running"
else
  bad "mounting the volume" "$($K -n "$NS" describe pod csi-writer 2>&1 | tail -4 | tr '\n' ' ' | cut -c1-200)"
fi
# ⚠️ Read through the tenant's own exec. Inspecting the host would prove the
# driver works, which was never in doubt; the claim is that the TENANT's
# workload can use it.
csi_content=$($T exec csi-writer -- cat /data/f 2>&1 | tr -d '\r\n')
[ "$csi_content" = tenant-data ] && ok "and what it wrote into the volume reads back" \
  || bad "using the volume" "got: $(cut -c1-140 <<<"$csi_content")"

# --- retired: still visible, no new claims, existing keeps working -----------
$K label storageclass "$CSI_SC" storageclass.kubezoo.io/published=deprecated --overwrite >/dev/null
sleep 5
$T get storageclass "$CSI_SC" >/dev/null 2>&1 \
  && ok "a retired class is still VISIBLE, so an existing reference stays explicable" \
  || bad "retired class visibility" "the tenant can no longer see the class its own claim names"
# ⭐ "is being retired" specifically, not merely "refused": the retired state has
# to be distinguishable from the unpublished one, or this assertion would still
# pass if the label were simply removed -- which is a different decision with a
# different meaning, and the reason the two states exist separately.
expect_denied "a NEW claim on a retired class" "is being retired" -- \
  $T apply -f <(csi_pvc csi-after-retire)

# ⭐ The load-bearing half, and the easy one to fake: the SAME claim that existed
# before the label changed. Rebuilding one here would assert nothing about
# "existing".
[ "$($T get persistentvolumeclaims csi-data -o jsonpath='{.status.phase}' 2>/dev/null)" = Bound ] \
  && ok "the EXISTING claim is still Bound after its class was retired" \
  || bad "an existing claim survives retirement" "phase=$($T get persistentvolumeclaims csi-data -o jsonpath='{.status.phase}' 2>&1 | cut -c1-120)"

# ⚠️ A Pod RESTART is what a tenant actually hits -- a rollout, a drained node.
# The refusal is on the claim, so this has to keep working; if it does not,
# retiring a class is an outage rather than a wind-down.
$T delete pod csi-writer --wait=true >/dev/null 2>&1
$T apply -f <(csi_pod csi-reader 'sleep 3600') >/dev/null 2>&1
for _ in $(seq 40); do
  [ "$($T get pod csi-reader -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && break
  sleep 3
done
csi_content=$($T exec csi-reader -- cat /data/f 2>&1 | tr -d '\r\n')
if [ "$csi_content" = tenant-data ]; then
  ok "a NEW Pod still mounts the existing claim, and the data survived the restart"
else
  bad "remounting after retirement" "phase=$($T get pod csi-reader -o jsonpath='{.status.phase}' 2>&1) got: $(cut -c1-140 <<<"$csi_content")"
fi

# --- what the driver produces, and what the tenant may see of it -------------
# A real driver creates VolumeAttachments. kubezoo does not serve them and
# should not: spec.nodeName names a machine, which is what Node and
# ResourceSlice were withheld for.
#
# ⚠️ These four now fail EARLIER than they used to, and the assertion is written
# loosely enough not to notice. They were refused by the server, as NotFound;
# since discovery stopped advertising resources kubezoo does not install, kubectl
# refuses them itself with "doesn't have a resource type". Both are covered by the
# pattern below, and both mean the same thing here -- but if this ever gets
# tightened to one message, tighten it to the one the build actually produces.
csi_va=$($K get volumeattachments --no-headers 2>/dev/null | wc -l)
[ "$csi_va" -ge 1 ] || bad "the driver attaches" "no VolumeAttachment exists, so the check below proves nothing about a resource that is in use"
for csi_res in volumeattachments csidrivers csinodes csistoragecapacities; do
  csi_out=$($T get "$csi_res" 2>&1)
  if grep -qiE "not found|doesn't have a resource type|error|forbidden" <<<"$csi_out"; then
    ok "a tenant cannot address $csi_res"
  else
    bad "$csi_res is exposed" "the tenant got: $(tr '\n' ' ' <<<"$csi_out" | cut -c1-140)"
  fi
done

# --- what the tenant is TOLD exists, versus what it can reach ---------------
# ⛔ Asserted as a property over the whole discovery document rather than as a
# list of names, because a list only ever catches what somebody thought of.
# Measured before this held: of 64 kinds advertised to a tenant, ELEVEN answered
# NotFound -- the four machine-facing storage resources just checked above,
# resourceslices, ipaddresses, servicecidrs, and both admission policy kinds
# with their bindings. Every one of them sits in a group kubezoo does serve, and
# discovery filtered by GROUP.
#
# ⭐ Not a hole -- none of them was reachable. It is the API telling the tenant
# something untrue, in the direction that misleads rather than exposes: a tenant
# that tries to create a ValidatingAdmissionPolicy is told the platform has no
# such API, when the truth is that it may not have one. Anything that walks
# discovery and acts on it -- a backup tool, a policy engine, a dynamic informer
# -- breaks on all eleven.
disc_advertised=$($T api-resources --request-timeout=30s 2>/dev/null | awk 'NR>1{print $1}' | sort -u)
disc_count=$(wc -w <<<"$disc_advertised")
# ⚠️ A sweep over an empty list passes in silence, which is the failure this
# assertion exists to prevent.
if [ "$disc_count" -lt 40 ]; then
  bad "the tenant's discovery document" "only $disc_count kinds came back, so the sweep below proves nothing"
else
  disc_lying=""
  for disc_res in $disc_advertised; do
    # Create-only kinds: a review is POSTed and answered, never listed, so GET
    # says MethodNotAllowed and that is correct rather than a lie.
    case $disc_res in
      tokenreviews|subjectaccessreviews|selfsubjectaccessreviews|selfsubjectrulesreviews|\
      localsubjectaccessreviews|selfsubjectreviews|bindings) continue ;;
    esac
    disc_out=$($T get "$disc_res" --request-timeout=15s 2>&1 | head -1)
    case "$disc_out" in
      *"the server could not find"*|*NotFound*|*"doesn't have a resource type"*)
        disc_lying="$disc_lying $disc_res" ;;
    esac
  done
  if [ -z "$disc_lying" ]; then
    ok "every one of the $disc_count kinds discovery advertises can actually be addressed"
  else
    bad "discovery advertises what it does not serve" \
      "these answered NotFound when the tenant addressed them:$disc_lying -- a client that walks discovery breaks on each"
  fi
fi

# ⭐ The invariant the whole translation layer exists to hold, asserted over
# whole objects rather than over the fields somebody remembered: a tenant never
# sees its own upstream prefix. Anywhere it appears, some convertor stopped
# trimming -- and the failure that produces is silent, because the value is a
# perfectly well-formed name that simply belongs to the platform's namespace of
# names. That is exactly how the in-pod namespace file went wrong: every request
# succeeded, and an operator indexing on it matched nothing, forever.
#
# ⚠️ Read as JSON over every kind rather than per-field. A field list only ever
# covers what was thought of, and this whole section came out of finding that
# discovery had eleven kinds nobody had looked at.
leak_kinds="deployments replicasets pods services configmaps serviceaccounts roles
  rolebindings networkpolicies poddisruptionbudgets secrets endpoints endpointslices events
  persistentvolumeclaims ingresses jobs cronjobs horizontalpodautoscalers leases"
leaked=""
leak_seen=0
for leak_kind in $leak_kinds; do
  leak_json=$($T get "$leak_kind" -o json --request-timeout=20s 2>/dev/null) || continue
  [ -z "$leak_json" ] && continue
  leak_seen=$((leak_seen+1))
  leak_hit=$(printf '%s' "$leak_json" | grep -o "$TID-[a-z0-9.-]*" | sort -u | tr '\n' ' ')
  [ -n "$leak_hit" ] && leaked="$leaked $leak_kind($leak_hit)"
done
# ⚠️ A sweep that read nothing passes in silence.
if [ "$leak_seen" -lt 10 ]; then
  bad "the tenant's own view" "only $leak_seen kinds could be read, so the leak sweep proves nothing"
elif [ -z "$leaked" ]; then
  ok "across $leak_seen kinds, nothing the tenant reads back carries its upstream prefix"
else
  bad "the tenant sees its own upstream prefix" \
    "in:$leaked -- a name from the platform's namespace of names reached the tenant, and nothing errors when it does"
fi


# --- the other way into storage from a pod spec ----------------------------
# ⛔ A generic ephemeral volume embeds a whole PVC template, and the claim is
# created by kube-controller-manager's ephemeral controller straight upstream --
# it never passes through kubezoo, so the guard that matches
# *core.PersistentVolumeClaim never sees it. #105 refused the inline csi volume
# in this same pod spec; this is the second door in the same room.
#
# ⚠️ The class is UNPUBLISHED at this point in the section, which is what makes
# this a test of publication rather than of syntax.
# ⚠️ The class is RETIRED at this point in the section, not unpublished, so the
# refusal says so. A first version matched the unpublished wording and failed
# while the guard was working perfectly -- and the failure text ("refused, but
# not by ...") is the only reason that was obvious rather than a hunt.
expect_denied "an ephemeral volume on a retired class" "is being retired" -- \
  $T apply -f - <<'EPH'
apiVersion: v1
kind: Pod
metadata: {name: eph-refused}
spec:
  containers:
    - name: c
      image: busybox
      command: ["sh","-c","sleep 3600"]
      volumeMounts: [{name: v, mountPath: /data}]
      securityContext: {privileged: false, allowPrivilegeEscalation: false, runAsNonRoot: true,
        runAsUser: 1000, capabilities: {drop: ["ALL"]}, seccompProfile: {type: RuntimeDefault}}
  volumes:
    - name: v
      ephemeral:
        volumeClaimTemplate:
          spec:
            accessModes: [ReadWriteOnce]
            storageClassName: csi-hostpath-sc
            resources: {requests: {storage: 64Mi}}
EPH
# ⚠️ And through a TEMPLATE, patched in afterwards -- a Deployment's volumes are
# mutable, so a create-only guard would be reachable by writing twice.
expect_denied "and one inside a Deployment template" "is being retired" -- \
  $T apply -f - <<'EPH'
apiVersion: apps/v1
kind: Deployment
metadata: {name: eph-deploy}
spec:
  replicas: 0
  selector: {matchLabels: {app: eph}}
  template:
    metadata: {labels: {app: eph}}
    spec:
      containers:
        - name: c
          image: busybox
          command: ["sh","-c","sleep 3600"]
          volumeMounts: [{name: v, mountPath: /data}]
          securityContext: {privileged: false, allowPrivilegeEscalation: false, runAsNonRoot: true,
            runAsUser: 1000, capabilities: {drop: ["ALL"]}, seccompProfile: {type: RuntimeDefault}}
      volumes:
        - name: v
          ephemeral:
            volumeClaimTemplate:
              spec:
                accessModes: [ReadWriteOnce]
                storageClassName: csi-hostpath-sc
                resources: {requests: {storage: 64Mi}}
EPH
# ⭐ Positive control, and it is not decoration: without it this section would
# pass just as well if the guard refused EVERY ephemeral volume, which would be a
# different bug wearing the same green.
$K label storageclass "$CSI_SC" storageclass.kubezoo.io/published=true --overwrite >/dev/null
for _ in $(seq 20); do $T get storageclass "$CSI_SC" >/dev/null 2>&1 && break; sleep 2; done
expect_allowed "an ephemeral volume on a published class" \
  $T apply -f - <<'EPH'
apiVersion: v1
kind: Pod
metadata: {name: eph-ok}
spec:
  containers:
    - name: c
      image: busybox
      command: ["sh","-c","sleep 3600"]
      volumeMounts: [{name: v, mountPath: /data}]
      securityContext: {privileged: false, allowPrivilegeEscalation: false, runAsNonRoot: true,
        runAsUser: 1000, capabilities: {drop: ["ALL"]}, seccompProfile: {type: RuntimeDefault}}
  volumes:
    - name: v
      ephemeral:
        volumeClaimTemplate:
          spec:
            accessModes: [ReadWriteOnce]
            storageClassName: csi-hostpath-sc
            resources: {requests: {storage: 64Mi}}
EPH
$K label storageclass "$CSI_SC" storageclass.kubezoo.io/published- >/dev/null 2>&1

# --- an endpoint address has to be one of your own pods' -------------------
# ⛔ Seventh spec, seventh time all three layers were empty: the convertors
# translate targetRef and nothing else, no policy mentions endpoints, and
# upstream validates only the FORM of an address. Cross-tenant IS contained --
# the binding is keyed on the slice's own namespace, which kubezoo prefixes --
# but who FOLLOWS the address is not: an ingress controller connects to endpoint
# IPs directly, bypassing the ClusterIP, and nginx is a published class here.
expect_denied "an endpoint address with no pod behind it" "names no pod" -- \
  $T apply -f - <<'EP'
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata: {name: forged, labels: {kubernetes.io/service-name: whatever}}
addressType: IPv4
ports: [{port: 80}]
endpoints: [{addresses: ["10.0.0.1"]}]
EP
# ⚠️ And with a targetRef to a pod the tenant really owns, but an address that is
# not that pod's -- the shape a check on targetRef alone would let through.
expect_denied "an address that is not the pod's own" "not one of pod" -- \
  $T apply -f - <<'EP'
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata: {name: forged2, labels: {kubernetes.io/service-name: whatever}}
addressType: IPv4
ports: [{port: 80}]
endpoints:
  - addresses: ["169.254.10.10"]
    targetRef: {kind: Pod, name: csi-reader, namespace: default}
EP

# --- what the kubelet will dial on a tenant's behalf -----------------------
# ⛔ probe/http/request.go: `host := httpGet.Host`, and upstream's own comment
# says "When httpGet.Host is empty, podIP will be used instead" -- so a non-empty
# host is dialled verbatim, BY THE KUBELET, from the node's network. The tenant's
# pods are on a per-tenant network; the node is not. periodSeconds and the pod
# count are the tenant's, so it is a scanner the platform runs on a schedule the
# tenant sets, and the Ready condition reports the result back.
probe_pod() { cat <<PRB
apiVersion: v1
kind: Pod
metadata: {name: $1}
spec:
  containers:
    - name: c
      image: busybox
      command: ["sh","-c","sleep 3600"]
      $2
      securityContext: {privileged: false, allowPrivilegeEscalation: false, runAsNonRoot: true,
        runAsUser: 1000, capabilities: {drop: ["ALL"]}, seccompProfile: {type: RuntimeDefault}}
PRB
}
expect_denied "a liveness probe aimed at an address of the tenant's choosing" "dialled BY THE KUBELET" -- \
  $T apply -f <(probe_pod probe-http 'livenessProbe: {httpGet: {host: 169.254.169.254, path: /, port: 80}}')
expect_denied "and a tcpSocket one" "dialled BY THE KUBELET" -- \
  $T apply -f <(probe_pod probe-tcp 'readinessProbe: {tcpSocket: {host: 10.0.0.1, port: 22}}')
expect_denied "and a preStop hook" "dialled BY THE KUBELET" -- \
  $T apply -f <(probe_pod probe-hook 'lifecycle: {preStop: {httpGet: {host: 10.0.0.1, path: /, port: 80}}}')
# ⚠️ initContainers carry the same probes, and a rule applied to one list and not
# the other is this repository's most repeated bug.
expect_denied "and one on an initContainer" "dialled BY THE KUBELET" -- \
  $T apply -f - <<'PRB'
apiVersion: v1
kind: Pod
metadata: {name: probe-init}
spec:
  initContainers:
    - name: i
      image: busybox
      command: ["sh","-c","true"]
      restartPolicy: Always
      livenessProbe: {httpGet: {host: 10.0.0.1, path: /, port: 80}}
      securityContext: {privileged: false, allowPrivilegeEscalation: false, runAsNonRoot: true,
        runAsUser: 1000, capabilities: {drop: ["ALL"]}, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: c
      image: busybox
      command: ["sh","-c","sleep 3600"]
      securityContext: {privileged: false, allowPrivilegeEscalation: false, runAsNonRoot: true,
        runAsUser: 1000, capabilities: {drop: ["ALL"]}, seccompProfile: {type: RuntimeDefault}}
PRB
# ⭐ Positive control: an ordinary probe with no host is the normal case and the
# whole point of the field being optional. Without this the section would pass
# just as well if every probe were refused.
expect_allowed "while an ordinary probe on the pod's own address still works" \
  $T apply -f <(probe_pod probe-ok 'livenessProbe: {httpGet: {path: /, port: 8080}}')

echo
echo "== a registry address is dialled by the kubelet, so the tenant does not pick it =="
# ⛔ Measured, on this lab, before the policy existed:
#   image: 10.244.0.1:5000/x   -> Event: dial tcp 10.244.0.1:5000: connect: connection refused
#   image: 10.99.99.99:5000/x  -> Event: dial tcp 10.99.99.99:5000: i/o timeout
# open / closed / filtered are all distinguishable, the address and port come back
# verbatim, and the tenant reads it with `kubectl describe pod`. The pull happens
# in the NODE's network namespace, which is not the tenant's -- the same shape as
# the probe host and the ingress auth-url, with the kubelet doing the dialling.
# ⚠️ The name is a parameter because the initContainer case below puts two of
# these in one pod. Sharing the default made that object violate TWO things at
# once, and the one it was refused for was "Duplicate value: c" -- see the note
# in config/policy/README.md about not attributing a refusal to the wrong rule.
img_ctr() { printf '{name: %s, image: %s, command: ["sh","-c","sleep 3600"], securityContext: {privileged: false, allowPrivilegeEscalation: false, runAsNonRoot: true, runAsUser: 1000, capabilities: {drop: ["ALL"]}, seccompProfile: {type: RuntimeDefault}}}' "${2:-c}" "$1"; }
img_pod() {
  cat <<EOF
apiVersion: v1
kind: Pod
metadata: {name: $1}
spec:
  containers: [$(img_ctr "$2")]
EOF
}
# ⭐ Positive controls first, and the first one is not a formality: the rule has to
# tell a registry host from an ordinary image name, and "busybox:1.36" contains a
# colon. A check that read the colon as a port would refuse the commonest way
# anyone writes an image, and every refusal below would still pass.
expect_allowed "an ordinary Docker Hub image, tag and all" \
  $T -n default apply -f <(img_pod img-bare busybox:1.36)
# org/name is also Docker Hub -- a first segment with a slash after it is not a
# host unless it looks like one.
expect_allowed "and the org/name form of the same registry" \
  $T -n default apply -f <(img_pod img-org library/busybox:1.36)
expect_denied "an image whose registry is an address on the platform's network" "registry this platform allows" -- \
  $T -n default apply -f <(img_pod img-addr 10.99.99.99:5000/whatever:latest)
# An allowlist, not an address filter: a real registry nobody allowed is refused too.
expect_denied "and one from a real registry the platform has not allowed" "registry this platform allows" -- \
  $T -n default apply -f <(img_pod img-ghcr ghcr.io/someone/something:v1)
expect_denied "and one on an initContainer" "registry this platform allows" -- \
  $T -n default apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata: {name: img-init}
spec:
  initContainers: [$(img_ctr 10.99.99.99:5000/whatever:latest i)]
  containers: [$(img_ctr busybox:1.36 c)]
EOF
# ⛔⛔ The controller kinds are the point of writing this as a native VAP. Kyverno's
# autogen copies a validate.deny's CONDITIONS verbatim into the derived controller
# rules and only shifts references in the message (kyverno pkg/autogen/v1/rule.go:167),
# so a rule reading spec.containers would read an empty list on a Deployment and
# never refuse -- Ready, and doing nothing. If this assertion goes green while the
# Pod one does too, the branch for spec.template.spec is working.
expect_denied "and one in a Deployment's template, not just in a Pod" "registry this platform allows" -- \
  $T -n default apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: img-deploy}
spec:
  replicas: 1
  selector: {matchLabels: {app: img}}
  template:
    metadata: {labels: {app: img}}
    spec:
      containers: [$(img_ctr 10.99.99.99:5000/whatever:latest)]
EOF
# CronJob nests one level deeper than every other controller, so it is the branch
# most likely to be wrong -- and a wrong branch here fails OPEN.
expect_denied "and one in a CronJob, which nests a level deeper" "registry this platform allows" -- \
  $T -n default apply -f - <<EOF
apiVersion: batch/v1
kind: CronJob
metadata: {name: img-cron}
spec:
  schedule: "0 0 * * *"
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          containers: [$(img_ctr 10.99.99.99:5000/whatever:latest)]
EOF
# PodTemplate keeps its spec at the top level rather than under .spec, and it is
# the kind Kyverno's autogen leaves out entirely.
expect_denied "and one in a PodTemplate, the kind autogen forgets" "registry this platform allows" -- \
  $T -n default apply -f - <<EOF
apiVersion: v1
kind: PodTemplate
metadata: {name: img-tpl}
template:
  spec:
    containers: [$(img_ctr 10.99.99.99:5000/whatever:latest)]
EOF
# ⛔ And the subresource, which was missing from this policy's first draft: an
# ephemeral container's image cannot be set on a Pod create or update at all --
# upstream only accepts it here -- and kubezoo serves the subresource
# (cmd/kubezoo/app/apigroups.go:150). A policy matching "pods" alone would have
# carried an ephemeralContainers branch that nothing could ever reach.
expect_denied "and one on an ephemeral container, which only the subresource can set" "registry this platform allows" -- \
  $T -n default patch pod img-bare --subresource=ephemeralcontainers --type=strategic \
    -p "{\"spec\":{\"ephemeralContainers\":[{\"name\":\"dbg\",\"image\":\"10.99.99.99:5000/whatever:latest\",\"securityContext\":{\"privileged\":false,\"allowPrivilegeEscalation\":false,\"runAsNonRoot\":true,\"runAsUser\":1000,\"capabilities\":{\"drop\":[\"ALL\"]},\"seccompProfile\":{\"type\":\"RuntimeDefault\"}}}]}}"
$T -n default delete pod img-bare img-org --wait=false >/dev/null 2>&1

# --- the field the endpoint guard was trusting -----------------------------
# ⛔ kubezoo serves pods/status and a tenant is "*" on "*" in its own namespaces,
# so it could write status.podIPs -- the very field refuseForgedEndpointAddress
# reads to decide whether an endpoint address is real. The guard shipped
# validating a tenant-written value against a tenant-written value.
#
# ⭐ And the forged address is reachable on its own: the apiserver's pods/proxy
# subresource dials status.podIP straight from the control plane.
$T apply -f - >/dev/null 2>&1 <<'PS'
apiVersion: v1
kind: Pod
metadata: {name: status-probe}
spec:
  containers:
    - name: c
      image: busybox
      command: ["sh","-c","sleep 3600"]
      securityContext: {privileged: false, allowPrivilegeEscalation: false, runAsNonRoot: true,
        runAsUser: 1000, capabilities: {drop: ["ALL"]}, seccompProfile: {type: RuntimeDefault}}
PS
for _ in $(seq 40); do
  [ "$($T get pod status-probe -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && break
  sleep 3
done
# ⛔⛔ THE FIELD HERE IS THE SCALAR podIP, AND THAT IS THE WHOLE POINT.
#
# This assertion first patched status.podIPs, saw "patched (no change)", and was
# written up as a hole that could not be reproduced. It reproduces. Measured
# against the upstream apiserver directly, on an ordinary pod:
#
#   patch {"status":{"podIP":"10.9.9.9"}}                -> podIP=10.9.9.9 podIPs=[10.9.9.9]
#   patch {"status":{"podIPs":[{"ip":"10.9.9.9"}]}}      -> (no change)
#
# The reason is in the version this builds against, pkg/apis/core/v1/conversion.go:263:
#
#   // If both fields (v1.PodIPs and v1.PodIP) are provided and differ, then
#   // PodIP is authoritative for compatibility with older kubelets
#
# The internal PodStatus has no scalar field, so the two versioned ones are
# reconciled on the way in and the SCALAR WINS. A merge patch that sets only
# podIPs leaves the real podIP in place beside it, they differ, and the real one
# is taken -- the write lands and changes nothing.
#
# ⚠️ So "(no change)" never meant the write was refused. It meant this patch does
# not alter the stored object, and reading it as a refusal is what turned a live
# hole into a mystery. Both variants are asserted below, because a guard that
# only catches the ineffective one is worth nothing.
for ps_field in '"podIP":"10.99.99.99"' '"podIPs":[{"ip":"10.99.99.99"}]'; do
  $T patch pod status-probe --subresource=status --type=merge \
    -p "{\"status\":{$ps_field}}" >/dev/null 2>&1
done
# Asserted on the END STATE rather than on the refusal: what matters is not which
# layer says no, it is that the address a pod claims is not the tenant's to
# choose. Read through $K, because the tenant's own view is not the evidence.
ps_ip=$($K -n "$NS" get pod status-probe -o jsonpath='{.status.podIP}' 2>/dev/null)
ps_ips=$($K -n "$NS" get pod status-probe -o jsonpath='{.status.podIPs[0].ip}' 2>/dev/null)
if [ "$ps_ip" = 10.99.99.99 ] || [ "$ps_ips" = 10.99.99.99 ]; then
  bad "a tenant cannot choose its pod's address" "podIP=$ps_ip podIPs[0]=$ps_ips -- refuseForgedEndpointAddress reads this field to decide whether an endpoint address is real, so a tenant that can write it validates its own forgery, and the apiserver's pod proxy dials it straight from the control plane"
else
  ok "a tenant cannot choose the address its own pod claims, by either field (still $ps_ip)"
fi
# ⭐ Positive control: a readiness gate is a tenant's controller writing
# status.conditions, and refusing the whole subresource to close four fields
# would have taken that away. Without this the section would pass just as well
# if pods/status were refused outright.
expect_allowed "while a condition on the same subresource still writes" \
  $T patch pod status-probe --subresource=status --type=merge \
    -p '{"status":{"conditions":[{"type":"kubezoo.io/probe","status":"True","lastTransitionTime":"2024-01-01T00:00:00Z"}]}}'

# ⚠️ And the exemption that was not a race: a pod that never schedules has no
# address for as long as it exists, so "no IPs yet" was a permanent hole rather
# than a window. Asserted with a targetRef to a pod that HAS an address but a
# different one, and to one that has none at all.
expect_denied "an endpoint naming a pod that has no address" "not one of pod" -- \
  $T apply -f - <<'EPX'
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata: {name: forged3, labels: {kubernetes.io/service-name: whatever}}
addressType: IPv4
ports: [{port: 80}]
endpoints:
  - addresses: ["10.99.99.98"]
    targetRef: {kind: Pod, name: status-probe, namespace: default}
EPX

# --- the URL that is not called a URL --------------------------------------
# ⛔ kubezoo refuses a webhook's clientConfig.url because "a URL cannot be
# confined to the tenant; use clientConfig.service". That reasoning assumes a
# Service in the tenant's namespace can only reach the tenant's own pods -- true
# of ClusterIP, FALSE of ExternalName. Upstream's ResolveCluster has an explicit
# ExternalName branch returning https://<externalName>:<port>, so the apiserver
# would dial an arbitrary host FROM THE CONTROL PLANE, carrying an
# AdmissionReview. Not gated on --enable-aggregator-routing: the default
# resolver is the one with that branch.
expect_denied "an ExternalName service" "control plane will follow\|ExternalName" -- \
  $T apply -f - <<'EXTNAME'
apiVersion: v1
kind: Service
metadata: {name: ext-name}
spec: {type: ExternalName, externalName: attacker.example.com}
EXTNAME
# ⚠️ And the timing shape the parity test named: an ordinary Service first, then
# a patch. Nothing revalidates a webhook once written -- the resolver runs at
# call time -- so the Service write is the last chance.
$T apply -f - >/dev/null 2>&1 <<'EXTNAME'
apiVersion: v1
kind: Service
metadata: {name: ext-later}
spec: {ports: [{port: 80}], selector: {app: none}}
EXTNAME
expect_denied "and patching an existing service into one" "control plane will follow\|ExternalName" -- \
  $T patch service ext-later --type=merge -p '{"spec":{"type":"ExternalName","externalName":"attacker.example.com"}}'

# --- and the way round all of it -------------------------------------------
# ⛔ An inline csi volume names a DRIVER, not a class: no StorageClass, no claim,
# nothing for the publication check to look at. Measured before it was refused:
# with the class UNPUBLISHED the pod was accepted, reached Running, and had the
# driver's volume mounted. Pod Security does not help and is not meant to --
# `csi` is on the restricted profile's ALLOWED volume list.
#
# ⚠️ Both shapes. A Pod's spec.volumes is immutable so a create is the only way
# in, but a TEMPLATE's is not: refused at create, a tenant would otherwise create
# the workload clean and patch the volume in afterwards.
expect_denied "an inline csi volume in a Pod" "volume provided directly by a CSI driver" -- \
  $T apply -f - <<'INLINE'
apiVersion: v1
kind: Pod
metadata: {name: inline-csi}
spec:
  containers:
    - name: c
      image: busybox
      command: ["sh","-c","sleep 3600"]
      volumeMounts: [{name: v, mountPath: /data}]
      securityContext: {privileged: false, allowPrivilegeEscalation: false, runAsNonRoot: true,
        runAsUser: 1000, capabilities: {drop: ["ALL"]}, seccompProfile: {type: RuntimeDefault}}
  volumes:
    - name: v
      csi: {driver: hostpath.csi.k8s.io}
INLINE
expect_denied "and one patched into a Deployment afterwards" "volume provided directly by a CSI driver" -- \
  $T apply -f - <<'INLINE'
apiVersion: apps/v1
kind: Deployment
metadata: {name: inline-csi-deploy}
spec:
  replicas: 0
  selector: {matchLabels: {app: inline-csi}}
  template:
    metadata: {labels: {app: inline-csi}}
    spec:
      containers:
        - name: c
          image: busybox
          command: ["sh","-c","sleep 3600"]
          volumeMounts: [{name: v, mountPath: /data}]
          securityContext: {privileged: false, allowPrivilegeEscalation: false, runAsNonRoot: true,
            runAsUser: 1000, capabilities: {drop: ["ALL"]}, seccompProfile: {type: RuntimeDefault}}
      volumes:
        - name: v
          csi: {driver: hostpath.csi.k8s.io}
INLINE

# --- snapshots: a tenant's own volume, and only its own --------------------
# ⭐ The first PLATFORM CRD group tenants address under its real name.
# snapshot.storage.k8s.io comes with the platform's storage, not with a tenant,
# so the usual rule -- a CRD belongs to whoever's prefix its group carries --
# gives it to nobody. docs/design-volumesnapshot-cn.md is the review that an
# entry in the shared allowlist costs.
CSI_SNAPC=csi-hostpath-snapclass
if ! $K get volumesnapshotclass "$CSI_SNAPC" >/dev/null 2>&1; then
  bad "snapshot class" "$CSI_SNAPC is missing -- up.sh installs external-snapshotter and creates it; the snapshot checks are skipped rather than reported green"
else
# ⚠️ The storage class is RETIRED by the time this runs -- the section above
# leaves it that way on purpose. A snapshot restore creates a NEW claim on it,
# which is exactly what retirement refuses, so the restore would fail for a
# reason that has nothing to do with snapshots. Put it back first.
$K label storageclass "$CSI_SC" storageclass.kubezoo.io/published=true --overwrite >/dev/null
$K label volumesnapshotclass "$CSI_SNAPC" volumesnapshotclass.kubezoo.io/published=true --overwrite >/dev/null
sleep 4

csi_disc=$($T api-resources --api-group=snapshot.storage.k8s.io 2>&1)
grep -q volumesnapshots <<<"$csi_disc" \
  && ok "a tenant sees snapshot.storage.k8s.io under its real name, not a prefixed one" \
  || bad "shared group discovery" "$(tr '\n' ' ' <<<"$csi_disc" | cut -c1-180)"
# ⛔ And not the resource that would let it import another tenant's data.
grep -q volumesnapshotcontents <<<"$csi_disc" \
  && bad "volumesnapshotcontents is advertised" "a tenant must not even see it" \
  || ok "and volumesnapshotcontents is not advertised"

$T apply -f - >/dev/null 2>&1 <<SNAP
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: {name: csi-snap}
spec:
  volumeSnapshotClassName: $CSI_SNAPC
  source: {persistentVolumeClaimName: csi-data}
SNAP
for _ in $(seq 40); do
  [ "$($T get volumesnapshots csi-snap -o jsonpath='{.status.readyToUse}' 2>/dev/null)" = true ] && break
  sleep 3
done
if [ "$($T get volumesnapshots csi-snap -o jsonpath='{.status.readyToUse}' 2>/dev/null)" = true ]; then
  ok "the platform's snapshot controller really takes a tenant's snapshot"
else
  bad "taking a snapshot" "readyToUse='$($T get volumesnapshots csi-snap -o jsonpath='{.status.readyToUse}' 2>&1 | cut -c1-100)' ; $($K -n "$NS" describe volumesnapshot csi-snap 2>&1 | tail -3 | tr '\n' ' ' | cut -c1-200)"
fi
# ⭐ The controller writes snapcontent-<uid> straight upstream, with no tenant
# prefix -- the shape that made a dynamically provisioned claim unreadable.
# Reading it back at all is the assertion.
csi_bound=$($T get volumesnapshots csi-snap -o jsonpath='{.status.boundVolumeSnapshotContentName}' 2>&1)
[[ "$csi_bound" == snapcontent-* ]] \
  && ok "and the tenant reads back the content name the controller wrote" \
  || bad "reading the bound content" "got '$(cut -c1-140 <<<"$csi_bound")'"

$T apply -f - >/dev/null 2>&1 <<SNAP
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: csi-restored}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $CSI_SC
  resources: {requests: {storage: 64Mi}}
  dataSource: {name: csi-snap, kind: VolumeSnapshot, apiGroup: snapshot.storage.k8s.io}
SNAP
# ⚠️ Asserted before the Pod, so that a refused or unbound claim is not reported
# as a scheduling problem. It was: the claim was refused because its class had
# been retired two assertions earlier, and the failure read "pod does not have a
# host assigned".
for _ in $(seq 40); do
  [ "$($T get persistentvolumeclaims csi-restored -o jsonpath='{.status.phase}' 2>/dev/null)" = Bound ] && break
  sleep 3
done
if [ "$($T get persistentvolumeclaims csi-restored -o jsonpath='{.status.phase}' 2>/dev/null)" = Bound ]; then
  ok "a claim whose dataSource is the tenant's snapshot binds"
else
  bad "restoring into a claim" "phase='$($T get persistentvolumeclaims csi-restored -o jsonpath='{.status.phase}' 2>&1 | cut -c1-120)' -- an error rather than a phase means the claim itself was refused"
fi
$T apply -f <(csi_pod csi-restorer 'sleep 3600' csi-restored) >/dev/null 2>&1
for _ in $(seq 50); do
  [ "$($T get pod csi-restorer -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && break
  sleep 3
done
csi_content=$($T exec csi-restorer -- cat /data/f 2>&1 | tr -d '\r\n')
[ "$csi_content" = tenant-data ] \
  && ok "and a claim restored from that snapshot carries the tenant's data" \
  || bad "restoring from a snapshot" "phase=$($T get pod csi-restorer -o jsonpath='{.status.phase}' 2>&1) got: $(cut -c1-140 <<<"$csi_content")"

# ⛔ The escape the whole integration is shaped around. A VolumeSnapshotContent
# carries spec.source.snapshotHandle -- the real handle on the storage system --
# so a tenant able to make one could name another tenant's snapshot, point the
# reference back at itself (which passes every upstream check, namespace
# included) and restore a claim from it. Requiring a reference back does NOT
# help: it reserves the object, it does not stop the payload being someone
# else's. See docs/design-volumesnapshot-cn.md §3.
expect_denied "importing a pre-existing snapshot" "cannot be imported" -- \
  $T apply -f <(cat <<SNAP
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: {name: csi-imported}
spec:
  volumeSnapshotClassName: $CSI_SNAPC
  source: {volumeSnapshotContentName: snapcontent-someone-elses}
SNAP
)
csi_out=$($T apply -f - 2>&1 <<SNAP
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotContent
metadata: {name: csi-forged}
spec:
  deletionPolicy: Delete
  driver: hostpath.csi.k8s.io
  source: {snapshotHandle: "someone-elses-handle"}
  volumeSnapshotRef: {namespace: default, name: csi-snap}
SNAP
)
grep -qiE "not found|doesn't have a resource type|no matches|forbidden|error" <<<"$csi_out" \
  && ok "and a tenant cannot create a VolumeSnapshotContent at all" \
  || bad "content creation" "$(tr '\n' ' ' <<<"$csi_out" | cut -c1-180)"

# Publication is the authorization here too.
$K label volumesnapshotclass "$CSI_SNAPC" volumesnapshotclass.kubezoo.io/published- >/dev/null 2>&1
sleep 4
expect_denied "a snapshot on an unpublished class" "no volume snapshot class" -- \
  $T apply -f <(cat <<SNAP
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: {name: csi-unpublished}
spec:
  volumeSnapshotClassName: $CSI_SNAPC
  source: {persistentVolumeClaimName: csi-data}
SNAP
)
fi

# --- the shapes a claim can take, beside an ordinary filesystem ------------
$T apply -f - >/dev/null 2>&1 <<'BLK'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: csi-block}
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Block
  storageClassName: csi-hostpath-sc
  resources: {requests: {storage: 64Mi}}
BLK
for _ in $(seq 40); do
  [ "$($T get persistentvolumeclaims csi-block -o jsonpath='{.status.phase}' 2>/dev/null)" = Bound ] && break
  sleep 3
done
$T apply -f - >/dev/null 2>&1 <<'BLK'
apiVersion: v1
kind: Pod
metadata: {name: csi-block-pod}
spec:
  containers:
    - name: c
      image: busybox
      command: ["sh","-c","sleep 3600"]
      volumeDevices: [{name: v, devicePath: /dev/xvda}]
      securityContext: {privileged: false, allowPrivilegeEscalation: false, runAsNonRoot: true,
        runAsUser: 1000, capabilities: {drop: ["ALL"]}, seccompProfile: {type: RuntimeDefault}}
  volumes: [{name: v, persistentVolumeClaim: {claimName: csi-block}}]
BLK
for _ in $(seq 40); do
  [ "$($T get pod csi-block-pod -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && break
  sleep 3
done
# ⚠️ Asserted on the device NODE, not on the pod running: a Pod can be Running
# with the device path missing, and "Running" would report that as success.
csi_dev=$($T exec csi-block-pod -- sh -c 'ls -l /dev/xvda 2>&1' 2>&1 | tr -d '\r' | head -1)
case "$csi_dev" in
  b*) ok "a tenant gets a raw block volume as an actual block device" ;;
  *)  bad "a block volume reaches the pod" "ls said: $(cut -c1-120 <<<"$csi_dev")" ;;
esac

$T apply -f - >/dev/null 2>&1 <<'CLONE'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: csi-clone}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: csi-hostpath-sc
  resources: {requests: {storage: 64Mi}}
  dataSource: {kind: PersistentVolumeClaim, name: csi-data}
CLONE
for _ in $(seq 40); do
  [ "$($T get persistentvolumeclaims csi-clone -o jsonpath='{.status.phase}' 2>/dev/null)" = Bound ] && break
  sleep 3
done
[ "$($T get persistentvolumeclaims csi-clone -o jsonpath='{.status.phase}' 2>/dev/null)" = Bound ] \
  && ok "a claim cloned from the tenant's own claim binds" \
  || bad "cloning a claim" "phase=$($T get persistentvolumeclaims csi-clone -o jsonpath='{.status.phase}' 2>&1 | cut -c1-60) ; $($K -n "$NS" describe pvc csi-clone 2>&1 | tail -3 | tr '\n' ' ' | cut -c1-160)"
# ⭐ dataSource is a TypedLocalObjectReference, so the source is same-namespace by
# construction. Asserted rather than assumed: "safe by construction" has been
# wrong in both directions today, and what lands UPSTREAM is what decides.
csi_srcns=$($K -n "$NS" get pvc csi-clone -o jsonpath='{.spec.dataSourceRef.namespace}' 2>/dev/null)
{ [ -z "$csi_srcns" ] || [ "$csi_srcns" = "$NS" ]; } \
  && ok "and its data source resolves inside the tenant" \
  || bad "clone source namespace" "upstream says '$csi_srcns', the tenant's namespace is $NS"

# --- a performance tier is sold, and a deleted claim really goes away ------
if $K get volumeattributesclass csi-hostpath-gold >/dev/null 2>&1; then
  # ⭐ #93 added the check and nothing ever ran it against a driver. Both states.
  $K label volumeattributesclass csi-hostpath-gold volumeattributesclass.kubezoo.io/published- >/dev/null 2>&1
  sleep 3
  expect_denied "a claim naming an unpublished VolumeAttributesClass" "publish\|available\|forbidden" -- \
    $T apply -f - <<'VAC'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: csi-vac-refused}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: csi-hostpath-sc
  volumeAttributesClassName: csi-hostpath-gold
  resources: {requests: {storage: 64Mi}}
VAC
  $K label volumeattributesclass csi-hostpath-gold volumeattributesclass.kubezoo.io/published=true --overwrite >/dev/null
  for _ in $(seq 20); do $T get volumeattributesclass csi-hostpath-gold >/dev/null 2>&1 && break; sleep 2; done
  $T apply -f - >/dev/null 2>&1 <<'VAC'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: csi-vac}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: csi-hostpath-sc
  volumeAttributesClassName: csi-hostpath-gold
  resources: {requests: {storage: 64Mi}}
VAC
  for _ in $(seq 40); do
    [ "$($T get persistentvolumeclaims csi-vac -o jsonpath='{.status.phase}' 2>/dev/null)" = Bound ] && break
    sleep 3
  done
  [ "$($T get persistentvolumeclaims csi-vac -o jsonpath='{.status.phase}' 2>/dev/null)" = Bound ] \
    && ok "and a claim on a published one binds, with the driver applying it for real" \
    || bad "a published VolumeAttributesClass" "phase=$($T get persistentvolumeclaims csi-vac -o jsonpath='{.status.phase}' 2>&1 | cut -c1-60) ; $($K -n "$NS" describe pvc csi-vac 2>&1 | tail -3 | tr '\n' ' ' | cut -c1-160)"
  $K label volumeattributesclass csi-hostpath-gold volumeattributesclass.kubezoo.io/published- >/dev/null 2>&1
else
  bad "VolumeAttributesClass fixture" "csi-hostpath-gold is missing -- up.sh creates it; the two checks below it are skipped rather than reported green"
fi

# ⚠️ Deletes the CLONE, not a claim anything else reads. Reclaiming is the one
# storage check that destroys what it looks at.
csi_clonevol=$($T get persistentvolumeclaims csi-clone -o jsonpath='{.spec.volumeName}' 2>/dev/null)
$T delete persistentvolumeclaims csi-clone --wait=true >/dev/null 2>&1
csi_gone=no
for _ in $(seq 40); do
  $K get pv "$csi_clonevol" >/dev/null 2>&1 || { csi_gone=yes; break; }
  sleep 3
done
[ "$csi_gone" = yes ] \
  && ok "deleting a claim really deletes its volume, so the space comes back" \
  || bad "reclaiming a volume" "PV $csi_clonevol still there after 120s (phase=$($K get pv "$csi_clonevol" -o jsonpath='{.status.phase}' 2>&1 | cut -c1-40))"

# ⚠️ LAST of the storage checks, and that is not cosmetic: growing csi-data
# changes a fixture every check before it reads. Placed earlier, it made the
# snapshot restore fail -- a claim restored from a 128Mi snapshot cannot ask for
# 64Mi, and upstream refused it correctly while the failure read as a snapshot
# problem. The hostage rule in this file's header, met for the third time today.
# --- growing a volume: the only tenant write that really updates a bound claim
# ⭐ Why this is here rather than in a storage backlog: spec.volumeName is
# IMMUTABLE once a claim is bound, and pkg/convert/pvc.go decides whether to
# re-prefix it. An expansion is the first and only tenant operation that
# exercises that decision on a live claim -- if Forward gets it wrong, upstream
# refuses the whole write and the tenant can never touch its own volume again.
csi_before=$($T get persistentvolumeclaims csi-data -o jsonpath='{.spec.volumeName}' 2>&1)
csi_out=$($T patch persistentvolumeclaims csi-data --type=merge \
  -p '{"spec":{"resources":{"requests":{"storage":"128Mi"}}}}' 2>&1)
grep -qiE "patched|configured" <<<"$csi_out" \
  && ok "a tenant can patch its bound claim to a larger size" \
  || bad "expanding a claim" "$(tr '\n' ' ' <<<"$csi_out" | cut -c1-180)"
csi_after=$($T get persistentvolumeclaims csi-data -o jsonpath='{.spec.volumeName}' 2>&1)
[ "$csi_before" = "$csi_after" ] \
  && ok "and the immutable volumeName came through the update untouched" \
  || bad "volumeName mangled by an update" "before='$csi_before' after='$csi_after' -- re-prefixing an immutable field makes upstream refuse every later write to this claim"

# ⚠️ Two halves, asserted apart, because they fail for different reasons: the
# driver growing the volume, and the filesystem catching up on the node. A single
# check reported "the volume does not grow" when it had grown and only a pod was
# missing -- upstream says so itself, in the FileSystemResizePending condition.
for _ in $(seq 40); do
  [ "$($K get pv "$csi_before" -o jsonpath='{.spec.capacity.storage}' 2>/dev/null)" = 128Mi ] && break
  sleep 3
done
[ "$($K get pv "$csi_before" -o jsonpath='{.spec.capacity.storage}' 2>/dev/null)" = 128Mi ] \
  && ok "the driver really grows the volume" \
  || bad "control-plane expansion" "PV capacity=$($K get pv "$csi_before" -o jsonpath='{.spec.capacity.storage}' 2>&1 | cut -c1-60)"
for _ in $(seq 50); do
  [ "$($T get persistentvolumeclaims csi-data -o jsonpath='{.status.capacity.storage}' 2>/dev/null)" = 128Mi ] && break
  sleep 3
done
[ "$($T get persistentvolumeclaims csi-data -o jsonpath='{.status.capacity.storage}' 2>/dev/null)" = 128Mi ] \
  && ok "and the tenant's claim reports the new size" \
  || bad "the claim catches up" "status.capacity=$($T get persistentvolumeclaims csi-data -o jsonpath='{.status.capacity.storage}' 2>&1 | cut -c1-60) ; conditions: $($T get persistentvolumeclaims csi-data -o jsonpath='{range .status.conditions[*]}{.type}={.status} {end}' 2>&1 | cut -c1-100)"
expect_denied "shrinking it again" "invalid\|smaller\|forbidden" -- \
  $T patch persistentvolumeclaims csi-data --type=merge -p '{"spec":{"resources":{"requests":{"storage":"64Mi"}}}}'


# Put the class back the way up.sh leaves it.
$K label storageclass "$CSI_SC" storageclass.kubezoo.io/published- >/dev/null 2>&1
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

# ⭐ And give the tenant its rights back before returning, rather than leaving
# "this section must stay last" as a convention someone has to read.
#
# ⚠️ I appended a section below this one anyway, and got exactly what the comment
# above predicts: a Forbidden that points at storage. A comment that has already
# been ignored once is not a guard. Waiting here turns the ordering requirement
# into a mechanism -- the section cleans up after itself, so appending below it
# becomes merely slow rather than wrong.
for _ in $(seq 40); do
  $T get serviceaccount default >/dev/null 2>&1 && break
  sleep 1
done
$T get serviceaccount default >/dev/null 2>&1 \
  || echo "  NOTE the tenant's RoleBinding has not come back; anything after this may fail for that reason"

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
