// Copyright 2018 Istio Authors
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

package v2

import (
	"net"

	"github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	"github.com/gogo/protobuf/types"

	"istio.io/api/mesh/v1alpha1"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pkg/config"
)

// EndpointsFilterFunc is a function that filters data from the ClusterLoadAssignment and returns updated one
type EndpointsFilterFunc func(endpoints []endpoint.LocalityLbEndpoints, conn *XdsConnection, env *model.Environment) []*endpoint.LocalityLbEndpoints

// EndpointsByNetworkFilter is a network filter function to support Split Horizon EDS - filter the endpoints based on the network
// of the connected sidecar. The filter will filter out all endpoints which are not present within the
// sidecar network and add a gateway endpoint to remote networks that have endpoints (if gateway exists).
// Information for the mesh networks is provided as a MeshNetwork config map.
func EndpointsByNetworkFilter(endpoints []*endpoint.LocalityLbEndpoints, conn *XdsConnection, env *model.Environment) []*endpoint.LocalityLbEndpoints {
	// If the sidecar does not specify a network, ignore Split Horizon EDS and return all
	network, found := conn.modelNode.Metadata[model.NodeMetadataNetwork]
	if !found {
		// Couldn't find the sidecar network, using default/local
		network = ""
	}

	// calculate the multiples of weight.
	// It is needed to normalize the LB Weight across different networks.
	multiples := 1
	for _, network := range env.MeshNetworks.Networks {
		num := 0
		registryName := getNetworkRegistry(network)
		for _, gw := range network.Gateways {
			addrs := getGatewayAddresses(gw, registryName, env)
			num += len(addrs)
		}
		if num > 1 {
			multiples *= num
		}
	}

	// A new array of endpoints to be returned that will have both local and
	// remote gateways (if any)
	filtered := make([]*endpoint.LocalityLbEndpoints, 0)

	// Go through all cluster endpoints and add those with the same network as the sidecar
	// to the result. Also count the number of endpoints per each remote network while
	// iterating so that it can be used as the weight for the gateway endpoint
	for _, ep := range endpoints {
		// Weight (number of endpoints) for the EDS cluster for each remote networks
		remoteEps := map[string]uint32{}

		lbEndpoints := make([]*endpoint.LbEndpoint, 0)
		for _, lbEp := range ep.LbEndpoints {
			epNetwork := istioMetadata(lbEp, "network")
			if epNetwork == network {
				// This is a local endpoint
				lbEp.LoadBalancingWeight = &types.UInt32Value{
					Value: uint32(multiples),
				}
				lbEndpoints = append(lbEndpoints, lbEp)
			} else {
				// Remote endpoint. Increase the weight counter
				remoteEps[epNetwork]++
			}
		}

		// Add endpoints to remote networks' gateways

		// Iterate over all networks that have the cluster endpoint (weight>0) and
		// for each one of those add a new endpoint that points to the network's
		// gateway with the relevant weight
		for network, w := range remoteEps {
			networkConf, found := env.MeshNetworks.Networks[network]
			if !found {
				adsLog.Debugf("the endpoints within network %s will be ignored for no network configured", network)
				continue
			}
			gws := networkConf.Gateways
			if len(gws) == 0 {
				adsLog.Debugf("the endpoints within network %s will be ignored for no gateways configured", network)
				continue
			}

			registryName := getNetworkRegistry(networkConf)
			gwEps := make([]*endpoint.LbEndpoint, 0)
			// There may be multiples gateways for the network. Add an LbEndpoint for
			// each one of them
			for _, gw := range gws {
				gwAddresses := getGatewayAddresses(gw, registryName, env)
				// If gateway addresses are found, create an endpoint for each one of them
				if len(gwAddresses) > 0 {
					for _, gwAddr := range gwAddresses {
						epAddr := util.BuildAddress(gwAddr, gw.Port)
						gwEp := &endpoint.LbEndpoint{
							HostIdentifier: &endpoint.LbEndpoint_Endpoint{
								Endpoint: &endpoint.Endpoint{
									Address: epAddr,
								},
							},
							LoadBalancingWeight: &types.UInt32Value{
								Value: w,
							},
						}
						gwEps = append(gwEps, gwEp)
					}
				}
			}
			if len(gwEps) == 0 {
				continue
			}
			weight := w * uint32(multiples/len(gwEps))
			for _, gwEp := range gwEps {
				gwEp.LoadBalancingWeight.Value = weight
				lbEndpoints = append(lbEndpoints, gwEp)
			}
		}

		// Found local endpoint(s) so add to the result a new one LocalityLbEndpoints
		// that holds only the local endpoints
		newEp := createLocalityLbEndpoints(ep, lbEndpoints)
		filtered = append(filtered, newEp)
	}

	return filtered
}

// TODO: remove this, filtering should be done before generating the config, and
// network metadata should not be included in output. A node only receives endpoints
// in the same network as itself - so passing an network meta, with exactly
// same value that the node itself had, on each endpoint is a bit absurd.

// Checks whether there is an istio metadata string value for the provided key
// within the endpoint metadata. If exists, it will return the value.
func istioMetadata(ep *endpoint.LbEndpoint, key string) string {
	if ep.Metadata != nil &&
		ep.Metadata.FilterMetadata["istio"] != nil &&
		ep.Metadata.FilterMetadata["istio"].Fields != nil &&
		ep.Metadata.FilterMetadata["istio"].Fields[key] != nil {
		return ep.Metadata.FilterMetadata["istio"].Fields[key].GetStringValue()
	}
	return ""
}

func createLocalityLbEndpoints(base *endpoint.LocalityLbEndpoints, lbEndpoints []*endpoint.LbEndpoint) *endpoint.LocalityLbEndpoints {
	var weight *types.UInt32Value
	if len(lbEndpoints) == 0 {
		weight = nil
	} else {
		weight = &types.UInt32Value{}
		for _, lbEp := range lbEndpoints {
			weight.Value += lbEp.GetLoadBalancingWeight().Value
		}
	}
	ep := &endpoint.LocalityLbEndpoints{
		Locality:            base.Locality,
		LbEndpoints:         lbEndpoints,
		LoadBalancingWeight: weight,
		Priority:            base.Priority,
	}
	return ep
}

// LoadBalancingWeightNormalize set LoadBalancingWeight with a valid value.
func LoadBalancingWeightNormalize(endpoints []*endpoint.LocalityLbEndpoints) []*endpoint.LocalityLbEndpoints {
	return util.LocalityLbWeightNormalize(endpoints)
}

func getNetworkRegistry(network *v1alpha1.Network) string {
	var registryName string
	for _, eps := range network.Endpoints {
		if eps != nil && len(eps.GetFromRegistry()) > 0 {
			registryName = eps.GetFromRegistry()
			break
		}
	}

	return registryName
}

func getGatewayAddresses(gw *v1alpha1.Network_IstioNetworkGateway, registryName string, env *model.Environment) []string {
	// First, if a gateway address is provided in the configuration use it. If the gateway address
	// in the config was a hostname it got already resolved and replaced with an IP address
	// when loading the config
	if gwIP := net.ParseIP(gw.GetAddress()); gwIP != nil {
		return []string{gw.GetAddress()}
	}

	// Second, try to find the gateway addresses by the provided service name
	if gwSvcName := gw.GetRegistryServiceName(); len(gwSvcName) > 0 && len(registryName) > 0 {
		svc, _ := env.GetService(config.Hostname(gwSvcName))
		if svc != nil {
			return svc.Attributes.ClusterExternalAddresses[registryName]
		}
	}

	return nil
}
