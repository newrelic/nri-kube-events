// Package objects carries Kubernetes object state to New Relic when an object changes.
package objects

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// leaderAnnotation is the legacy leader-election record. Its holder renews it every few
// seconds, so it changes without any change in the object it annotates.
const leaderAnnotation = "control-plane.alpha.kubernetes.io/leader"

// leaseGroup is the API group of the Lease kind whose spec.renewTime is dropped.
const leaseGroup = "coordination.k8s.io"

// argoGroup is the API group of the Argo CD Application kind whose status.reconciledAt
// and metadata.generation are dropped.
const argoGroup = "argoproj.io"

// kedaGroup is the API group of the KEDA ScaledObject kind whose recommendation
// annotation is dropped.
const kedaGroup = "keda.sh"

// kedaRecommendationAnnotation is the recommendation history clm10-keda-scaler writes on
// ScaledObjects. It changes far more often than the scaler's configuration.
const kedaRecommendationAnnotation = "clm10-keda-scaler/cpuutilizationtarget.recommendation.history"

// Normalize returns a deep copy of obj without the fields that change on every heartbeat
// or write but carry no state: metadata.managedFields, metadata.resourceVersion, the
// leader-election annotation, lastHeartbeatTime in every status.conditions entry,
// spec.renewTime of a coordination.k8s.io Lease, status.reconciledAt and
// metadata.generation of an argoproj.io Application, and the
// clm10-keda-scaler/cpuutilizationtarget.recommendation.history annotation of a keda.sh
// ScaledObject. The input is never modified.
//
// The Argo CD and KEDA fields come from an ingest measurement on a staging cluster: every
// Argo CD refresh changes only reconciledAt and generation, and most ScaledObject changes
// touch only the recommendation annotation. Dropping generation means a record can show an older
// generation than the live Application until its next real change or the next resend.
func Normalize(obj map[string]any) map[string]any {
	out, _ := deepCopy(obj).(map[string]any)
	if out == nil {
		return map[string]any{}
	}

	if meta, ok := out["metadata"].(map[string]any); ok {
		delete(meta, "managedFields")
		delete(meta, "resourceVersion")
		if ann, ok := meta["annotations"].(map[string]any); ok {
			delete(ann, leaderAnnotation)
		}
	}

	if status, ok := out["status"].(map[string]any); ok {
		if conds, ok := status["conditions"].([]any); ok {
			for _, c := range conds {
				if cm, ok := c.(map[string]any); ok {
					delete(cm, "lastHeartbeatTime")
				}
			}
		}
	}

	if isKind(out, leaseGroup, "Lease") {
		if spec, ok := out["spec"].(map[string]any); ok {
			delete(spec, "renewTime")
		}
	}

	if isKind(out, argoGroup, "Application") {
		if status, ok := out["status"].(map[string]any); ok {
			delete(status, "reconciledAt")
		}
		if meta, ok := out["metadata"].(map[string]any); ok {
			delete(meta, "generation")
		}
	}

	if isKind(out, kedaGroup, "ScaledObject") {
		if meta, ok := out["metadata"].(map[string]any); ok {
			if ann, ok := meta["annotations"].(map[string]any); ok {
				delete(ann, kedaRecommendationAnnotation)
			}
		}
	}

	return out
}

// HealthRuleVersion is the version of the rules that derive the health attributes of a
// record (health.go). Hash folds it in, so raising it after a rule change makes the next
// carrier re-send every object once, and no stored record keeps attributes derived by the
// old rules.
const HealthRuleVersion = 2

// Hash returns the first 16 hex characters of the SHA-256 of "health-rules-v<N>\n"
// followed by the normalized object's JSON, where N is HealthRuleVersion. encoding/json
// writes map keys in sorted order, so the hash does not depend on key order.
func Hash(obj map[string]any) string {
	return hashWithRuleVersion(obj, HealthRuleVersion)
}

func hashWithRuleVersion(obj map[string]any, version int) string {
	b, err := json.Marshal(Normalize(obj))
	if err != nil {
		// Only values that JSON cannot encode (NaN, Inf, channels) reach this branch; a
		// dynamic-client object never holds them. fmt also prints maps in sorted key order,
		// so the fallback hash is still stable and still changes when the content changes.
		b = []byte(fmt.Sprintf("%#v", Normalize(obj)))
	}
	sum := sha256.Sum256(append([]byte(fmt.Sprintf("health-rules-v%d\n", version)), b...))
	return hex.EncodeToString(sum[:])[:16]
}

// isKind reports whether obj has the given kind in the given API group, read from the
// apiVersion prefix.
func isKind(obj map[string]any, group, kind string) bool {
	k, _ := obj["kind"].(string)
	apiVersion, _ := obj["apiVersion"].(string)
	return k == kind && strings.HasPrefix(apiVersion, group+"/")
}

// deepCopy copies the container types that JSON decoding produces. Scalars are immutable
// and are returned as they are.
func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if t == nil {
			return map[string]any(nil)
		}
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = deepCopy(val)
		}
		return m
	case []any:
		if t == nil {
			return []any(nil)
		}
		s := make([]any, len(t))
		for i, val := range t {
			s[i] = deepCopy(val)
		}
		return s
	default:
		return v
	}
}
