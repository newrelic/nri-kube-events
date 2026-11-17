package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"github.com/newrelic/nri-kube-events/pkg/objects"
)

// Secrets are never listed as objects. The API server's table view with includeObject=None
// returns each Secret's name, type and key count and nothing else, so no value and no
// annotation (last-applied-configuration can hold values) reaches this machine. A Secret's
// uid comes only with its metadata, so its row has no uid and no hash.

const tableAccept = "application/json;as=Table;v=v1;g=meta.k8s.io"

var secretsGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

type secretsTable struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	ColumnDefinitions []struct {
		Name string `json:"name"`
	} `json:"columnDefinitions"`
	Rows []struct {
		Cells []any `json:"cells"`
	} `json:"rows"`
}

// listSecretNames returns the names of namespace's Secrets, read page by page from the
// table view with includeObject=None.
func listSecretNames(ctx context.Context, rc rest.Interface, namespace string) ([]string, error) {
	var names []string
	cont := ""
	for {
		req := rc.Get().Namespace(namespace).Resource("secrets").
			Param("includeObject", "None").Param("limit", strconv.Itoa(listLimit)).
			SetHeader("Accept", tableAccept)
		if cont != "" {
			req = req.Param("continue", cont)
		}
		raw, err := req.Do(ctx).Raw()
		if err != nil {
			return nil, fmt.Errorf("listing secrets in %s: %w", namespace, err)
		}
		page, next, err := parseSecretNames(raw)
		if err != nil {
			return nil, fmt.Errorf("listing secrets in %s: %w", namespace, err)
		}
		names = append(names, page...)
		if next == "" {
			return names, nil
		}
		cont = next
	}
}

// parseSecretNames reads the Name column of a Secrets table and its continue token.
func parseSecretNames(raw []byte) (names []string, cont string, err error) {
	var t secretsTable
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, "", fmt.Errorf("decoding secrets table: %w", err)
	}
	col := -1
	for i, c := range t.ColumnDefinitions {
		if c.Name == "Name" {
			col = i
			break
		}
	}
	if col < 0 {
		return nil, "", fmt.Errorf("secrets table has no Name column")
	}
	for _, r := range t.Rows {
		if col >= len(r.Cells) {
			return nil, "", fmt.Errorf("secrets table row has no Name cell")
		}
		name, ok := r.Cells[col].(string)
		if !ok || name == "" {
			return nil, "", fmt.Errorf("secrets table row has no Name")
		}
		names = append(names, name)
	}
	return names, t.Metadata.Continue, nil
}

// secretRows returns a row for each of namespace's Secrets, with no uid and no hash. The
// carrier's own HMAC Secret is left out, because the carrier never sends it.
func secretRows(carrierNamespace, namespace string, names []string) []row {
	out := make([]row, 0, len(names))
	for _, name := range names {
		meta := map[string]any{"metadata": map[string]any{"namespace": namespace, "name": name}}
		if objects.IsOwnStoreObject(carrierNamespace, secretsGVR, meta) {
			continue
		}
		out = append(out, row{kind: "Secret", namespace: namespace, name: name})
	}
	return out
}
