// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022-present Datadog, Inc.

package remote

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTrimHashTargetPath(t *testing.T) {
	tests := []struct {
		input  json.RawMessage
		err    bool
		output uint64
	}{
		{input: json.RawMessage(`{"v":2,"b":"abcd"}`), output: 2},
		{input: json.RawMessage(`{"b":"abcd"}`), err: true},
	}
	for _, test := range tests {
		t.Run(string(test.input), func(tt *testing.T) {
			output, err := targetVersion(&test.input)
			if test.err {
				assert.Error(tt, err)
			} else {
				assert.NoError(tt, err)
				assert.Equal(tt, test.output, output)
			}

		})
	}
}
