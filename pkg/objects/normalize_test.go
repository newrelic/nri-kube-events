package objects

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The hash folds in HealthRuleVersion, so a carrier with new health rules sees every
// object as changed once and re-sends it with the new derived attributes.
func TestHashFoldsInHealthRuleVersion(t *testing.T) {
	obj := pod("nginx:1.27", "Running", "100", nil)
	b, err := json.Marshal(Normalize(obj))
	require.NoError(t, err)
	want := sha256.Sum256(append([]byte(fmt.Sprintf("health-rules-v%d\n", HealthRuleVersion)), b...))
	assert.Equal(t, hex.EncodeToString(want[:])[:16], Hash(obj))
	assert.Equal(t, 2, HealthRuleVersion)

	plain := sha256.Sum256(b)
	assert.NotEqual(t, hex.EncodeToString(plain[:])[:16], Hash(obj), "the version changes the hash")
	assert.NotEqual(t, hashWithRuleVersion(obj, 1), hashWithRuleVersion(obj, 2))
}

func pod(image, phase, resourceVersion string, managedFields []any) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":            "web-0",
			"namespace":       "default",
			"resourceVersion": resourceVersion,
			"managedFields":   managedFields,
		},
		"spec": map[string]any{
			"containers": []any{
				map[string]any{"name": "web", "image": image},
			},
		},
		"status": map[string]any{"phase": phase},
	}
}

func lease(holder, renewTime string) map[string]any {
	return map[string]any{
		"apiVersion": "coordination.k8s.io/v1",
		"kind":       "Lease",
		"metadata": map[string]any{
			"name":      "kube-scheduler",
			"namespace": "kube-system",
		},
		"spec": map[string]any{
			"holderIdentity":       holder,
			"leaseDurationSeconds": int64(15),
			"renewTime":            renewTime,
		},
	}
}

func node(heartbeat, ready string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata": map[string]any{
			"name": "node-a",
			"annotations": map[string]any{
				"control-plane.alpha.kubernetes.io/leader": `{"holderIdentity":"x","renewTime":"` + heartbeat + `"}`,
				"node.alpha.kubernetes.io/ttl":             "0",
			},
		},
		"status": map[string]any{
			"conditions": []any{
				map[string]any{
					"type":               "Ready",
					"status":             ready,
					"lastHeartbeatTime":  heartbeat,
					"lastTransitionTime": "2026-11-01T00:00:00Z",
				},
				map[string]any{
					"type":               "MemoryPressure",
					"status":             "False",
					"lastHeartbeatTime":  heartbeat,
					"lastTransitionTime": "2026-11-01T00:00:00Z",
				},
			},
		},
	}
}

func TestHashIgnoresManagedFieldsAndResourceVersion(t *testing.T) {
	a := pod("nginx:1.27", "Running", "100", []any{map[string]any{"manager": "kubelet"}})
	b := pod("nginx:1.27", "Running", "200", []any{map[string]any{"manager": "kube-controller-manager"}})
	assert.Equal(t, Hash(a), Hash(b))
	assert.Len(t, Hash(a), 16)
	assert.Regexp(t, `^[0-9a-f]{16}$`, Hash(a))
}

func TestHashChangesOnSpecAndStatus(t *testing.T) {
	base := Hash(pod("nginx:1.27", "Running", "100", nil))
	assert.NotEqual(t, base, Hash(pod("nginx:1.28", "Running", "100", nil)), "image change")
	assert.NotEqual(t, base, Hash(pod("nginx:1.27", "Failed", "100", nil)), "phase change")
}

func TestLeaseRenewTimeIgnored(t *testing.T) {
	base := Hash(lease("node-a", "2026-11-06T10:00:00.000000Z"))
	assert.Equal(t, base, Hash(lease("node-a", "2026-11-06T10:00:10.000000Z")))
	assert.NotEqual(t, base, Hash(lease("node-b", "2026-11-06T10:00:00.000000Z")))
}

func TestRenewTimeKeptForNonLeaseKinds(t *testing.T) {
	mk := func(renew string) map[string]any {
		return map[string]any{
			"apiVersion": "example.com/v1",
			"kind":       "Widget",
			"spec":       map[string]any{"renewTime": renew},
		}
	}
	assert.NotEqual(t, Hash(mk("a")), Hash(mk("b")))
}

func TestHeartbeatTimeIgnored(t *testing.T) {
	base := Hash(node("2026-11-06T10:00:00Z", "True"))
	assert.Equal(t, base, Hash(node("2026-11-06T10:00:40Z", "True")))
	assert.NotEqual(t, base, Hash(node("2026-11-06T10:00:00Z", "False")), "condition status change")
}

func TestLeaderAnnotationDroppedOtherAnnotationsKept(t *testing.T) {
	n := Normalize(node("2026-11-06T10:00:00Z", "True"))
	ann := n["metadata"].(map[string]any)["annotations"].(map[string]any)
	assert.NotContains(t, ann, "control-plane.alpha.kubernetes.io/leader")
	assert.Equal(t, "0", ann["node.alpha.kubernetes.io/ttl"])
}

func TestHashStableAcrossKeyOrder(t *testing.T) {
	a := map[string]any{}
	a["kind"] = "ConfigMap"
	a["apiVersion"] = "v1"
	a["data"] = map[string]any{"z": "1", "a": "2", "m": "3"}

	b := map[string]any{}
	b["data"] = map[string]any{"m": "3", "a": "2", "z": "1"}
	b["apiVersion"] = "v1"
	b["kind"] = "ConfigMap"

	assert.Equal(t, Hash(a), Hash(b))
}

func TestNormalizeDoesNotMutateInput(t *testing.T) {
	in := pod("nginx:1.27", "Running", "100", []any{map[string]any{"manager": "kubelet"}})
	nd := node("2026-11-06T10:00:00Z", "True")
	ls := lease("node-a", "2026-11-06T10:00:00Z")

	out := Normalize(in)
	_ = Normalize(nd)
	_ = Normalize(ls)

	meta := in["metadata"].(map[string]any)
	require.Contains(t, meta, "managedFields")
	assert.Equal(t, "100", meta["resourceVersion"])
	assert.NotContains(t, out["metadata"].(map[string]any), "managedFields")
	assert.NotContains(t, out["metadata"].(map[string]any), "resourceVersion")

	cond := nd["status"].(map[string]any)["conditions"].([]any)[0].(map[string]any)
	assert.Contains(t, cond, "lastHeartbeatTime")
	assert.Contains(t, nd["metadata"].(map[string]any)["annotations"].(map[string]any), "control-plane.alpha.kubernetes.io/leader")
	assert.Contains(t, ls["spec"].(map[string]any), "renewTime")

	// Mutating the output must not reach the input.
	out["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["image"] = "changed"
	assert.Equal(t, "nginx:1.27", in["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["image"])
}

func TestNormalizeToleratesMissingAndOddShapes(t *testing.T) {
	assert.NotPanics(t, func() {
		Normalize(nil)
		Normalize(map[string]any{})
		Normalize(map[string]any{"metadata": "not-a-map", "status": map[string]any{"conditions": "x"}})
		Normalize(map[string]any{"status": map[string]any{"conditions": []any{"not-a-map", nil}}})
		Hash(nil)
	})
}

func argoApp() map[string]any {
	return map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name":       "web",
			"namespace":  "argocd",
			"generation": int64(7),
		},
		"spec": map[string]any{
			"source": map[string]any{
				"repoURL":        "https://example.com/repo.git",
				"targetRevision": "v1",
			},
		},
		"status": map[string]any{
			"reconciledAt": "2026-11-17T10:00:00Z",
			"sync":         map[string]any{"status": "Synced"},
		},
	}
}

func scaledObject() map[string]any {
	return map[string]any{
		"apiVersion": "keda.sh/v1alpha1",
		"kind":       "ScaledObject",
		"metadata": map[string]any{
			"name":      "web",
			"namespace": "default",
			"annotations": map[string]any{
				"clm10-keda-scaler/cpuutilizationtarget.recommendation.history": `[{"t":"2026-11-17T10:00:00Z","v":60}]`,
				"example.com/owner": "team-a",
			},
		},
		"spec": map[string]any{
			"scaleTargetRef": map[string]any{"name": "web"},
		},
		"status": map[string]any{
			"lastActiveTime": "2026-11-17T10:00:00Z",
		},
	}
}

func TestArgoApplicationRefreshIgnored(t *testing.T) {
	a := argoApp()
	b := argoApp()
	b["status"].(map[string]any)["reconciledAt"] = "2026-11-17T10:03:00Z"
	b["metadata"].(map[string]any)["generation"] = int64(8)
	assert.Equal(t, Hash(a), Hash(b))
	c := argoApp()
	c["spec"].(map[string]any)["source"].(map[string]any)["targetRevision"] = "v2"
	assert.NotEqual(t, Hash(a), Hash(c))
}

func TestGenerationKeptForOtherKinds(t *testing.T) {
	deploy := func(gen int64) map[string]any {
		return map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]any{"name": "web", "namespace": "default", "generation": gen},
			"spec":       map[string]any{"replicas": int64(2)},
		}
	}
	assert.NotEqual(t, Hash(deploy(7)), Hash(deploy(8)), "Deployment generation")

	otherApp := func(gen int64) map[string]any {
		a := argoApp()
		a["apiVersion"] = "example.com/v1"
		a["metadata"].(map[string]any)["generation"] = gen
		return a
	}
	assert.NotEqual(t, Hash(otherApp(7)), Hash(otherApp(8)), "Application outside argoproj.io")
}

func TestReconciledAtKeptForOtherGroups(t *testing.T) {
	a := argoApp()
	a["apiVersion"] = "example.com/v1"
	b := argoApp()
	b["apiVersion"] = "example.com/v1"
	b["status"].(map[string]any)["reconciledAt"] = "2026-11-17T10:03:00Z"
	assert.NotEqual(t, Hash(a), Hash(b))
}

func TestKedaRecommendationAnnotationIgnored(t *testing.T) {
	base := Hash(scaledObject())

	rec := scaledObject()
	rec["metadata"].(map[string]any)["annotations"].(map[string]any)["clm10-keda-scaler/cpuutilizationtarget.recommendation.history"] = `[{"t":"2026-11-17T10:05:00Z","v":65}]`
	assert.Equal(t, base, Hash(rec), "recommendation annotation change")

	other := scaledObject()
	other["metadata"].(map[string]any)["annotations"].(map[string]any)["example.com/owner"] = "team-b"
	assert.NotEqual(t, base, Hash(other), "other annotation change")

	active := scaledObject()
	active["status"].(map[string]any)["lastActiveTime"] = "2026-11-17T10:05:00Z"
	assert.NotEqual(t, base, Hash(active), "status.lastActiveTime change")
}

func TestNoiseRemovalDoesNotMutateInput(t *testing.T) {
	app := argoApp()
	so := scaledObject()
	appBefore := deepCopy(app)
	soBefore := deepCopy(so)

	_ = Normalize(app)
	_ = Normalize(so)

	assert.Equal(t, appBefore, app)
	assert.Equal(t, soBefore, so)
}
