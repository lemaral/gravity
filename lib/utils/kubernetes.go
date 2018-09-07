/*
Copyright 2018 Gravitational, Inc.

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

package utils

import (
	"os"
	"strings"

	"github.com/gravitational/gravity/lib/constants"
	"github.com/gravitational/gravity/lib/defaults"

	"github.com/gravitational/rigging"
	"github.com/gravitational/trace"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// LoadKubeconfig tries to read a kubeconfig file and if it can't, returns an error.
// One exception, missing files result in empty configs, not an error.
func LoadKubeConfig() (*clientcmdapi.Config, error) {
	filename, err := EnsureLocalPath(
		os.Getenv(constants.EnvKubeConfig), defaults.KubeConfigDir, defaults.KubeConfigFile)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	config, err := clientcmd.LoadFromFile(filename)
	if err != nil && !os.IsNotExist(err) {
		return nil, trace.ConvertSystemError(err)
	}
	if config == nil {
		config = clientcmdapi.NewConfig()
	}
	return config, nil
}

// SaveKubeConfig saves updated config to location specified by environment variable or
// default location
func SaveKubeConfig(config clientcmdapi.Config) error {
	filename, err := EnsureLocalPath(
		os.Getenv(constants.EnvKubeConfig), defaults.KubeConfigDir, defaults.KubeConfigFile)
	if err != nil {
		return trace.Wrap(err)
	}
	err = clientcmd.WriteToFile(config, filename)
	if err != nil {
		return trace.ConvertSystemError(err)
	}
	return nil
}

// GetKubeClient returns instance of client to the kubernetes cluster
// using in-cluster configuration if available and falling back to
// configuration file under configPath otherwise
func GetKubeClient(configPath string) (client *kubernetes.Clientset, config *rest.Config, err error) {
	config, err = rest.InClusterConfig()
	if err != nil {
		config, err = clientcmd.BuildConfigFromFlags("", configPath)
		if err != nil {
			return nil, nil, trace.Wrap(err)
		}
	}

	client, err = kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	return client, config, nil
}

// GetKubeClientFromPath creates a kubernetes client from the specified configPath
func GetKubeClientFromPath(configPath string) (*kubernetes.Clientset, error) {
	config, err := clientcmd.BuildConfigFromFlags("", configPath)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return client, nil
}

// GetLocalKubeClient returns a client with config from KUBECONFIG env var or ~/.kube/config
func GetLocalKubeClient() (*kubernetes.Clientset, error) {
	configPath, err := EnsureLocalPath(
		os.Getenv(constants.EnvKubeConfig), defaults.KubeConfigDir, defaults.KubeConfigFile)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	client, err := GetKubeClientFromPath(configPath)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return client, nil
}

// GetMasters returns IPs of nodes which are marked with a "master" label
func GetMasters(nodes map[string]v1.Node) (ips []string) {
	ips = make([]string, 0, len(nodes))
	for _, node := range nodes {
		if role := node.Labels[defaults.KubernetesRoleLabel]; role != defaults.RoleMaster {
			continue
		}
		if ip, exists := node.Labels[defaults.KubernetesAdvertiseIPLabel]; exists {
			ips = append(ips, ip)
		} else {
			// Prior to 5.0.0-alpha.8 we were using the hostname label to store IP address
			// So we fallback to trying to read this from the hostname. Once we no longer need to support
			// upgrades from prior to 5.0.0 we can remove this code
			// TODO(knisbet) remove when no longer required
			if ip, exists := node.Labels[defaults.KubernetesHostnameLabel]; exists {
				ips = append(ips, ip)
			}
		}
	}
	return ips
}

// GetNodes returns the map of kubernetes nodes keyed by advertise IPs
func GetNodes(client corev1.NodeInterface) (nodes map[string]v1.Node, err error) {
	nodeList, err := client.List(metav1.ListOptions{})
	if err != nil {
		return nil, rigging.ConvertError(err)
	}

	nodes = make(map[string]v1.Node, len(nodeList.Items))
	for _, node := range nodeList.Items {
		ip, exists := node.Labels[defaults.KubernetesAdvertiseIPLabel]
		if exists {
			nodes[ip] = node
			continue
		}

		// Prior to 5.0.0-alpha.8 we were using the hostname label to store IP address
		// So we fallback to trying to read this from the hostname. Once we no longer need to support
		// upgrades from prior to 5.0.0 we can remove this code
		// TODO(knisbet) remove when no longer required
		ip, exists = node.Labels[defaults.KubernetesHostnameLabel]
		if exists {
			nodes[ip] = node
			continue
		}

		return nil, trace.NotFound("label %q not found for node %v",
			defaults.KubernetesAdvertiseIPLabel, node)
	}

	return nodes, nil
}

// MakeSelector converts set of key-value pairs to selector
func MakeSelector(in map[string]string) labels.Selector {
	set := make(labels.Set)
	for key, val := range in {
		set[key] = val
	}
	return set.AsSelector()
}

// FlattenVersion removes or replaces characters from the version string
// to make it useable as part of kubernetes resource names
func FlattenVersion(version string) string {
	return flattener.Replace(version)
}

var flattener = strings.NewReplacer(".", "", "+", "-")
