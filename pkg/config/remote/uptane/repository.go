// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022-present Datadog, Inc.

package uptane

import (
	"github.com/DataDog/datadog-agent/pkg/config/remote/meta"
	"github.com/theupdateframework/go-tuf/client"
	"go.etcd.io/bbolt"
)

type directorRepository struct {
	localStore  *localStore
	remoteStore *remoteStoreDirector
	tufClient   *client.Client
}

type configRepository struct {
	localStore  *localStore
	remoteStore *remoteStoreConfig
	tufClient   *client.Client
}

func newDirectorRepository(cacheDB *bbolt.DB, cacheKey string, targetStore *targetStore, roots meta.EmbeddedRoots) (*directorRepository, error) {
	localStore, err := newLocalStoreDirector(cacheDB, cacheKey, roots)
	if err != nil {
		return nil, err
	}
	remoteStore := newRemoteStoreDirector(targetStore)
	tufClient := client.NewClient(localStore, remoteStore)
	return &directorRepository{
		localStore:  localStore,
		remoteStore: remoteStore,
		tufClient:   tufClient,
	}, nil
}

func newConfigRepository(cacheDB *bbolt.DB, cacheKey string, targetStore *targetStore, roots meta.EmbeddedRoots) (*configRepository, error) {
	localStore, err := newLocalStoreConfig(cacheDB, cacheKey, roots)
	if err != nil {
		return nil, err
	}
	remoteStore := newRemoteStoreConfig(targetStore)
	tufClient := client.NewClient(localStore, remoteStore)
	return &configRepository{
		localStore:  localStore,
		remoteStore: remoteStore,
		tufClient:   tufClient,
	}, nil
}
