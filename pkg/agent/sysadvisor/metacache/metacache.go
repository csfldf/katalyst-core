/*
Copyright 2022 The Katalyst Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metacache

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager/errors"

	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/types"
	"github.com/kubewharf/katalyst-core/pkg/config"
	"github.com/kubewharf/katalyst-core/pkg/metaserver/agent/metric"
	"github.com/kubewharf/katalyst-core/pkg/util/machine"
)

// [notice]
// to compatible with checkpoint checksum calculation,
// we should make guarantees below in checkpoint properties assignment
// 1. resource.Quantity use resource.MustParse("0") to initialize, not to use resource.Quantity{}
// 2. CPUSet use NewCPUSet(...) to initialize, not to use CPUSet{}
// 3. not use omitempty in map property and must make new map to do initialization

const (
	stateFileName             string = "sys_advisor_state"
	storeStateWarningDuration        = 2 * time.Second
)

// MetaReader provides a standard interface to refer to metadata type
type MetaReader interface {
	// GetContainerEntries returns a ContainerEntry copy keyed by pod uid
	GetContainerEntries(podUID string) (types.ContainerEntries, bool)
	// GetContainerInfo returns a ContainerInfo copy keyed by pod uid and container name
	GetContainerInfo(podUID string, containerName string) (*types.ContainerInfo, bool)
	// GetContainerMetric returns the metric value of a container
	GetContainerMetric(podUID string, containerName string, metricName string) (float64, error)
	// RangeContainer applies a function to every podUID, containerName, containerInfo set
	RangeContainer(f func(podUID string, containerName string, containerInfo *types.ContainerInfo) bool)

	// GetPoolInfo returns a PoolInfo copy by pool name
	GetPoolInfo(poolName string) (*types.PoolInfo, bool)
	// GetPoolSize returns the size of pool as integer
	GetPoolSize(poolName string) (int, bool)

	// GetRegionInfo returns a RegionInfo copy by region name
	GetRegionInfo(regionName string) (*types.RegionInfo, bool)
	// RangeRegionInfo applies a function to every regionName, regionInfo set
	RangeRegionInfo(f func(regionName string, regionInfo *types.RegionInfo) bool)
}

// RawMetaWriter provides a standard interface to modify raw metadata (generated by other agents) in local cache
type RawMetaWriter interface {
	// AddContainer adds a container keyed by pod uid and container name. For repeatedly added
	// container, only mutable metadata will be updated, i.e. request quantity changed by vpa
	AddContainer(podUID string, containerName string, containerInfo *types.ContainerInfo) error
	// SetContainerInfo updates ContainerInfo keyed by pod uid and container name
	SetContainerInfo(podUID string, containerName string, containerInfo *types.ContainerInfo) error
	// RangeAndUpdateContainer applies a function to every podUID, containerName, containerInfo set.
	// Not recommended to use if RangeContainer satisfies the requirement.
	// If f returns false, range stops the iteration.
	RangeAndUpdateContainer(f func(podUID string, containerName string, containerInfo *types.ContainerInfo) bool)
	// RangeAndDeleteContainer applies a function to every podUID, containerName, containerInfo set.
	// If f returns true, the containerInfo will be deleted.
	RangeAndDeleteContainer(f func(containerInfo *types.ContainerInfo) bool)

	// DeleteContainer deletes a ContainerInfo keyed by pod uid and container name
	DeleteContainer(podUID string, containerName string) error
	// RemovePod deletes a PodInfo keyed by pod uid. Repeatedly remove will be ignored.
	RemovePod(podUID string) error

	// SetPoolInfo stores a PoolInfo by pool name
	SetPoolInfo(poolName string, poolInfo *types.PoolInfo) error
	// DeletePool deletes a PoolInfo keyed by pool name
	DeletePool(poolName string) error
	// GCPoolEntries deletes GCPoolEntries not existing on node
	GCPoolEntries(livingPoolNameSet sets.String) error
}

// AdvisorMetaWriter provides a standard interface to modify advised metadata (generated by sysadvisor)
type AdvisorMetaWriter interface {
	UpdateRegionEntries(entries types.RegionEntries) error
}

type MetaCache interface {
	MetaReader
	RawMetaWriter
	AdvisorMetaWriter
}

// MetaCacheImp stores metadata and info of pod, node, pool, subnuma etc. as a cache,
// and synchronizes data to sysadvisor state file. It is thread-safe to read and write.
// Deep copy logic is performed during accessing metacache entries instead of directly
// return pointer of each struct to avoid mis-overwrite.
type MetaCacheImp struct {
	podEntries types.PodEntries
	podMutex   sync.RWMutex

	poolEntries types.PoolEntries
	poolMutex   sync.RWMutex

	regionEntries types.RegionEntries
	regionMutex   sync.RWMutex

	checkpointManager checkpointmanager.CheckpointManager
	checkpointName    string

	metricsFetcher metric.MetricsFetcher
}

var _ MetaCache = &MetaCacheImp{}

// NewMetaCacheImp returns the single instance of MetaCacheImp
func NewMetaCacheImp(conf *config.Configuration, metricsFetcher metric.MetricsFetcher) (*MetaCacheImp, error) {
	stateFileDir := conf.GenericSysAdvisorConfiguration.StateFileDirectory
	checkpointManager, err := checkpointmanager.NewCheckpointManager(stateFileDir)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize checkpoint manager: %v", err)
	}

	mc := &MetaCacheImp{
		podEntries:        make(types.PodEntries),
		poolEntries:       make(types.PoolEntries),
		regionEntries:     make(types.RegionEntries),
		checkpointManager: checkpointManager,
		checkpointName:    stateFileName,
		metricsFetcher:    metricsFetcher,
	}

	// Restore from checkpoint before any function call to metacache api
	if err := mc.restoreState(); err != nil {
		return mc, err
	}

	return mc, nil
}

/*
	standard implementation for metaReader
*/

func (mc *MetaCacheImp) GetContainerEntries(podUID string) (types.ContainerEntries, bool) {
	mc.podMutex.RLock()
	defer mc.podMutex.RUnlock()

	v, ok := mc.podEntries[podUID]
	return v.Clone(), ok
}

func (mc *MetaCacheImp) GetContainerInfo(podUID string, containerName string) (*types.ContainerInfo, bool) {
	mc.podMutex.RLock()
	defer mc.podMutex.RUnlock()

	podInfo, ok := mc.podEntries[podUID]
	if !ok {
		return nil, false
	}
	containerInfo, ok := podInfo[containerName]

	return containerInfo.Clone(), ok
}

// RangeContainer should deepcopy so that pod and container entries will not be overwritten.
func (mc *MetaCacheImp) RangeContainer(f func(podUID string, containerName string, containerInfo *types.ContainerInfo) bool) {
	mc.podMutex.RLock()
	defer mc.podMutex.RUnlock()

	for podUID, podInfo := range mc.podEntries.Clone() {
		for containerName, containerInfo := range podInfo {
			if !f(podUID, containerName, containerInfo) {
				break
			}
		}
	}
}

func (mc *MetaCacheImp) GetContainerMetric(podUID string, containerName string, metricName string) (float64, error) {
	return mc.metricsFetcher.GetContainerMetric(podUID, containerName, metricName)
}

func (mc *MetaCacheImp) GetPoolInfo(poolName string) (*types.PoolInfo, bool) {
	mc.poolMutex.RLock()
	defer mc.poolMutex.RUnlock()

	poolInfo, ok := mc.poolEntries[poolName]
	return poolInfo.Clone(), ok
}

func (mc *MetaCacheImp) GetPoolSize(poolName string) (int, bool) {
	mc.poolMutex.RLock()
	defer mc.poolMutex.RUnlock()

	pi, ok := mc.poolEntries[poolName]
	if !ok {
		return 0, false
	}
	return machine.CountCPUAssignmentCPUs(pi.TopologyAwareAssignments), true
}

func (mc *MetaCacheImp) GetRegionInfo(regionName string) (*types.RegionInfo, bool) {
	mc.regionMutex.RLock()
	defer mc.regionMutex.RUnlock()

	regionInfo, ok := mc.regionEntries[regionName]
	return regionInfo.Clone(), ok
}

func (mc *MetaCacheImp) RangeRegionInfo(f func(regionName string, regionInfo *types.RegionInfo) bool) {
	mc.regionMutex.RLock()
	defer mc.regionMutex.RUnlock()

	for regionName, regionInfo := range mc.regionEntries.Clone() {
		if !f(regionName, regionInfo) {
			break
		}

	}
}

/*
	standard implementation for RawMetaWriter
*/

func (mc *MetaCacheImp) AddContainer(podUID string, containerName string, containerInfo *types.ContainerInfo) error {
	mc.podMutex.Lock()
	defer mc.podMutex.Unlock()
	if podInfo, ok := mc.podEntries[podUID]; ok {
		if ci, ok := podInfo[containerName]; ok {
			ci.UpdateMeta(containerInfo)
			return nil
		}
	}

	return mc.setContainerInfo(podUID, containerName, containerInfo)
}

func (mc *MetaCacheImp) SetContainerInfo(podUID string, containerName string, containerInfo *types.ContainerInfo) error {
	mc.podMutex.Lock()
	defer mc.podMutex.Unlock()

	return mc.setContainerInfo(podUID, containerName, containerInfo)
}

func (mc *MetaCacheImp) setContainerInfo(podUID string, containerName string, containerInfo *types.ContainerInfo) error {
	podInfo, ok := mc.podEntries[podUID]
	if !ok {
		mc.podEntries[podUID] = make(types.ContainerEntries)
		podInfo = mc.podEntries[podUID]
	}
	if reflect.DeepEqual(podInfo[containerName], containerInfo) {
		return nil
	}
	podInfo[containerName] = containerInfo
	return mc.storeState()
}

func (mc *MetaCacheImp) deleteContainer(podUID string, containerName string) error {
	podInfo, ok := mc.podEntries[podUID]
	if !ok {
		return nil
	}
	_, ok = podInfo[containerName]
	if !ok {
		return nil
	}
	delete(podInfo, containerName)
	if len(podInfo) == 0 {
		delete(mc.podEntries, podUID)
	}

	return mc.storeState()
}

func (mc *MetaCacheImp) DeleteContainer(podUID string, containerName string) error {
	mc.podMutex.Lock()
	defer mc.podMutex.Unlock()

	return mc.deleteContainer(podUID, containerName)
}

func (mc *MetaCacheImp) RangeAndDeleteContainer(f func(containerInfo *types.ContainerInfo) bool) {
	mc.podMutex.Lock()
	defer mc.podMutex.Unlock()

	for _, podInfo := range mc.podEntries {
		for _, containerInfo := range podInfo {
			if f(containerInfo) {
				if err := mc.deleteContainer(containerInfo.PodUID, containerInfo.ContainerName); err != nil {
					klog.Errorf("deleteContainer [%v/%v] err %v", containerInfo.PodUID, containerInfo.ContainerName, err)
				}
			}
		}
	}
}

func (mc *MetaCacheImp) RangeAndUpdateContainer(f func(podUID string, containerName string, containerInfo *types.ContainerInfo) bool) {
	mc.podMutex.Lock()
	defer mc.podMutex.Unlock()

	oldPodEntries := mc.podEntries.Clone()

	for podUID, podInfo := range mc.podEntries {
		for containerName, containerInfo := range podInfo {
			if !f(podUID, containerName, containerInfo) {
				break
			}
		}
	}

	if !reflect.DeepEqual(oldPodEntries, mc.podEntries) {
		mc.storeState()
	}
}

func (mc *MetaCacheImp) RemovePod(podUID string) error {
	mc.podMutex.Lock()
	defer mc.podMutex.Unlock()

	_, ok := mc.podEntries[podUID]
	if !ok {
		return nil
	}
	delete(mc.podEntries, podUID)

	return mc.storeState()
}

/*
	standard implementation for AdvisorMetaWriter
*/

func (mc *MetaCacheImp) SetPoolInfo(poolName string, poolInfo *types.PoolInfo) error {
	mc.poolMutex.Lock()
	defer mc.poolMutex.Unlock()

	if reflect.DeepEqual(mc.poolEntries[poolName], poolInfo) {
		return nil
	}

	mc.poolEntries[poolName] = poolInfo

	return mc.storeState()
}

func (mc *MetaCacheImp) DeletePool(poolName string) error {
	mc.poolMutex.Lock()
	defer mc.poolMutex.Unlock()

	if _, ok := mc.poolEntries[poolName]; !ok {
		return nil
	}

	delete(mc.poolEntries, poolName)

	return mc.storeState()
}

func (mc *MetaCacheImp) GCPoolEntries(livingPoolNameSet sets.String) error {
	mc.poolMutex.Lock()
	defer mc.poolMutex.Unlock()

	needStoreState := false
	for poolName := range mc.poolEntries {
		if _, ok := livingPoolNameSet[poolName]; !ok {
			delete(mc.poolEntries, poolName)
			needStoreState = true
		}
	}

	if needStoreState {
		return mc.storeState()
	}

	return nil
}

func (mc *MetaCacheImp) UpdateRegionEntries(entries types.RegionEntries) error {
	mc.regionMutex.Lock()
	defer mc.regionMutex.Unlock()
	mc.regionEntries = entries.Clone()
	return nil
}

/*
	other helper functions
*/

func (mc *MetaCacheImp) storeState() error {
	checkpoint := NewMetaCacheCheckpoint()
	checkpoint.PodEntries = mc.podEntries
	checkpoint.PoolEntries = mc.poolEntries
	checkpoint.RegionEntries = mc.regionEntries

	begin := time.Now()
	defer func() {
		duration := time.Since(begin)
		if duration > storeStateWarningDuration {
			klog.ErrorS(fmt.Errorf("storeState took too long"), "storeState took longer than expected", "expected", storeStateWarningDuration, "actual", duration.Round(time.Millisecond))
		}
	}()

	if err := mc.checkpointManager.CreateCheckpoint(mc.checkpointName, checkpoint); err != nil {
		klog.Errorf("[metacache] store state failed: %v", err)
		return err
	}
	klog.Infof("[metacache] store state succeeded")

	return nil
}

func (mc *MetaCacheImp) restoreState() error {
	checkpoint := NewMetaCacheCheckpoint()

	if err := mc.checkpointManager.GetCheckpoint(mc.checkpointName, checkpoint); err != nil {
		if err == errors.ErrCheckpointNotFound {
			klog.Infof("[metacache] checkpoint %v not found, create", mc.checkpointName)
			return mc.storeState()
		} else if err == errors.ErrCorruptCheckpoint {
			klog.Infof("[metacache] checkpoint %v corrupted, create", mc.checkpointName)
			return mc.storeState()
		}
		klog.Errorf("[metacache] restore state failed: %v", err)
		return err
	}

	mc.podEntries = checkpoint.PodEntries
	mc.poolEntries = checkpoint.PoolEntries
	mc.regionEntries = checkpoint.RegionEntries

	klog.Infof("[metacache] restore state succeeded")

	return nil
}
