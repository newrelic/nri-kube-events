package objects

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cond(typ, status, reason string) map[string]any {
	c := map[string]any{"type": typ}
	if status != "" {
		c["status"] = status
	}
	if reason != "" {
		c["reason"] = reason
	}
	return c
}

func withConditions(kind string, conds ...map[string]any) map[string]any {
	list := make([]any, len(conds))
	for i, c := range conds {
		list[i] = c
	}
	return map[string]any{
		"kind":     kind,
		"metadata": map[string]any{"name": "x"},
		"status":   map[string]any{"conditions": list},
	}
}

func TestPolarityReplicaFailureTrueIsBad(t *testing.T) {
	h := ComputeHealth(withConditions("Deployment", cond("ReplicaFailure", "True", "FailedCreate")))
	assert.Equal(t, ",ReplicaFailure=True:FailedCreate,", h.ConditionsUnhealthy)
	assert.True(t, h.Unhealthy)
}

func TestPolarityScalingLimitedTrueIsBad(t *testing.T) {
	h := ComputeHealth(withConditions("HorizontalPodAutoscaler", cond("ScalingLimited", "True", "TooManyReplicas")))
	assert.Equal(t, ",ScalingLimited=True:TooManyReplicas,", h.ConditionsUnhealthy)
	assert.True(t, h.Unhealthy)
}

func TestPolarityKedaFallbackFalseIsHealthy(t *testing.T) {
	h := ComputeHealth(withConditions("ScaledObject",
		cond("Ready", "True", "ScaledObjectReady"),
		cond("Active", "True", "ScalerActive"),
		cond("Fallback", "False", "NoFallbackFound"),
	))
	assert.Empty(t, h.ConditionsUnhealthy)
	assert.False(t, h.Unhealthy)

	bad := ComputeHealth(withConditions("ScaledObject", cond("Fallback", "True", "FallbackExists")))
	assert.Equal(t, ",Fallback=True:FallbackExists,", bad.ConditionsUnhealthy)
}

func TestPolarityAvailableFalseIsBad(t *testing.T) {
	h := ComputeHealth(withConditions("Deployment",
		cond("Progressing", "True", "NewReplicaSetAvailable"),
		cond("Available", "False", "MinimumReplicasUnavailable"),
	))
	assert.Equal(t, ",Available=False:MinimumReplicasUnavailable,", h.ConditionsUnhealthy)
	assert.True(t, h.Unhealthy)
}

func TestPolarityUnknownStatusOnPositiveIsBad(t *testing.T) {
	h := ComputeHealth(withConditions("Node", cond("Ready", "Unknown", "NodeStatusUnknown")))
	assert.Equal(t, ",Ready=Unknown:NodeStatusUnknown,", h.ConditionsUnhealthy)
}

func TestPolarityProgressingFalseIsBad(t *testing.T) {
	h := ComputeHealth(withConditions("Deployment", cond("Progressing", "False", "ProgressDeadlineExceeded")))
	assert.Equal(t, ",Progressing=False:ProgressDeadlineExceeded,", h.ConditionsUnhealthy)
}

func TestConditionsUnhealthySortedAndEmptyReason(t *testing.T) {
	h := ComputeHealth(withConditions("Node",
		cond("Ready", "False", ""),
		cond("DiskPressure", "True", "KubeletHasDiskPressure"),
	))
	assert.Equal(t, ",DiskPressure=True:KubeletHasDiskPressure,,Ready=False:,", h.ConditionsUnhealthy)
}

func TestNodeMemoryPressureFalseIsHealthy(t *testing.T) {
	h := ComputeHealth(withConditions("Node",
		cond("MemoryPressure", "False", "KubeletHasSufficientMemory"),
		cond("DiskPressure", "False", "KubeletHasNoDiskPressure"),
		cond("PIDPressure", "False", "KubeletHasSufficientPID"),
		cond("NetworkUnavailable", "False", "RouteCreated"),
		cond("Ready", "True", "KubeletReady"),
	))
	assert.Empty(t, h.ConditionsUnhealthy)
	assert.False(t, h.Unhealthy)
	require.NotNil(t, h.Ready)
	assert.True(t, *h.Ready)
}

func TestProviderInstalledIsNotNegative(t *testing.T) {
	assert.False(t, IsNegativeCondition("ProviderInstalled"))
	h := ComputeHealth(withConditions("Provider", cond("ProviderInstalled", "True", "")))
	assert.Empty(t, h.ConditionsUnhealthy)
	assert.False(t, h.Unhealthy)
}

func TestIsNegativeCondition(t *testing.T) {
	negative := []string{
		"ReplicaFailure", "ScalingLimited", "MemoryPressure", "DiskPressure", "PIDPressure",
		"NetworkUnavailable", "Fallback", "NonStructuralSchema", "DisruptionTarget", "Deleting",
		"ActiveIncident", "ActiveFreeze", "replicafailure", "pidpressure",
		"InvalidSpecError", "SyncFailed", "ComparisonError", "ResourceDegraded", "ReconcileStalled",
		"CertificateExpired", "ConfigDrifted", "ServiceUnavailable", "OOMError", "Failure",
		"Dangling",
	}
	for _, typ := range negative {
		assert.True(t, IsNegativeCondition(typ), typ)
	}
	positive := []string{
		"Ready", "Available", "Progressing", "ProviderInstalled", "Initialized",
		"ContainersReady", "PodScheduled", "Established", "NamesAccepted", "Active",
		"Unfailed", "Errored", "ErrorsResolved", "",
	}
	for _, typ := range positive {
		assert.False(t, IsNegativeCondition(typ), typ)
	}
}

// NonStructuralSchema=True is a schema-quality warning, not an incident: neutral types are
// ignored for health whatever their status.
func TestNeutralConditionsIgnored(t *testing.T) {
	for _, st := range []string{"True", "False", "Unknown", ""} {
		h := ComputeHealth(withConditions("CustomResourceDefinition",
			cond("Established", "True", "InitialNamesAccepted"),
			cond("NonStructuralSchema", st, "Violations"),
		))
		assert.Equal(t, "", h.ConditionsUnhealthy, st)
		assert.False(t, h.Unhealthy, st)
	}
	// Other bad conditions of the same object still count.
	h := ComputeHealth(withConditions("CustomResourceDefinition",
		cond("Established", "False", "NotAccepted"),
		cond("NonStructuralSchema", "True", "Violations"),
	))
	assert.Equal(t, ",Established=False:NotAccepted,", h.ConditionsUnhealthy)
}

// A FlowSchema reports Dangling=False when its priority level exists; True is the problem.
func TestFlowSchemaDanglingPolarity(t *testing.T) {
	h := ComputeHealth(withConditions("FlowSchema", cond("Dangling", "False", "Found")))
	assert.Equal(t, "", h.ConditionsUnhealthy)
	assert.False(t, h.Unhealthy)

	h = ComputeHealth(withConditions("FlowSchema", cond("Dangling", "True", "NotFound")))
	assert.Equal(t, ",Dangling=True:NotFound,", h.ConditionsUnhealthy)
	assert.True(t, h.Unhealthy)
}

// metadata.generation counts spec changes. An object with no spec has nothing for a
// controller to observe: a gatekeeper ConstraintPodStatus carries the observedGeneration
// of the constraint it reports on, not its own, and is never stale.
func TestObjectWithoutSpecIsNotStale(t *testing.T) {
	status := map[string]any{
		"constraintUID":      "c1d81643-997e-4f0e-85d3-8b4287016f16",
		"enforced":           true,
		"observedGeneration": int64(1),
		"conditions":         []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(1)}},
	}
	obj := map[string]any{
		"kind":     "ConstraintPodStatus",
		"metadata": map[string]any{"name": "x", "generation": int64(2)},
		"status":   status,
	}
	h := ComputeHealth(obj)
	assert.False(t, h.Stale)
	assert.False(t, h.Unhealthy)

	obj["spec"] = map[string]any{"replicas": int64(1)}
	h = ComputeHealth(obj)
	assert.True(t, h.Stale, "an object with a spec is still checked")
	assert.True(t, h.Unhealthy)
}

func TestStatuslessNegativeConditionIsBad(t *testing.T) {
	h := ComputeHealth(withConditions("Application",
		map[string]any{"type": "InvalidSpecError", "message": "application destination is invalid"},
	))
	assert.Equal(t, ",InvalidSpecError=:,", h.ConditionsUnhealthy)
	assert.True(t, h.Unhealthy)

	ok := ComputeHealth(withConditions("Thing", map[string]any{"type": "Ready"}))
	assert.Empty(t, ok.ConditionsUnhealthy)
}

func TestPausedIsNotBad(t *testing.T) {
	for _, typ := range []string{"Paused", "Remediating", "RollingOut", "ScalingUp", "ScalingDown"} {
		for _, st := range []string{"True", "False", "Unknown", ""} {
			h := ComputeHealth(withConditions("Thing", cond(typ, st, "R")))
			assert.Empty(t, h.ConditionsUnhealthy, typ+"="+st)
		}
	}
}

func TestStaleFromPerConditionObservedGeneration(t *testing.T) {
	c := cond("Ready", "True", "")
	c["observedGeneration"] = int64(3)
	obj := withConditions("Thing", c)
	obj["spec"] = map[string]any{}
	obj["metadata"].(map[string]any)["generation"] = int64(4)
	h := ComputeHealth(obj)
	assert.True(t, h.Stale)
	assert.True(t, h.Unhealthy)
	assert.Empty(t, h.ConditionsUnhealthy)

	c["observedGeneration"] = float64(4)
	assert.False(t, ComputeHealth(obj).Stale)
}

func TestStaleWhenObservedGenerationBehind(t *testing.T) {
	obj := map[string]any{
		"kind":     "Deployment",
		"metadata": map[string]any{"generation": float64(7)},
		"spec":     map[string]any{"replicas": int64(1)},
		"status":   map[string]any{"observedGeneration": float64(6)},
	}
	h := ComputeHealth(obj)
	assert.True(t, h.Stale)
	assert.True(t, h.Unhealthy)

	obj["status"].(map[string]any)["observedGeneration"] = int64(7)
	assert.False(t, ComputeHealth(obj).Stale)

	delete(obj["metadata"].(map[string]any), "generation")
	obj["status"].(map[string]any)["observedGeneration"] = int64(1)
	assert.False(t, ComputeHealth(obj).Stale, "no generation means not stale")
}

func TestPhaseFailedOrUnknownIsUnhealthy(t *testing.T) {
	for _, phase := range []string{"Failed", "Unknown"} {
		h := ComputeHealth(map[string]any{"kind": "Pod", "status": map[string]any{"phase": phase}})
		assert.Equal(t, phase, h.Phase)
		assert.True(t, h.Unhealthy, phase)
	}
	h := ComputeHealth(map[string]any{"kind": "Pod", "status": map[string]any{"phase": "Running"}})
	assert.Equal(t, "Running", h.Phase)
	assert.False(t, h.Unhealthy)
	assert.Empty(t, ComputeHealth(map[string]any{"kind": "ConfigMap"}).Phase)
}

func TestArgoHealthDegradedIsUnhealthy(t *testing.T) {
	for _, st := range []string{"Degraded", "Missing", "Unknown"} {
		obj := map[string]any{
			"kind":   "Application",
			"status": map[string]any{"health": map[string]any{"status": st}},
		}
		assert.True(t, ComputeHealth(obj).Unhealthy, st)
	}
	for _, st := range []string{"Healthy", "Progressing", "Suspended"} {
		obj := map[string]any{
			"kind":   "Application",
			"status": map[string]any{"health": map[string]any{"status": st}},
		}
		assert.False(t, ComputeHealth(obj).Unhealthy, st)
	}
}

func TestReadyFallsBackToAvailable(t *testing.T) {
	h := ComputeHealth(withConditions("Deployment", cond("Available", "True", "")))
	require.NotNil(t, h.Ready)
	assert.True(t, *h.Ready)

	h = ComputeHealth(withConditions("Thing", cond("Available", "True", ""), cond("Ready", "False", "")))
	require.NotNil(t, h.Ready)
	assert.False(t, *h.Ready, "Ready wins over Available")

	assert.Nil(t, ComputeHealth(withConditions("Thing", cond("Progressing", "True", ""))).Ready)
	assert.Nil(t, ComputeHealth(map[string]any{"kind": "ConfigMap"}).Ready)
}

func withPhase(obj map[string]any, phase string) map[string]any {
	obj["status"].(map[string]any)["phase"] = phase
	return obj
}

// A completed Pod reports Ready, ContainersReady and PodReadyToStartContainers False with
// reason PodCompleted. That is its normal terminal state, not a problem.
func TestSucceededPodReadinessConditionsAreHealthy(t *testing.T) {
	pod := withPhase(withConditions("Pod",
		cond("PodReadyToStartContainers", "False", ""),
		cond("Initialized", "True", "PodCompleted"),
		cond("Ready", "False", "PodCompleted"),
		cond("ContainersReady", "False", "PodCompleted"),
		cond("PodScheduled", "True", ""),
	), "Succeeded")
	h := ComputeHealth(pod)
	assert.Equal(t, "", h.ConditionsUnhealthy)
	assert.False(t, h.Unhealthy)
	require.NotNil(t, h.Ready)
	assert.False(t, *h.Ready, "k8s.ready still reports the Ready condition")

	// Other bad conditions of a Succeeded object still count.
	h = ComputeHealth(withPhase(withConditions("Pod",
		cond("Ready", "False", "PodCompleted"),
		cond("DisruptionTarget", "True", "EvictionByEvictionAPI"),
	), "Succeeded"))
	assert.Equal(t, ",DisruptionTarget=True:EvictionByEvictionAPI,", h.ConditionsUnhealthy)
	assert.True(t, h.Unhealthy)
}

// Deleting a Pod bumps metadata.generation while status.observedGeneration stays, so a
// terminating object is not stale.
func TestTerminatingObjectIsNotStale(t *testing.T) {
	pod := func(deleting bool) map[string]any {
		obj := withPhase(withConditions("Pod", cond("Ready", "False", "PodCompleted")), "Succeeded")
		obj["spec"] = map[string]any{"restartPolicy": "Never"}
		meta := obj["metadata"].(map[string]any)
		meta["generation"] = int64(2)
		if deleting {
			meta["deletionTimestamp"] = "2026-11-06T09:00:00Z"
		}
		obj["status"].(map[string]any)["observedGeneration"] = int64(1)
		return obj
	}
	h := ComputeHealth(pod(true))
	assert.False(t, h.Stale)
	assert.False(t, h.Unhealthy)

	h = ComputeHealth(pod(false))
	assert.True(t, h.Stale)
	assert.True(t, h.Unhealthy)
}

func TestRunningOrFailedPodReadyFalseIsUnhealthy(t *testing.T) {
	for _, phase := range []string{"Running", "Failed", "Unknown", "Pending"} {
		h := ComputeHealth(withPhase(withConditions("Pod", cond("Ready", "False", "ContainersNotReady")), phase))
		assert.Equal(t, ",Ready=False:ContainersNotReady,", h.ConditionsUnhealthy, phase)
		assert.True(t, h.Unhealthy, phase)
	}
}

// ConditionsUnhealthy is capped at PartChars runes by dropping whole conditions from the
// end, so it still ends in "," and LIKE '%,Type=%' never matches a cut condition.
func TestConditionsUnhealthyCapped(t *testing.T) {
	var conds []map[string]any
	for i := 0; i < 300; i++ {
		conds = append(conds, cond(fmt.Sprintf("Check%04d", i), "False", "SomeLongReasonText"))
	}
	h := ComputeHealth(withConditions("Thing", conds...))
	assert.LessOrEqual(t, utf8.RuneCountInString(h.ConditionsUnhealthy), PartChars)
	assert.True(t, strings.HasPrefix(h.ConditionsUnhealthy, ",Check0000=False:SomeLongReasonText,"))
	assert.True(t, strings.HasSuffix(h.ConditionsUnhealthy, ",SomeLongReasonText,") || strings.HasSuffix(h.ConditionsUnhealthy, ":SomeLongReasonText,"))
	for _, item := range strings.Split(strings.Trim(h.ConditionsUnhealthy, ","), ",,") {
		assert.Regexp(t, `^Check\d{4}=False:SomeLongReasonText$`, item)
	}
	assert.True(t, h.ConditionsTruncated)
	assert.True(t, h.Unhealthy)

	// A single condition longer than the cap leaves the string empty but the object unhealthy.
	h = ComputeHealth(withConditions("Thing", cond("Ready", "False", strings.Repeat("x", PartChars))))
	assert.Equal(t, "", h.ConditionsUnhealthy)
	assert.True(t, h.ConditionsTruncated)
	assert.True(t, h.Unhealthy)

	h = ComputeHealth(withConditions("Thing", cond("Ready", "False", "")))
	assert.False(t, h.ConditionsTruncated)
}

func manyBadConditions(n int, reason string) []map[string]any {
	conds := make([]map[string]any, n)
	for i := range conds {
		conds[i] = cond(fmt.Sprintf("Check%04d", i), "False", reason)
	}
	return conds
}

// The forwarder truncates a string attribute past MaxAttrBytes bytes, so the cap also drops
// whole conditions by byte length.
func TestConditionsUnhealthyByteCapped(t *testing.T) {
	// 40 bad conditions whose reasons are 100 three-byte runes each: about 12,000 bytes in full.
	obj := withConditions("Thing", manyBadConditions(40, strings.Repeat("中", 100))...)
	h := ComputeHealth(obj)
	assert.LessOrEqual(t, len(h.ConditionsUnhealthy), MaxAttrBytes)
	assert.True(t, h.ConditionsTruncated)
	assert.True(t, strings.HasSuffix(h.ConditionsUnhealthy, ","))
	// Each item ",CheckNNNN=False:<reason>," is 318 bytes, and 12 × 318 = 3,816 fits in
	// MaxAttrBytes while 13 × 318 does not.
	assert.Equal(t, 12, strings.Count(h.ConditionsUnhealthy, "=False:"))
	assert.Equal(t, 12*318, len(h.ConditionsUnhealthy))
}
