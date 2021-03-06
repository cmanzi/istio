// Copyright 2019 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package builder

import (
	"fmt"

	tcp_filter "github.com/envoyproxy/go-control-plane/envoy/api/v2/listener"
	http_config "github.com/envoyproxy/go-control-plane/envoy/config/filter/http/rbac/v2"
	http_filter "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/http_connection_manager/v2"
	tcp_config "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/rbac/v2"
	envoy_rbac "github.com/envoyproxy/go-control-plane/envoy/config/rbac/v2"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/util"
	authz_model "istio.io/istio/pilot/pkg/security/authz/model"
	"istio.io/istio/pilot/pkg/security/authz/policy"
	"istio.io/istio/pilot/pkg/security/authz/policy/v1alpha1"
	"istio.io/istio/pilot/pkg/security/authz/policy/v1beta1"
	"istio.io/istio/pkg/config/labels"
	istiolog "istio.io/pkg/log"
)

var (
	rbacLog = istiolog.RegisterScope("rbac", "rbac debugging", 0)
)

// Builder wraps all needed information for building the RBAC filter for a service.
type Builder struct {
	isXDSMarshalingToAnyEnabled bool
	v1alpha1Generator           policy.Generator
	v1beta1Generator            policy.Generator
}

// NewBuilder creates a builder instance that can be used to build corresponding RBAC filter config.
func NewBuilder(serviceInstance *model.ServiceInstance, workloadLabels labels.Collection,
	policies *model.AuthorizationPolicies, isXDSMarshalingToAnyEnabled bool) *Builder {
	if serviceInstance.Service == nil {
		rbacLog.Errorf("no service for serviceInstance: %v", serviceInstance)
		return nil
	}

	serviceName := serviceInstance.Service.Attributes.Name
	serviceNamespace := serviceInstance.Service.Attributes.Namespace
	serviceHostname := string(serviceInstance.Service.Hostname)
	serviceMetadata, err := authz_model.NewServiceMetadata(serviceName, serviceNamespace, serviceInstance)
	if err != nil {
		rbacLog.Errorf("failed to create ServiceMetadata for %s: %s", serviceName, err)
		return nil
	}

	isGlobalPermissiveEnabled := policies.IsGlobalPermissiveEnabled()

	builder := &Builder{
		isXDSMarshalingToAnyEnabled: isXDSMarshalingToAnyEnabled,
	}

	if policies.IsRBACEnabled(serviceHostname, serviceNamespace) {
		builder.v1alpha1Generator = v1alpha1.NewGenerator(serviceMetadata, policies, isGlobalPermissiveEnabled)
	} else {
		rbacLog.Debugf("v1alpha1 RBAC policy disabled for service %s", serviceHostname)
	}

	// TODO: support policy in root namespace.
	matchedPolicies := policies.ListAuthorizationPolicies(serviceNamespace, workloadLabels)
	if len(matchedPolicies) > 0 {
		builder.v1beta1Generator = v1beta1.NewGenerator(matchedPolicies)
	} else {
		rbacLog.Debugf("v1beta1 authorization policies disabled for workload %v in %s",
			workloadLabels, serviceNamespace)
	}

	if builder.v1alpha1Generator == nil && builder.v1beta1Generator == nil {
		return nil
	}

	return builder
}

// BuildHTTPFilter builds the RBAC HTTP filter.
func (b *Builder) BuildHTTPFilter() *http_filter.HttpFilter {
	if b == nil {
		return nil
	}

	rbacConfig := b.generate(false /* forTCPFilter */)
	if rbacConfig == nil {
		return nil
	}
	httpConfig := http_filter.HttpFilter{
		Name: authz_model.RBACHTTPFilterName,
	}
	if b.isXDSMarshalingToAnyEnabled {
		httpConfig.ConfigType = &http_filter.HttpFilter_TypedConfig{TypedConfig: util.MessageToAny(rbacConfig)}
	} else {
		httpConfig.ConfigType = &http_filter.HttpFilter_Config{Config: util.MessageToStruct(rbacConfig)}
	}

	rbacLog.Debugf("built http filter config: %v", httpConfig)
	return &httpConfig
}

// BuildTCPFilter builds the RBAC TCP filter.
func (b *Builder) BuildTCPFilter() *tcp_filter.Filter {
	if b == nil {
		return nil
	}

	// The build function always return the config for HTTP filter, we need to extract the
	// generated rules and set it in the config for TCP filter.
	config := b.generate(true /* forTCPFilter */)
	if config == nil {
		return nil
	}
	rbacConfig := &tcp_config.RBAC{
		Rules:       config.Rules,
		ShadowRules: config.ShadowRules,
		StatPrefix:  authz_model.RBACTCPFilterStatPrefix,
	}

	tcpConfig := tcp_filter.Filter{
		Name: authz_model.RBACTCPFilterName,
	}
	if b.isXDSMarshalingToAnyEnabled {
		tcpConfig.ConfigType = &tcp_filter.Filter_TypedConfig{TypedConfig: util.MessageToAny(rbacConfig)}
	} else {
		tcpConfig.ConfigType = &tcp_filter.Filter_Config{Config: util.MessageToStruct(rbacConfig)}
	}

	rbacLog.Debugf("built tcp filter config: %v", tcpConfig)
	return &tcpConfig
}

func (b *Builder) generate(forTCPFilter bool) *http_config.RBAC {
	var v1alpha1Config *http_config.RBAC
	if b.v1alpha1Generator != nil {
		v1alpha1Config = b.v1alpha1Generator.Generate(forTCPFilter)
		rbacLog.Debugf("generated filter config from v1alpha1 policy: %v", v1alpha1Config)
	}

	var v1beta1Config *http_config.RBAC
	if b.v1beta1Generator != nil {
		v1beta1Config = b.v1beta1Generator.Generate(forTCPFilter)
		rbacLog.Debugf("generated filter config from v1beta1 policy: %v", v1beta1Config)
	}

	if v1alpha1Config == nil && v1beta1Config == nil {
		rbacLog.Errorf("No RBAC filter config generator available")
		return nil
	} else if v1alpha1Config == nil {
		return v1beta1Config
	} else if v1beta1Config == nil {
		return v1alpha1Config
	}

	if v1alpha1Config.Rules == nil {
		v1alpha1Config.Rules = &envoy_rbac.RBAC{}
	}
	if v1alpha1Config.Rules.Policies == nil {
		v1alpha1Config.Rules.Policies = map[string]*envoy_rbac.Policy{}
	}
	// Only need to merge rules, the shadow rules is not supported in v1beta1.
	for k, v := range v1beta1Config.GetRules().GetPolicies() {
		name := fmt.Sprintf("authz-v1beta1-merged[%s]", k)
		v1alpha1Config.Rules.Policies[name] = v
	}
	rbacLog.Debugf("merged v1beta1 to v1alpha1 config: %v", v1alpha1Config)
	return v1alpha1Config
}
