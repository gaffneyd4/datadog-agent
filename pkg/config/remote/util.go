// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022-present Datadog, Inc.

package remote

import (
	"encoding/json"
	"fmt"
)

type versionCustom struct {
	Version *uint64 `json:"v"`
}

func targetVersion(custom *json.RawMessage) (uint64, error) {
	if custom == nil {
		return 0, fmt.Errorf("custom is nil")
	}
	var version versionCustom
	err := json.Unmarshal(*custom, &version)
	if err != nil {
		return 0, err
	}
	if version.Version == nil {
		return 0, fmt.Errorf("custom.v is not defined, could not get target version")
	}
	return *version.Version, nil
}
