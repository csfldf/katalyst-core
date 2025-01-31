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

package headroompolicy

import (
	"io/ioutil"
	"os"
	"testing"

	info "github.com/google/cadvisor/info/v1"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/kubewharf/katalyst-api/pkg/apis/node/v1alpha1"
	"github.com/kubewharf/katalyst-api/pkg/consts"
	"github.com/kubewharf/katalyst-core/cmd/katalyst-agent/app/options"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/dynamicpolicy/state"
	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/metacache"
	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/types"
	"github.com/kubewharf/katalyst-core/pkg/config"
	"github.com/kubewharf/katalyst-core/pkg/config/agent/sysadvisor/qosaware/resource/cpu/headroom"
	pkgconsts "github.com/kubewharf/katalyst-core/pkg/consts"
	"github.com/kubewharf/katalyst-core/pkg/metaserver"
	"github.com/kubewharf/katalyst-core/pkg/metaserver/agent"
	metaservercnr "github.com/kubewharf/katalyst-core/pkg/metaserver/agent/cnr"
	"github.com/kubewharf/katalyst-core/pkg/metaserver/agent/metric"
	"github.com/kubewharf/katalyst-core/pkg/metaserver/agent/pod"
	"github.com/kubewharf/katalyst-core/pkg/metrics"
	"github.com/kubewharf/katalyst-core/pkg/util/machine"
	utilmetric "github.com/kubewharf/katalyst-core/pkg/util/metric"
)

func generateTestConfiguration(t *testing.T, checkpointDir, stateFileDir string) *config.Configuration {
	conf, err := options.NewOptions().Config()
	require.NoError(t, err)
	require.NotNil(t, conf)

	conf.GenericSysAdvisorConfiguration.StateFileDirectory = stateFileDir
	conf.MetaServerConfiguration.CheckpointManagerDir = checkpointDir

	return conf
}

func generateTestMetaServer(t *testing.T, cnr *v1alpha1.CustomNodeResource, podList []*v1.Pod,
	metricsFetcher metric.MetricsFetcher) *metaserver.MetaServer {
	// numa node0 cpu(s): 0-23,48-71
	// numa node1 cpu(s): 24-47,72-95
	cpuTopology, err := machine.GenerateDummyCPUTopology(96, 2, 2)
	require.NoError(t, err)

	metaServer := &metaserver.MetaServer{
		MetaAgent: &agent.MetaAgent{
			KatalystMachineInfo: &machine.KatalystMachineInfo{
				MachineInfo: &info.MachineInfo{
					NumCores: 96,
				},
				CPUTopology: cpuTopology,
			},
			CNRFetcher:     &metaservercnr.CNRFetcherStub{CNR: cnr},
			PodFetcher:     &pod.PodFetcherStub{PodList: podList},
			MetricsFetcher: metricsFetcher,
		},
	}
	return metaServer
}

func TestPolicyUtilization_GetHeadroom(t *testing.T) {
	type fields struct {
		entries                 types.RegionEntries
		cnr                     *v1alpha1.CustomNodeResource
		podList                 []*v1.Pod
		policyUtilizationConfig *headroom.PolicyUtilizationConfiguration
		essentials              types.ResourceEssentials
		setFakeMetric           func(store *utilmetric.MetricStore)
		setMetaCache            func(cache *metacache.MetaCacheImp)
	}
	tests := []struct {
		name    string
		fields  fields
		want    float64
		wantErr bool
	}{
		{
			name: "normal report",
			fields: fields{
				entries: map[string]*types.RegionInfo{
					"share-0": {
						RegionType: types.QoSRegionTypeShare,
					},
				},
				cnr: &v1alpha1.CustomNodeResource{
					Status: v1alpha1.CustomNodeResourceStatus{
						Resources: v1alpha1.Resources{
							Allocatable: &v1.ResourceList{
								consts.ReclaimedResourceMilliCPU: resource.MustParse("10000"),
							},
						},
					},
				},
				policyUtilizationConfig: &headroom.PolicyUtilizationConfiguration{
					ReclaimedCPUTargetCoreUtilization: 0.6,
					ReclaimedCPUMaxCoreUtilization:    0,
					ReclaimedCPUMaxOversoldRate:       1.5,
				},
				essentials: types.ResourceEssentials{
					EnableReclaim: true,
					Total:         96,
				},
				setFakeMetric: func(store *utilmetric.MetricStore) {
					for i := 0; i < 10; i++ {
						store.SetCPUMetric(i, pkgconsts.MetricCPUUsage, 30)
					}
				},
				setMetaCache: func(cache *metacache.MetaCacheImp) {
					err := cache.SetPoolInfo(state.PoolNameReclaim, &types.PoolInfo{
						PoolName: state.PoolNameReclaim,
						TopologyAwareAssignments: map[int]machine.CPUSet{
							0: machine.MustParse("0-9"),
						},
					})
					require.NoError(t, err)
				},
			},
			want: 13,
		},
		{
			name: "gap by oversold ratio",
			fields: fields{
				entries: map[string]*types.RegionInfo{
					"share-0": {
						RegionType: types.QoSRegionTypeShare,
					},
				},
				cnr: &v1alpha1.CustomNodeResource{
					Status: v1alpha1.CustomNodeResourceStatus{
						Resources: v1alpha1.Resources{
							Allocatable: &v1.ResourceList{
								consts.ReclaimedResourceMilliCPU: resource.MustParse("10000"),
							},
						},
					},
				},
				policyUtilizationConfig: &headroom.PolicyUtilizationConfiguration{
					ReclaimedCPUTargetCoreUtilization: 0.6,
					ReclaimedCPUMaxCoreUtilization:    0,
					ReclaimedCPUMaxOversoldRate:       1.2,
				},
				essentials: types.ResourceEssentials{
					EnableReclaim: true,
					Total:         96,
				},
				setFakeMetric: func(store *utilmetric.MetricStore) {
					for i := 0; i < 10; i++ {
						store.SetCPUMetric(i, pkgconsts.MetricCPUUsage, 0)
					}
				},
				setMetaCache: func(cache *metacache.MetaCacheImp) {
					err := cache.SetPoolInfo(state.PoolNameReclaim, &types.PoolInfo{
						PoolName: state.PoolNameReclaim,
						TopologyAwareAssignments: map[int]machine.CPUSet{
							0: machine.MustParse("0-9"),
						},
					})
					require.NoError(t, err)
				},
			},
			want: 12,
		},
		{
			name: "over maximum core utilization",
			fields: fields{
				entries: map[string]*types.RegionInfo{
					"share-0": {
						RegionType: types.QoSRegionTypeShare,
					},
				},
				cnr: &v1alpha1.CustomNodeResource{
					Status: v1alpha1.CustomNodeResourceStatus{
						Resources: v1alpha1.Resources{
							Allocatable: &v1.ResourceList{
								consts.ReclaimedResourceMilliCPU: resource.MustParse("15000"),
							},
						},
					},
				},
				policyUtilizationConfig: &headroom.PolicyUtilizationConfiguration{
					ReclaimedCPUTargetCoreUtilization: 0.6,
					ReclaimedCPUMaxCoreUtilization:    0.8,
					ReclaimedCPUMaxOversoldRate:       1.5,
				},
				essentials: types.ResourceEssentials{
					EnableReclaim: true,
					Total:         96,
				},
				setFakeMetric: func(store *utilmetric.MetricStore) {
					for i := 0; i < 96; i++ {
						store.SetCPUMetric(i, pkgconsts.MetricCPUUsage, 90)
					}
				},
				setMetaCache: func(cache *metacache.MetaCacheImp) {
					err := cache.SetPoolInfo(state.PoolNameReclaim, &types.PoolInfo{
						PoolName: state.PoolNameReclaim,
						TopologyAwareAssignments: map[int]machine.CPUSet{
							0: machine.MustParse("0-9"),
						},
					})
					require.NoError(t, err)
				},
			},
			want: 14,
		},
		{
			name: "limited by capacity",
			fields: fields{
				entries: map[string]*types.RegionInfo{
					"share-0": {
						RegionType: types.QoSRegionTypeShare,
					},
				},
				cnr: &v1alpha1.CustomNodeResource{
					Status: v1alpha1.CustomNodeResourceStatus{
						Resources: v1alpha1.Resources{
							Allocatable: &v1.ResourceList{
								consts.ReclaimedResourceMilliCPU: resource.MustParse("86000"),
							},
						},
					},
				},
				policyUtilizationConfig: &headroom.PolicyUtilizationConfiguration{
					ReclaimedCPUTargetCoreUtilization:   0.6,
					ReclaimedCPUMaxCoreUtilization:      0.8,
					ReclaimedCPUMaxOversoldRate:         1.5,
					ReclaimedCPUMaxHeadroomCapacityRate: 1.,
				},
				essentials: types.ResourceEssentials{
					EnableReclaim: true,
					Total:         96,
				},
				setFakeMetric: func(store *utilmetric.MetricStore) {
					for i := 0; i < 96; i++ {
						store.SetCPUMetric(i, pkgconsts.MetricCPUUsage, 30)
					}
				},
				setMetaCache: func(cache *metacache.MetaCacheImp) {
					err := cache.SetPoolInfo(state.PoolNameReclaim, &types.PoolInfo{
						PoolName: state.PoolNameReclaim,
						TopologyAwareAssignments: map[int]machine.CPUSet{
							0: machine.MustParse("0-85"),
						},
					})
					require.NoError(t, err)
				},
			},
			want: 96,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ckDir, err := ioutil.TempDir("", "checkpoint")
			require.NoError(t, err)
			defer os.RemoveAll(ckDir)

			sfDir, err := ioutil.TempDir("", "statefile")
			require.NoError(t, err)
			defer os.RemoveAll(sfDir)

			conf := generateTestConfiguration(t, ckDir, sfDir)
			conf.CPUHeadroomPolicyConfiguration.PolicyUtilization = tt.fields.policyUtilizationConfig
			metricsFetcher := metric.NewFakeMetricsFetcher(metrics.DummyMetrics{})
			metaCache, err := metacache.NewMetaCacheImp(conf, metricsFetcher)
			require.NoError(t, err)

			err = metaCache.UpdateRegionEntries(tt.fields.entries)
			require.NoError(t, err)
			tt.fields.setMetaCache(metaCache)

			metaServer := generateTestMetaServer(t, tt.fields.cnr, tt.fields.podList, metricsFetcher)
			p := NewPolicyUtilization("share-0", conf, nil, metaCache, metaServer, metrics.DummyMetrics{})

			store := utilmetric.GetMetricStoreInstance()
			tt.fields.setFakeMetric(store)

			p.SetEssentials(tt.fields.essentials)

			err = p.Update()
			require.NoError(t, err)
			got, err := p.GetHeadroom()
			if (err != nil) != tt.wantErr {
				t.Errorf("GetHeadroom() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("GetHeadroom() got = %v, want %v", got, tt.want)
			}
		})
	}
}
