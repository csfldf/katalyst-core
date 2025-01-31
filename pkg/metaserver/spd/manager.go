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

package spd

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/atomic"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"

	configapis "github.com/kubewharf/katalyst-api/pkg/apis/config/v1alpha1"
	workloadapis "github.com/kubewharf/katalyst-api/pkg/apis/workload/v1alpha1"
	"github.com/kubewharf/katalyst-core/pkg/client"
	pkgconfig "github.com/kubewharf/katalyst-core/pkg/config"
	"github.com/kubewharf/katalyst-core/pkg/metaserver/agent/cnc"
	"github.com/kubewharf/katalyst-core/pkg/metrics"
	"github.com/kubewharf/katalyst-core/pkg/util"
	"github.com/kubewharf/katalyst-core/pkg/util/native"
)

const (
	defaultClearUnusedSPDPeriod = 10 * time.Minute
)

const (
	metricsNameGetCNCTargetConfigFailed = "spd_manager_get_cnc_target_failed"
	metricsNameUpdateCacheFailed        = "spd_manager_update_cache_failed"
	metricsNameCacheNotFound            = "spd_manager_cache_not_found"
)

type GetPodSPDNameFunc func(pod *v1.Pod) (string, error)

type ServiceProfileManager interface {
	// GetSPD get spd for given pod
	GetSPD(ctx context.Context, pod *v1.Pod) (*workloadapis.ServiceProfileDescriptor, error)

	// Run async loop to clear unused spd
	Run(ctx context.Context)
}

type spdManager struct {
	started *atomic.Bool
	mux     sync.Mutex

	client            *client.GenericClientSet
	emitter           metrics.MetricEmitter
	cncFetcher        cnc.CNCFetcher
	checkpointManager checkpointmanager.CheckpointManager
	getPodSPDNameFunc GetPodSPDNameFunc

	ServiceProfileCacheTTL time.Duration

	// spdCache is a cache of namespace/name to current target spd
	spdCache *Cache
}

// NewSPDManager creates a spd manager to implement ServiceProfileManager
func NewSPDManager(clientSet *client.GenericClientSet, emitter metrics.MetricEmitter,
	cncFetcher cnc.CNCFetcher, conf *pkgconfig.Configuration) (ServiceProfileManager, error) {
	checkpointManager, err := checkpointmanager.NewCheckpointManager(conf.CheckpointManagerDir)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize checkpoint manager: %v", err)
	}

	m := &spdManager{
		started:                atomic.NewBool(false),
		client:                 clientSet,
		emitter:                emitter,
		checkpointManager:      checkpointManager,
		cncFetcher:             cncFetcher,
		ServiceProfileCacheTTL: conf.ServiceProfileCacheTTL,
	}

	m.getPodSPDNameFunc = util.GetPodSPDName
	m.spdCache = NewSPDCache(checkpointManager, defaultClearUnusedSPDPeriod)

	return m, nil
}

func (s *spdManager) GetSPD(ctx context.Context, pod *v1.Pod) (*workloadapis.ServiceProfileDescriptor, error) {
	spdName, err := s.getPodSPDNameFunc(pod)
	if err != nil {
		return nil, fmt.Errorf("get pod spd name failed: %v", err)
	}

	return s.getSPDByNamespaceName(ctx, pod.GetNamespace(), spdName)
}

// SetGetPodSPDNameFunc set get spd name function to override default getPodSPDNameFunc before started
func (s *spdManager) SetGetPodSPDNameFunc(f GetPodSPDNameFunc) {
	if s.started.Load() {
		klog.Warningf("spd manager has already started, not allowed to set implementations")
		return
	}

	s.getPodSPDNameFunc = f
}

func (s *spdManager) Run(ctx context.Context) {
	if s.started.Swap(true) {
		return
	}

	s.spdCache.Run(ctx)
	<-ctx.Done()
}

func (s *spdManager) getSPDByNamespaceName(ctx context.Context, namespace, name string) (*workloadapis.ServiceProfileDescriptor, error) {
	key := native.GenerateNamespaceNameKey(namespace, name)
	baseTag := []metrics.MetricTag{
		{Key: "spdNamespace", Val: namespace},
		{Key: "spdName", Val: name},
	}

	// first get spd origin spd from local cache
	originSPD := s.spdCache.GetSPD(key)

	// get spd current target config from cnc to limit rate of get remote spd by comparing local spd
	// hash with cnc target config hash, if cnc target config not found it will get remote spd directly
	targetConfig, err := s.getSPDTargetConfig(ctx, namespace, name)
	if err != nil {
		klog.Errorf("[spd-manager] get spd targetConfig config failed: %v, use local cache instead", err)
		targetConfig = &configapis.TargetConfig{
			ConfigNamespace: namespace,
			ConfigName:      name,
		}
		_ = s.emitter.StoreInt64(metricsNameGetCNCTargetConfigFailed, 1, metrics.MetricTypeNameCount, baseTag...)
	}

	// try to update spd cache from remote if cache spd hash is not equal to target config hash,
	// the rate of getting remote spd will be limited by spd ServiceProfileCacheTTL
	err = s.updateSPDCacheIfNeed(ctx, originSPD, targetConfig)
	if err != nil {
		klog.Errorf("[spd-manager] failed update spd cache from remote: %v, use local cache instead", err)
		_ = s.emitter.StoreInt64(metricsNameUpdateCacheFailed, 1, metrics.MetricTypeNameCount, baseTag...)
	}

	// get current spd after cache updated
	currentSPD := s.spdCache.GetSPD(key)
	if currentSPD != nil {
		return currentSPD, nil
	}

	_ = s.emitter.StoreInt64(metricsNameCacheNotFound, 1, metrics.MetricTypeNameCount, baseTag...)

	return nil, fmt.Errorf("get spd cache for %s not found", key)
}

// getSPDTargetConfig get spd target config from cnc
func (s *spdManager) getSPDTargetConfig(ctx context.Context, namespace, name string) (*configapis.TargetConfig, error) {
	currentCNC, err := s.cncFetcher.GetCNC(ctx)
	if err != nil {
		return &configapis.TargetConfig{}, err
	}

	for _, target := range currentCNC.Status.ServiceProfileConfigList {
		if target.ConfigNamespace == namespace && target.ConfigName == name {
			return &target, nil
		}
	}

	return nil, fmt.Errorf("get target spd %s/%s not found", namespace, name)
}

// updateSPDCacheIfNeed checks if the previous spd has changed, and
// re-get from APIServer if the previous is out-of date.
func (s *spdManager) updateSPDCacheIfNeed(ctx context.Context, originSPD *workloadapis.ServiceProfileDescriptor,
	targetConfig *configapis.TargetConfig) error {
	s.mux.Lock()
	defer s.mux.Unlock()

	if originSPD == nil && targetConfig == nil {
		return nil
	}

	now := time.Now()
	if originSPD == nil || util.GetSPDHash(originSPD) != targetConfig.Hash {
		key := native.GenerateNamespaceNameKey(targetConfig.ConfigNamespace, targetConfig.ConfigName)
		if lastFetchRemoteTime := s.spdCache.GetLastFetchRemoteTime(key); lastFetchRemoteTime.Add(s.ServiceProfileCacheTTL).After(time.Now()) {
			return nil
		} else {
			// first update the timestamp of the last attempt to fetch the remote spd to
			// avoid frequent requests to the api-server in some bad situations
			s.spdCache.SetLastFetchRemoteTime(key, now)
		}

		klog.Infof("[spd-manager] spd %s targetConfig hash is changed from %s to %s", key, util.GetSPDHash(originSPD), targetConfig.Hash)
		spd, err := s.client.InternalClient.WorkloadV1alpha1().ServiceProfileDescriptors(targetConfig.ConfigNamespace).
			Get(ctx, targetConfig.ConfigName, metav1.GetOptions{ResourceVersion: "0"})
		if err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("get spd %s from remote failed: %v", key, err)
		} else if err != nil {
			err = s.spdCache.DeleteSPD(key)
			if err != nil {
				return fmt.Errorf("delete spd %s from cache failed: %v", key, err)
			}

			klog.Infof("[spd-manager] spd %s cache has been deleted", key)
			return nil
		}

		err = s.spdCache.SetSPD(key, spd)
		if err != nil {
			return err
		}
		klog.Infof("[spd-manager] spd %s cache has been updated to %v", key, spd)
	}

	return nil
}
