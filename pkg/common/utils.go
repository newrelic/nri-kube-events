// Package common ...
// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package common

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/sirupsen/logrus"
	log "github.com/sirupsen/logrus"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	"k8s.io/kubectl/pkg/scheme"
)

const SplitMaxCols = 16
const NRDBLimit = 4095

func init() {
	err := apiregistrationv1.AddToScheme(scheme.Scheme)
	if err != nil {
		logrus.Warnf("failed to enable identification of built-in resources in apiregistration group: %v", err)
	}

	err = apiextensionsv1.AddToScheme(scheme.Scheme)
	if err != nil {
		logrus.Warnf("failed to enable identification of built-in resources in apiextensions group: %v", err)
	}
}

// LimitSplit splits the input string into multiple strings at the specified limit
// taking care not to split mid-rune.
func LimitSplit(input string, limit int) []string {
	if limit <= 0 {
		return []string{input}
	}

	var splits []string
	for len(input) > limit {
		boundary := limit
		// Check if this is a run boundary, else go backwards upto UTFMax bytes to look for
		// a boundary. If one isn't found in max bytes, give up and split anyway.
		for !utf8.RuneStart(input[boundary]) && boundary >= limit-utf8.UTFMax {
			boundary--
		}
		splits = append(splits, input[:boundary])
		input = input[boundary:]
	}
	if len(input) > 0 {
		splits = append(splits, input)
	}
	return splits
}

func IsBuiltInResource(gvk schema.GroupVersionKind) bool {
	return scheme.Scheme.Recognizes(gvk)
}

// K8SObjGetGVK gets the GVK for the given object.
func K8SObjGetGVK(obj runtime.Object) schema.GroupVersionKind {
	gvks, _, err := scheme.Scheme.ObjectKinds(obj)
	if err != nil {
		log.Warnf("missing apiVersion or kind and cannot assign it; %v", err)
		return schema.GroupVersionKind{}
	}

	for _, gvk := range gvks {
		if len(gvk.Kind) == 0 {
			continue
		}
		if len(gvk.Version) == 0 || gvk.Version == runtime.APIVersionInternal {
			continue
		}
		return gvk
	}
	return schema.GroupVersionKind{}
}

func GetObjNamespaceAndName(obj runtime.Object) (string, string, error) {
	accessor := meta.NewAccessor()
	var errs []error

	ns, err := accessor.Namespace(obj)
	errs = append(errs, err)

	name, err := accessor.Name(obj)
	errs = append(errs, err)

	return ns, name, errors.Join(errs...)
}

func FlattenStruct(v interface{}) (map[string]interface{}, error) {
	m := make(map[string]interface{})

	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var unflattened map[string]interface{}
	err = json.Unmarshal(data, &unflattened)
	if err != nil {
		return nil, err
	}

	var doFlatten func(string, interface{}, map[string]interface{})

	doFlatten = func(key string, v interface{}, m map[string]interface{}) {
		switch parsedType := v.(type) {
		case map[string]interface{}:
			for k, n := range parsedType {
				doFlatten(key+"."+k, n, m)
			}
		case []interface{}:
			for i, n := range parsedType {
				doFlatten(key+fmt.Sprintf("[%d]", i), n, m)
			}
		case string:
			// ignore empty strings
			if parsedType == "" {
				return
			}

			m[key] = v

		default:
			// ignore nil values
			if v == nil {
				return
			}

			m[key] = v
		}
	}

	for k, v := range unflattened {
		doFlatten(k, v, m)
	}

	return m, nil
}
