package objects

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Health is the generic health of an object, derived from its status alone.
type Health struct {
	// ConditionsUnhealthy lists every bad condition as ",Type=Status:Reason,", sorted by
	// type, and holds at most PartChars runes and at most MaxAttrBytes bytes. It is empty
	// when no condition is bad, or when the first bad condition alone exceeds either limit.
	// A Succeeded object's completed readiness conditions are left out.
	ConditionsUnhealthy string
	// ConditionsTruncated is set when ConditionsUnhealthy dropped bad conditions to stay
	// within PartChars runes and MaxAttrBytes bytes.
	ConditionsTruncated bool
	// Stale is true when the controller has not yet observed the latest generation. It is
	// false for an object with metadata.deletionTimestamp set, and for an object with no spec.
	Stale bool
	// Unhealthy summarizes the other fields and status.health.status.
	Unhealthy bool
	// Phase is status.phase, or "" when the object has none.
	Phase string
	// Ready is the status of the Ready condition, else of the Available condition, else nil.
	Ready *bool
}

// negativeExact are condition types whose True status is bad, compared case-insensitively
// against the whole type name.
var negativeExact = map[string]bool{
	"replicafailure":      true,
	"scalinglimited":      true,
	"memorypressure":      true,
	"diskpressure":        true,
	"pidpressure":         true,
	"networkunavailable":  true,
	"fallback":            true,
	"nonstructuralschema": true,
	"disruptiontarget":    true,
	"deleting":            true,
	"activeincident":      true,
	"activefreeze":        true,
	// A FlowSchema is Dangling=True when its priority level does not exist.
	"dangling": true,
}

// neutralTypes are condition types that never count for health, whatever their status.
// They are checked before polarity. NonStructuralSchema=True on a CRD is a schema-quality
// warning, not an incident.
var neutralTypes = map[string]bool{
	"nonstructuralschema": true,
}

// negativeWords mark a condition type as negative when any CamelCase word of the type name
// equals one of them, compared case-insensitively. A substring match would make
// ProviderInstalled negative, because "Installed" contains "stalled"; its words are Provider
// and Installed, so the word match keeps it positive.
var negativeWords = map[string]bool{
	"failure":     true,
	"failed":      true,
	"error":       true,
	"degraded":    true,
	"stalled":     true,
	"pressure":    true,
	"unavailable": true,
	"drifted":     true,
	"expired":     true,
	"invalid":     true,
}

// transientTypes describe an action in progress. Neither status of them is bad.
var transientTypes = map[string]bool{
	"paused":      true,
	"remediating": true,
	"rollingout":  true,
	"scalingup":   true,
	"scalingdown": true,
}

// completedReadiness are the readiness conditions a Pod sets False with reason PodCompleted
// once it has run to completion. When status.phase is Succeeded they are not bad.
var completedReadiness = map[string]bool{
	"Ready":                     true,
	"ContainersReady":           true,
	"PodReadyToStartContainers": true,
}

// IsNegativeCondition reports whether a True status of conditionType means a problem.
func IsNegativeCondition(conditionType string) bool {
	if conditionType == "" {
		return false
	}
	if negativeExact[strings.ToLower(conditionType)] {
		return true
	}
	for _, w := range camelWords(conditionType) {
		if negativeWords[strings.ToLower(w)] {
			return true
		}
	}
	return false
}

// camelWords splits a CamelCase name at lower-to-upper boundaries and at the end of an
// acronym: "PIDPressure" gives PID, Pressure; "InvalidSpecError" gives Invalid, Spec, Error.
func camelWords(s string) []string {
	r := []rune(s)
	var words []string
	start := 0
	for i := 1; i < len(r); i++ {
		prev, cur := r[i-1], r[i]
		boundary := false
		switch {
		case unicode.IsUpper(cur) && (unicode.IsLower(prev) || unicode.IsDigit(prev)):
			boundary = true
		case unicode.IsUpper(cur) && unicode.IsUpper(prev) && i+1 < len(r) && unicode.IsLower(r[i+1]):
			boundary = true
		}
		if boundary {
			words = append(words, string(r[start:i]))
			start = i
		}
	}
	return append(words, string(r[start:]))
}

// conditionIsBad applies the polarity rule. A negative type is bad when True, and also when
// it has no status (Argo CD sets InvalidSpecError without one). Every other type is bad when
// False or Unknown. Neutral and transient types are never bad.
func conditionIsBad(typ, status string) bool {
	if neutralTypes[strings.ToLower(typ)] || transientTypes[strings.ToLower(typ)] {
		return false
	}
	if IsNegativeCondition(typ) {
		return status == "" || status == "True"
	}
	return status == "False" || status == "Unknown"
}

// ComputeHealth derives Health from obj's metadata.generation and status. It never modifies obj.
func ComputeHealth(obj map[string]any) Health {
	var h Health
	meta, _ := obj["metadata"].(map[string]any)
	status, _ := obj["status"].(map[string]any)
	generation, hasGeneration := number(meta["generation"])
	// Deleting an object can bump metadata.generation without the controller observing it
	// (a Pod does), so a terminating object is never stale.
	if ts, _ := meta["deletionTimestamp"].(string); ts != "" {
		hasGeneration = false
	}
	// metadata.generation counts spec changes. Without a spec there is nothing for a
	// controller to observe, and an observedGeneration in status describes another object:
	// a gatekeeper ConstraintPodStatus carries the generation of the constraint it reports on.
	if _, hasSpec := obj["spec"]; !hasSpec {
		hasGeneration = false
	}

	h.Phase, _ = status["phase"].(string)

	if hasGeneration {
		if observed, ok := number(status["observedGeneration"]); ok && observed < generation {
			h.Stale = true
		}
	}

	type badCondition struct{ typ, status, reason string }
	var bad []badCondition
	var ready, available *bool

	conds, _ := status["conditions"].([]any)
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := cm["type"].(string)
		st, _ := cm["status"].(string)
		reason, _ := cm["reason"].(string)

		if hasGeneration {
			if observed, ok := number(cm["observedGeneration"]); ok && observed < generation {
				h.Stale = true
			}
		}
		if typ == "" {
			continue
		}
		if conditionIsBad(typ, st) && !(h.Phase == "Succeeded" && completedReadiness[typ]) {
			bad = append(bad, badCondition{typ, st, reason})
		}
		switch typ {
		case "Ready":
			v := st == "True"
			ready = &v
		case "Available":
			v := st == "True"
			available = &v
		}
	}

	sort.SliceStable(bad, func(i, j int) bool { return bad[i].typ < bad[j].typ })
	// Past PartChars runes or MaxAttrBytes bytes, whole conditions are dropped from the end,
	// so the string still ends in "," and LIKE '%,Type=%' never matches a cut condition.
	var b strings.Builder
	runes := 0
	for _, c := range bad {
		item := "," + c.typ + "=" + c.status + ":" + c.reason + ","
		n := utf8.RuneCountInString(item)
		if runes+n > PartChars || b.Len()+len(item) > MaxAttrBytes {
			h.ConditionsTruncated = true
			break
		}
		b.WriteString(item)
		runes += n
	}
	h.ConditionsUnhealthy = b.String()

	if ready != nil {
		h.Ready = ready
	} else {
		h.Ready = available
	}

	healthStatus := ""
	if hs, ok := status["health"].(map[string]any); ok {
		healthStatus, _ = hs["status"].(string)
	}

	h.Unhealthy = len(bad) > 0 ||
		h.Stale ||
		h.Phase == "Failed" || h.Phase == "Unknown" ||
		healthStatus == "Degraded" || healthStatus == "Missing" || healthStatus == "Unknown"
	return h
}

// number reads a JSON number that a decoder may have produced as float64 or int64, or that
// unstructured.Unstructured holds as int64.
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	default:
		return 0, false
	}
}
