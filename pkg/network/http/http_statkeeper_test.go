// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build linux_bpf || (windows && npm)
// +build linux_bpf windows,npm

package http

import (
	"regexp"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/DataDog/datadog-agent/pkg/network/config"
	"github.com/DataDog/datadog-agent/pkg/process/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessHTTPTransactions(t *testing.T) {
	cfg := &config.Config{MaxHTTPStatsBuffered: 1000}
	sk := newHTTPStatkeeper(cfg, newTelemetry())
	txs := make([]httpTX, 100)

	sourceIP := util.AddressFromString("1.1.1.1")
	sourcePort := 1234
	destIP := util.AddressFromString("2.2.2.2")
	destPort := 8080

	const numPaths = 10
	for i := 0; i < numPaths; i++ {
		path := "/testpath" + strconv.Itoa(i)

		for j := 0; j < 10; j++ {
			statusCode := (j%5 + 1) * 100
			latency := time.Duration(j%5) * time.Millisecond
			txs[i*10+j] = generateIPv4HTTPTransaction(sourceIP, destIP, sourcePort, destPort, path, statusCode, latency)
		}
	}

	sk.Process(txs)

	stats := sk.GetAndResetAllStats()
	assert.Equal(t, 0, len(sk.stats))
	assert.Equal(t, numPaths, len(stats))
	for key, stats := range stats {
		assert.Equal(t, "/testpath", key.Path[:9])
		for i := 0; i < 5; i++ {
			assert.Equal(t, 2, stats[i].Count)
			assert.Equal(t, 2.0, stats[i].Latencies.GetCount())

			p50, err := stats[i].Latencies.GetValueAtQuantile(0.5)
			assert.Nil(t, err)

			expectedLatency := float64(time.Duration(i) * time.Millisecond)
			acceptableError := expectedLatency * stats[i].Latencies.IndexMapping.RelativeAccuracy()
			assert.True(t, p50 >= expectedLatency-acceptableError)
			assert.True(t, p50 <= expectedLatency+acceptableError)
		}
	}
}

func BenchmarkProcessSameConn(b *testing.B) {
	cfg := &config.Config{MaxHTTPStatsBuffered: 1000}
	sk := newHTTPStatkeeper(cfg, newTelemetry())
	tx := generateIPv4HTTPTransaction(
		util.AddressFromString("1.1.1.1"),
		util.AddressFromString("2.2.2.2"),
		1234,
		8080,
		"foobar",
		404,
		30*time.Millisecond,
	)
	transactions := []httpTX{tx}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sk.Process(transactions)
	}
}

func TestGetPath(t *testing.T) {
	requestFragment := []byte("GET /foo/bar?var1=value HTTP/1.1\nHost: example.com\nUser-Agent: example-browser/1.0")
	b := make([]byte, len(requestFragment))
	assert.Equal(t, "/foo/bar", string(getPath(requestFragment, b)))
}

func TestGetPathHandlesNullTerminator(t *testing.T) {
	requestFragment := []byte("GET /foo/\x00bar?var1=value HTTP/1.1\nHost: example.com\nUser-Agent: example-browser/1.0")
	b := make([]byte, len(requestFragment))
	assert.Equal(t, "/foo/", string(getPath(requestFragment, b)))
}

func BenchmarkGetPath(b *testing.B) {
	requestFragment := []byte("GET /foo/bar?var1=value HTTP/1.1\nHost: example.com\nUser-Agent: example-browser/1.0")

	b.ReportAllocs()
	b.ResetTimer()
	buf := make([]byte, len(requestFragment))
	for i := 0; i < b.N; i++ {
		_ = getPath(requestFragment, buf)
	}
	runtime.KeepAlive(buf)
}

func TestPathProcessing(t *testing.T) {
	var (
		sourceIP   = util.AddressFromString("1.1.1.1")
		sourcePort = 1234
		destIP     = util.AddressFromString("2.2.2.2")
		destPort   = 8080
		statusCode = 200
		latency    = time.Second
	)

	setupStatKeeper := func(rules []*config.ReplaceRule) *httpStatKeeper {
		c := &config.Config{
			MaxHTTPStatsBuffered: 1000,
			HTTPReplaceRules:     rules,
		}

		return newHTTPStatkeeper(c, newTelemetry())
	}

	t.Run("reject rule", func(t *testing.T) {
		rules := []*config.ReplaceRule{
			{
				Re: regexp.MustCompile("payment"),
			},
		}

		sk := setupStatKeeper(rules)
		transactions := []httpTX{
			generateIPv4HTTPTransaction(sourceIP, destIP, sourcePort, destPort, "/foobar", statusCode, latency),
			generateIPv4HTTPTransaction(sourceIP, destIP, sourcePort, destPort, "/payment/123", statusCode, latency),
		}
		sk.Process(transactions)
		stats := sk.GetAndResetAllStats()

		require.Len(t, stats, 1)
		for key := range stats {
			assert.Equal(t, "/foobar", key.Path)
		}
	})

	t.Run("replace rule", func(t *testing.T) {
		rules := []*config.ReplaceRule{
			{
				Re:   regexp.MustCompile("/users/.*"),
				Repl: "/users/?",
			},
		}

		sk := setupStatKeeper(rules)
		transactions := []httpTX{
			generateIPv4HTTPTransaction(sourceIP, destIP, sourcePort, destPort, "/prefix/users/1", statusCode, latency),
			generateIPv4HTTPTransaction(sourceIP, destIP, sourcePort, destPort, "/prefix/users/2", statusCode, latency),
			generateIPv4HTTPTransaction(sourceIP, destIP, sourcePort, destPort, "/prefix/users/3", statusCode, latency),
		}
		sk.Process(transactions)
		stats := sk.GetAndResetAllStats()

		require.Len(t, stats, 1)
		for key, metrics := range stats {
			assert.Equal(t, "/prefix/users/?", key.Path)
			assert.Equal(t, 3, metrics[statusCode/100-1].Count)
		}
	})

	t.Run("chained rules", func(t *testing.T) {
		rules := []*config.ReplaceRule{
			{
				Re:   regexp.MustCompile("/users/[A-z0-9]+"),
				Repl: "/users/?",
			},
			{
				Re:   regexp.MustCompile("/payment/[0-9]+"),
				Repl: "/payment/?",
			},
		}

		sk := setupStatKeeper(rules)
		transactions := []httpTX{
			generateIPv4HTTPTransaction(sourceIP, destIP, sourcePort, destPort, "/users/ana/payment/123", statusCode, latency),
			generateIPv4HTTPTransaction(sourceIP, destIP, sourcePort, destPort, "/users/bob/payment/456", statusCode, latency),
		}
		sk.Process(transactions)
		stats := sk.GetAndResetAllStats()

		require.Len(t, stats, 1)
		for key, metrics := range stats {
			assert.Equal(t, "/users/?/payment/?", key.Path)
			assert.Equal(t, 2, metrics[statusCode/100-1].Count)
		}
	})
}
