// Package common ...
// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package common

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

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
