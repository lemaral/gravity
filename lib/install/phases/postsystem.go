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

package phases

import (
	"archive/tar"
	"context"
	"io"
	"io/ioutil"
	"path/filepath"

	"github.com/gravitational/gravity/lib/app"
	"github.com/gravitational/gravity/lib/app/resources"
	"github.com/gravitational/gravity/lib/archive"
	"github.com/gravitational/gravity/lib/constants"
	"github.com/gravitational/gravity/lib/defaults"
	"github.com/gravitational/gravity/lib/fsm"
	kubeutils "github.com/gravitational/gravity/lib/kubernetes"
	"github.com/gravitational/gravity/lib/ops"
	"github.com/gravitational/gravity/lib/state"
	"github.com/gravitational/gravity/lib/status"
	"github.com/gravitational/gravity/lib/storage"
	"github.com/gravitational/gravity/lib/utils"
	"github.com/gravitational/rigging"

	dockerarchive "github.com/docker/docker/pkg/archive"
	"github.com/gravitational/satellite/agent/proto/agentpb"
	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// NewWait returns a new "wait" phase executor
func NewWait(p fsm.ExecutorParams, operator ops.Operator) (*waitExecutor, error) {
	logger := &fsm.Logger{
		FieldLogger: logrus.WithFields(logrus.Fields{
			constants.FieldInstallPhase: p.Phase.ID,
		}),
		Key:      opKey(p.Plan),
		Operator: operator,
		Server:   p.Phase.Data.Server,
	}
	return &waitExecutor{
		FieldLogger:    logger,
		ExecutorParams: p,
	}, nil
}

type waitExecutor struct {
	// FieldLogger is used for logging
	logrus.FieldLogger
	// ExecutorParams is common executor params
	fsm.ExecutorParams
}

// Execute executes the wait phase
func (p *waitExecutor) Execute(ctx context.Context) error {
	p.Progress.NextStep("Waiting for the planet to start")
	p.Info("Waiting for the planet to start.")
	err := utils.Retry(defaults.RetryInterval, defaults.RetryAttempts,
		func() error {
			status, err := status.FromPlanetAgent(ctx, nil)
			if err != nil {
				return trace.Wrap(err)
			}
			// ideally we'd compare the nodes in the planet status to the plan
			// servers but simply checking that counts match will work for now
			if len(status.Nodes) != len(p.Plan.Servers) {
				return trace.BadParameter("not all planets have come up yet: %v",
					status)
			}
			if status.SystemStatus != agentpb.SystemStatus_Running {
				return trace.BadParameter("planet is not running yet: %v",
					status)
			}
			return nil
		})
	if err != nil {
		return trace.Wrap(err)
	}
	p.Info("Planet is running.")
	return nil
}

// Rollback is no-op for this phase
func (*waitExecutor) Rollback(ctx context.Context) error {
	return nil
}

// PreCheck makes sure the phase is executed on a master node
func (p *waitExecutor) PreCheck(ctx context.Context) error {
	err := fsm.CheckMasterServer(p.Plan.Servers)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// PostCheck is no-op for this phase
func (*waitExecutor) PostCheck(ctx context.Context) error {
	return nil
}

// NewNodes returns a new "nodes" phase executor
func NewNodes(p fsm.ExecutorParams, operator ops.Operator, apps app.Applications, client *kubernetes.Clientset) (*nodesExecutor, error) {
	application, err := apps.GetApp(*p.Phase.Data.Package)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	logger := &fsm.Logger{
		FieldLogger: logrus.WithFields(logrus.Fields{
			constants.FieldInstallPhase: p.Phase.ID,
		}),
		Key:      opKey(p.Plan),
		Operator: operator,
		Server:   p.Phase.Data.Server,
	}
	return &nodesExecutor{
		FieldLogger:    logger,
		Client:         client,
		Application:    *application,
		ExecutorParams: p,
	}, nil
}

type nodesExecutor struct {
	// FieldLogger is used for logging
	logrus.FieldLogger
	// Client is the Kubernetes client
	Client *kubernetes.Clientset
	// Application is the application being installed
	Application app.Application
	// ExecutorParams is common executor params
	fsm.ExecutorParams
}

// Execute executes the nodes phase
func (p *nodesExecutor) Execute(ctx context.Context) error {
	for _, server := range p.Plan.Servers {
		p.Progress.NextStep("Updating node %v with labels and taints",
			server.Hostname)
		// find this node's profile
		profile, err := p.Application.Manifest.NodeProfiles.ByName(server.Role)
		if err != nil {
			return trace.Wrap(err, "could not find node profile for %#v", server)
		}
		// update the node with labels and taints, try a few times to
		// account for possible transient errors
		err = utils.Retry(defaults.RetryInterval, defaults.RetryLessAttempts,
			func() error {
				return p.updateNode(p.Client, server, profile.Labels, profile.Taints)
			})
		if err != nil {
			return trace.Wrap(err)
		}
	}
	return nil
}

func (p *nodesExecutor) updateNode(client *kubernetes.Clientset, server storage.Server, labels map[string]string, taints []v1.Taint) error {
	// find corresponding Kubernetes node
	node, err := kubeutils.GetNode(client, server)
	if err != nil {
		return trace.Wrap(err)
	}
	for k, v := range labels {
		if k == defaults.KubernetesRoleLabel {
			node.Labels[k] = server.ClusterRole
		} else {
			node.Labels[k] = v
		}
	}
	node.Labels[defaults.KubernetesAdvertiseIPLabel] = server.AdvertiseIP
	node.Spec.Taints = taints
	p.Infof("Updating node %v with labels %v and taints %v.",
		node.Name, node.Labels, node.Spec.Taints)
	_, err = client.Core().Nodes().Update(node)
	if err != nil {
		return rigging.ConvertErrorWithContext(err,
			"failed to label and taint node %v", node.Name)
	}
	return nil
}

// Rollback is no-op for this phase
func (*nodesExecutor) Rollback(ctx context.Context) error {
	return nil
}

// PreCheck makes sure that all Kubernetes nodes have registered
func (p *nodesExecutor) PreCheck(ctx context.Context) error {
	err := fsm.CheckMasterServer(p.Plan.Servers)
	if err != nil {
		return trace.Wrap(err)
	}
	// make sure we have a Kubernetes node for each of our servers
	p.Info("Waiting for Kubernetes nodes to register.")
	for _, server := range p.Plan.Servers {
		err := p.waitForNode(server)
		if err != nil {
			return trace.Wrap(err)
		}
	}
	p.Info("All Kubernetes nodes have registered.")
	return nil
}

// waitForNode waits until a Kubernetes node for the provided server
// has become available
func (p *nodesExecutor) waitForNode(server storage.Server) (err error) {
	var node *v1.Node
	err = utils.Retry(defaults.RetryInterval, defaults.LabelRetryAttempts,
		func() error {
			node, err = kubeutils.GetNode(p.Client, server)
			if err != nil {
				return trace.Wrap(err, "Kubernetes node %v has not registered yet",
					server.AdvertiseIP)
			}
			return nil
		})
	if err != nil {
		return trace.Wrap(err, "failed to wait for Kubernetes node %v to register",
			server.AdvertiseIP)
	}
	p.Infof("Kubernetes node %v has registered: %v.",
		server.AdvertiseIP, node.Name)
	return nil
}

// PostCheck is no-op for this phase
func (*nodesExecutor) PostCheck(ctx context.Context) error {
	return nil
}

// NewRBAC returns a new "rbac" phase executor
func NewRBAC(p fsm.ExecutorParams, operator ops.Operator, apps app.Applications, client *kubernetes.Clientset) (*rbacExecutor, error) {
	logger := &fsm.Logger{
		FieldLogger: logrus.WithFields(logrus.Fields{
			constants.FieldInstallPhase: p.Phase.ID,
		}),
		Key:      opKey(p.Plan),
		Operator: operator,
		Server:   p.Phase.Data.Server,
	}
	return &rbacExecutor{
		FieldLogger:    logger,
		Apps:           apps,
		Client:         client,
		ExecutorParams: p,
	}, nil
}

type rbacExecutor struct {
	// FieldLogger is used for logging
	logrus.FieldLogger
	// Apps is the machine-local app service
	Apps app.Applications
	// Client is the Kubernetes client
	Client *kubernetes.Clientset
	// ExecutorParams is common executor params
	fsm.ExecutorParams
}

// Execute executes the rbac phase
func (p *rbacExecutor) Execute(ctx context.Context) error {
	p.Progress.NextStep("Creating Kubernetes RBAC resources")
	reader, err := p.Apps.GetAppResources(*p.Phase.Data.Package)
	if err != nil {
		return trace.Wrap(err)
	}
	defer reader.Close()
	stream, err := dockerarchive.DecompressStream(reader)
	if err != nil {
		return trace.Wrap(err)
	}
	defer stream.Close()
	err = archive.TarGlob(
		tar.NewReader(stream),
		defaults.ResourcesDir,
		[]string{defaults.ResourcesFile},
		func(_ string, reader io.Reader) error {
			return resources.ForEachObject(
				reader,
				fsm.GetUpsertBootstrapResourceFunc(p.Client))
		})
	if err != nil {
		return trace.Wrap(err)
	}
	p.Info("Created Kubernetes RBAC resources.")
	return nil
}

// Rollback is no-op for this phase
func (*rbacExecutor) Rollback(ctx context.Context) error {
	return nil
}

// PreCheck makes sure this phase is executed on a master node
func (p *rbacExecutor) PreCheck(ctx context.Context) error {
	err := fsm.CheckMasterServer(p.Plan.Servers)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// PostCheck is no-op for this phase
func (*rbacExecutor) PostCheck(ctx context.Context) error {
	return nil
}

// NewResources returns a new "resources" phase executor
func NewResources(p fsm.ExecutorParams, operator ops.Operator) (*resourcesExecutor, error) {
	logger := &fsm.Logger{
		FieldLogger: logrus.WithFields(logrus.Fields{
			constants.FieldInstallPhase: p.Phase.ID,
		}),
		Key:      opKey(p.Plan),
		Operator: operator,
		Server:   p.Phase.Data.Server,
	}
	return &resourcesExecutor{
		FieldLogger:    logger,
		ExecutorParams: p,
	}, nil
}

type resourcesExecutor struct {
	// FieldLogger is used for logging
	logrus.FieldLogger
	// ExecutorParams is common executor params
	fsm.ExecutorParams
}

// Execute executes the resources phase
func (p *resourcesExecutor) Execute(ctx context.Context) error {
	p.Progress.NextStep("Creating user-supplied Kubernetes resources")
	stateDir, err := state.GetStateDir()
	if err != nil {
		return trace.Wrap(err)
	}
	err = ioutil.WriteFile(filepath.Join(state.ShareDir(stateDir), "resources.yaml"),
		p.Phase.Data.Resources, defaults.SharedReadMask)
	if err != nil {
		return trace.Wrap(err, "failed to write user resources on disk")
	}
	out, err := utils.RunPlanetCommand(
		ctx,
		p.FieldLogger,
		defaults.KubectlBin,
		"--kubeconfig",
		constants.PrivilegedKubeconfig,
		"apply",
		"-f",
		filepath.Join(defaults.PlanetShareDir, "resources.yaml"),
	)
	if err != nil {
		return trace.Wrap(err, "failed to create user resources: %s", out)
	}
	p.Info("Created user-supplied Kubernetes resources.")
	return nil
}

// Rollback is no-op for this phase
func (*resourcesExecutor) Rollback(ctx context.Context) error {
	return nil
}

// PreCheck makes sure this phase is executed on a master node
func (p *resourcesExecutor) PreCheck(ctx context.Context) error {
	err := fsm.CheckMasterServer(p.Plan.Servers)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// PostCheck is no-op for this phase
func (*resourcesExecutor) PostCheck(ctx context.Context) error {
	return nil
}
