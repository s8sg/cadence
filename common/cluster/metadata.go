// Copyright (c) 2018 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package cluster

import (
	"fmt"
	"github.com/uber-common/bark"
	"github.com/uber/cadence/common/logging"
	"github.com/uber/cadence/common/metrics"

	"github.com/uber/cadence/common/service/config"
	"github.com/uber/cadence/common/service/dynamicconfig"
)

type (
	// Metadata provides information about clusters
	Metadata interface {
		// IsGlobalDomainEnabled whether the global domain is enabled,
		// this attr should be discarded when cross DC is made public
		IsGlobalDomainEnabled() bool
		// IsMasterCluster whether current cluster is master cluster
		IsMasterCluster() bool
		// GetNextFailoverVersion return the next failover version for domain failover
		GetNextFailoverVersion(string, int64) int64
		// IsVersionFromSameCluster return true if 2 version are used for the same cluster
		IsVersionFromSameCluster(version1 int64, version2 int64) bool
		// GetMasterClusterName return the master cluster name
		GetMasterClusterName() string
		// GetCurrentClusterName return the current cluster name
		GetCurrentClusterName() string
		// GetAllClusterFailoverVersions return the all cluster name -> corresponding initial failover version
		GetAllClusterFailoverVersions() map[string]int64
		// ClusterNameForFailoverVersion return the corresponding cluster name for a given failover version
		ClusterNameForFailoverVersion(failoverVersion int64) string
		// GetAllClientAddress return the frontend address for each cluster name
		GetAllClientAddress() map[string]config.Address

		// ArchivalConfig returns the archival config of the cluster
		ArchivalConfig() *ArchivalConfig

		// TODO remove after DC migration is over
		IsClusterDisabled(cluster string) bool
	}

	metadataImpl struct {
		logger        bark.Logger
		metricsClient metrics.Client
		// EnableGlobalDomain whether the global domain is enabled,
		// this attr should be discarded when cross DC is made public
		enableGlobalDomain dynamicconfig.BoolPropertyFn
		// failoverVersionIncrement is the increment of each cluster failover version
		failoverVersionIncrement int64
		// masterClusterName is the name of the master cluster, only the master cluster can register / update domain
		// all clusters can do domain failover
		masterClusterName string
		// currentClusterName is the name of the current cluster
		currentClusterName string
		// clusterInitialFailoverVersions contains all cluster name -> corresponding initial failover version
		clusterInitialFailoverVersions map[string]int64
		// clusterInitialFailoverVersions contains all initial failover version -> corresponding cluster name
		initialFailoverVersionClusters map[int64]string
		// clusterToAddress contains the cluster name to corresponding frontend client
		clusterToAddress map[string]config.Address
		// archivalStatus is cluster's archival status
		archivalStatus dynamicconfig.StringPropertyFn
		// defaultBucket is the default archival bucket name used for this cluster
		defaultBucket string
		// TODO remove after DC migration is over
		clustersDisabled map[string]struct{}
	}
)

// NewMetadata create a new instance of Metadata
func NewMetadata(
	logger bark.Logger,
	metricsClient metrics.Client,
	enableGlobalDomain dynamicconfig.BoolPropertyFn,
	failoverVersionIncrement int64,
	masterClusterName string,
	currentClusterName string,
	clusterInitialFailoverVersions map[string]int64,
	clusterToAddress map[string]config.Address,
	archivalStatus dynamicconfig.StringPropertyFn,
	defaultBucket string,
	clustersDisabled map[string]struct{},
) Metadata {

	if len(clusterInitialFailoverVersions) == 0 {
		panic("Empty initial failover versions for cluster")
	} else if len(masterClusterName) == 0 {
		panic("Master cluster name is empty")
	} else if len(currentClusterName) == 0 {
		panic("Current cluster name is empty")
	}
	initialFailoverVersionClusters := make(map[int64]string)
	for clusterName, initialFailoverVersion := range clusterInitialFailoverVersions {
		if failoverVersionIncrement <= initialFailoverVersion {
			panic(fmt.Sprintf(
				"Failover version increment %v is smaller than initial value: %v.",
				failoverVersionIncrement,
				clusterInitialFailoverVersions,
			))
		}
		if len(clusterName) == 0 {
			panic("Cluster name in all cluster names is empty")
		}
		initialFailoverVersionClusters[initialFailoverVersion] = clusterName
	}

	if _, ok := clusterInitialFailoverVersions[currentClusterName]; !ok {
		panic("Current cluster is not specified in all cluster names")
	}
	if _, ok := clusterInitialFailoverVersions[masterClusterName]; !ok {
		panic("Master cluster is not specified in all cluster names")
	}
	if len(initialFailoverVersionClusters) != len(clusterInitialFailoverVersions) {
		panic("Cluster to initial failover versions have duplicate initial versions")
	}

	// only check whether a cluster in cluster -> initial failover versions exists in cluster -> address
	for clusterName := range clusterInitialFailoverVersions {
		if _, ok := clusterToAddress[clusterName]; !ok {
			panic("Cluster -> initial failover version does not have an address")
		}
	}

	status, err := GetArchivalStatus(archivalStatus())
	if err != nil {
		panic(err)
	}
	archivalConfig := NewArchivalConfig(status, defaultBucket)
	if !archivalConfig.IsValid() {
		panic("Archival config is not valid")
	}

	return &metadataImpl{
		logger:                         logger,
		metricsClient:                  metricsClient,
		enableGlobalDomain:             enableGlobalDomain,
		failoverVersionIncrement:       failoverVersionIncrement,
		masterClusterName:              masterClusterName,
		currentClusterName:             currentClusterName,
		clusterInitialFailoverVersions: clusterInitialFailoverVersions,
		initialFailoverVersionClusters: initialFailoverVersionClusters,
		clusterToAddress:               clusterToAddress,
		archivalStatus:                 archivalStatus,
		defaultBucket:                  defaultBucket,
		clustersDisabled:               clustersDisabled,
	}
}

// IsGlobalDomainEnabled whether the global domain is enabled,
// this attr should be discarded when cross DC is made public
func (metadata *metadataImpl) IsGlobalDomainEnabled() bool {
	return metadata.enableGlobalDomain()
}

// GetNextFailoverVersion return the next failover version based on input
func (metadata *metadataImpl) GetNextFailoverVersion(cluster string, currentFailoverVersion int64) int64 {
	initialFailoverVersion, ok := metadata.clusterInitialFailoverVersions[cluster]
	if !ok {
		panic(fmt.Sprintf(
			"Unknown cluster name: %v with given cluster initial failover version map: %v.",
			cluster,
			metadata.clusterInitialFailoverVersions,
		))
	}
	failoverVersion := currentFailoverVersion/metadata.failoverVersionIncrement*metadata.failoverVersionIncrement + initialFailoverVersion
	if failoverVersion < currentFailoverVersion {
		return failoverVersion + metadata.failoverVersionIncrement
	}
	return failoverVersion
}

// IsVersionFromSameCluster return true if 2 version are used for the same cluster
func (metadata *metadataImpl) IsVersionFromSameCluster(version1 int64, version2 int64) bool {
	return (version1-version2)%metadata.failoverVersionIncrement == 0
}

func (metadata *metadataImpl) IsMasterCluster() bool {
	return metadata.masterClusterName == metadata.currentClusterName
}

// GetMasterClusterName return the master cluster name
func (metadata *metadataImpl) GetMasterClusterName() string {
	return metadata.masterClusterName
}

// GetCurrentClusterName return the current cluster name
func (metadata *metadataImpl) GetCurrentClusterName() string {
	return metadata.currentClusterName
}

// GetAllClusterFailoverVersions return the all cluster name -> corresponding initial failover version
func (metadata *metadataImpl) GetAllClusterFailoverVersions() map[string]int64 {
	return metadata.clusterInitialFailoverVersions
}

// ClusterNameForFailoverVersion return the corresponding cluster name for a given failover version
func (metadata *metadataImpl) ClusterNameForFailoverVersion(failoverVersion int64) string {
	initialFailoverVersion := failoverVersion % metadata.failoverVersionIncrement
	clusterName, ok := metadata.initialFailoverVersionClusters[initialFailoverVersion]
	if !ok {
		panic(fmt.Sprintf(
			"Unknown initial failover version %v with given cluster initial failover version map: %v and failover version increment %v.",
			initialFailoverVersion,
			metadata.clusterInitialFailoverVersions,
			metadata.failoverVersionIncrement,
		))
	}
	return clusterName
}

// GetAllClientAddress return the frontend address for each cluster name
func (metadata *metadataImpl) GetAllClientAddress() map[string]config.Address {
	return metadata.clusterToAddress
}

// ArchivalConfig returns the archival config of the cluster.
// This method always return a well formed ArchivalConfig of which there are only three forms:
// 1. ArchivalDisabled and empty DefaultBucket
// 2. ArchivalPaused and non-empty DefaultBucket
// 3. ArchivalEnabled and non-empty DefaultBucket
func (metadata *metadataImpl) ArchivalConfig() (retCfg *ArchivalConfig) {
	status, err := GetArchivalStatus(metadata.archivalStatus())
	if err != nil {
		metadata.logger.WithFields(bark.Fields{
			logging.TagClusterArchivalStatus: metadata.archivalStatus(),
			logging.TagErr:                   err,
		}).Error("error getting archival config, invalid archival status in dynamic config")
		metadata.metricsClient.IncCounter(metrics.ClusterMetadataArchivalConfigScope, metrics.ArchivalConfigFailures)
	}
	return NewArchivalConfig(status, metadata.defaultBucket)
}

// IsClusterDisabled return whether cluster is disabled
func (metadata *metadataImpl) IsClusterDisabled(cluster string) bool {
	_, ok := metadata.clustersDisabled[cluster]
	return ok
}
