// Copyright ApeCloud, Inc.
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

cluster: {
	metadata: {
		namespace: string
		name:      string
	}
}
component: {
	clusterDefName: string
	characterType:  string
	type:           string
	name:           string
	workloadType:   string
	replicas:       int
	podSpec: containers: [...]
	volumeClaimTemplates: [...]
}

statefulset: {
	apiVersion: "apps/v1"
	kind:       "StatefulSet"
	metadata: {
		namespace: cluster.metadata.namespace
		name:      "\(cluster.metadata.name)-\(component.name)"
		labels: {
			"app.kubernetes.io/name":       "\(component.clusterDefName)"
			"app.kubernetes.io/instance":   cluster.metadata.name
			"app.kubernetes.io/managed-by": "kubeblocks"

			"apps.kubeblocks.io/component-name": "\(component.name)"
		}
	}
	spec: {
		selector:
			matchLabels: {
				"app.kubernetes.io/name":       "\(component.clusterDefName)"
				"app.kubernetes.io/instance":   "\(cluster.metadata.name)"
				"app.kubernetes.io/managed-by": "kubeblocks"

				"apps.kubeblocks.io/component-name": "\(component.name)"
			}
		serviceName: "\(cluster.metadata.name)-\(component.name)-headless"
		if component.workloadType != "Replication" {
			replicas: component.replicas
		}
		if component.workloadType == "Replication" {
			replicas: 1
		}
		minReadySeconds:     10
		podManagementPolicy: "Parallel"
		template: {
			metadata: {
				labels: {
					"app.kubernetes.io/name":       "\(component.clusterDefName)"
					"app.kubernetes.io/instance":   "\(cluster.metadata.name)"
					"app.kubernetes.io/managed-by": "kubeblocks"

					"apps.kubeblocks.io/component-name": "\(component.name)"
					"apps.kubeblocks.io/workload-type":  "\(component.workloadType)"
				}
			}
			spec: component.podSpec
		}
		if component.workloadType != "Replication" {
			volumeClaimTemplates: component.volumeClaimTemplates
		}
	}
}
