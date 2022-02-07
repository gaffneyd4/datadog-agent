// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package pb

// TargetTPS contains the targeted traces per second the agent should try to sample for a particular service and env
type TargetTPS struct {
	Service string `msgpack:"0"`
	Env     string `msgpack:"1"`
	// Value contains the targetTPS value to apply (target traces per second).
	Value float64 `msgpack:"2"`
	// Rank is the rank associated to this TargetTPS. Lower ranks of a same (env, service) are discarded
	// in favor of the highest rank.
	Rank uint32 `msgpack:"3"`
	// Mechanism is the identifier of the mechanism that generated this TargetTPS
	Mechanism uint32 `msgpack:"4"`
}

// APMSampling is the list of target tps
type APMSampling struct {
	TargetTPS []TargetTPS `msgpack:"0"`
}
