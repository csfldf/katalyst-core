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

package rama

import (
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/metacache"
	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/types"
)

type RamaPolicy struct {
	metaCache *metacache.MetaCache
}

func NewRamaPolicy(metaCache *metacache.MetaCache) *RamaPolicy {
	cp := &RamaPolicy{
		metaCache: metaCache,
	}
	return cp
}

func (p *RamaPolicy) SetContainerSet(containerSet map[string]sets.String) {
}

func (p *RamaPolicy) SetControlKnob(types.ControlKnob) {
}

func (p *RamaPolicy) SetIndicator(types.Indicator) {
}

func (p *RamaPolicy) SetTarget(types.Indicator) {
}

func (p *RamaPolicy) Update() {
}

func (p *RamaPolicy) GetProvisionResult() interface{} {
	return nil
}
