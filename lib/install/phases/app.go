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
	"context"
	"io"
	"strconv"

	"github.com/gravitational/gravity/lib/app"
	"github.com/gravitational/gravity/lib/constants"
	"github.com/gravitational/gravity/lib/fsm"
	"github.com/gravitational/gravity/lib/ops"
	"github.com/gravitational/gravity/lib/schema"
	"github.com/gravitational/gravity/lib/storage"
	"github.com/gravitational/gravity/lib/systeminfo"
	"github.com/gravitational/gravity/lib/utils"

	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"
)

// NewApp returns a new "app" phase executor
func NewApp(p fsm.ExecutorParams, operator ops.Operator, apps app.Applications) (*appExecutor, error) {
	if p.Phase.Data == nil || p.Phase.Data.ServiceUser == nil {
		return nil, trace.BadParameter("service user is required")
	}

	serviceUser, err := userFromOSUser(*p.Phase.Data.ServiceUser)
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
	return &appExecutor{
		FieldLogger:    logger,
		Operator:       operator,
		LocalApps:      apps,
		ExecutorParams: p,
		ServiceUser:    *serviceUser,
	}, nil
}

type appExecutor struct {
	// FieldLogger is used for logging
	logrus.FieldLogger
	// Operator is installer ops service
	Operator ops.Operator
	// LocalApps is the machine-local app service
	LocalApps app.Applications
	// ServiceUser is the user used for services and system storage
	ServiceUser systeminfo.User
	// ExecutorParams is common executor params
	fsm.ExecutorParams
}

// Execute runs install and post install hooks for an app
func (p *appExecutor) Execute(ctx context.Context) error {
	err := p.runHooks(ctx, schema.HookInstall, schema.HookInstalled)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// runHooks runs specified app hooks
func (p *appExecutor) runHooks(ctx context.Context, hooks ...schema.HookType) error {
	for _, hook := range hooks {
		locator := *p.Phase.Data.Package
		req := app.HookRunRequest{
			Application: locator,
			Hook:        hook,
			ServiceUser: storage.OSUser{
				Name: p.ServiceUser.Name,
				UID:  strconv.Itoa(p.ServiceUser.UID),
				GID:  strconv.Itoa(p.ServiceUser.GID),
			},
		}
		_, err := app.CheckHasAppHook(p.LocalApps, req)
		if err != nil {
			if trace.IsNotFound(err) {
				p.Debugf("Application %v does not have %v hook.",
					locator, hook)
				continue
			}
			return trace.Wrap(err)
		}
		p.Progress.NextStep("Executing %v hook for %v:%v", hook,
			locator.Name, locator.Version)
		p.Infof("Executing %v hook for %v:%v.", hook, locator.Name, locator.Version)
		reader, writer := io.Pipe()
		go func() {
			defer reader.Close()
			err := p.Operator.StreamOperationLogs(p.Key(), reader)
			if err != nil && !utils.IsStreamClosedError(err) {
				logrus.Warnf("Error streaming hook logs: %v.",
					trace.DebugReport(err))
			}
		}()
		_, err = app.StreamAppHook(ctx, p.LocalApps, req, writer)
		if err != nil {
			return trace.Wrap(err, "%v %s hook failed", locator, hook)
		}
		// closing the writer will result in the reader returning io.EOF
		// so the goroutine above will gracefully finish streaming
		err = writer.Close()
		if err != nil {
			logrus.Warnf("Failed to close pipe writer: %v.", err)
		}
	}
	return nil
}

// Rollback is no-op for this phase
func (*appExecutor) Rollback(ctx context.Context) error {
	return nil
}

// PreCheck makes sure this phase is executed on a master node
func (p *appExecutor) PreCheck(ctx context.Context) error {
	err := fsm.CheckMasterServer(p.Plan.Servers)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// PostCheck is no-op for this phase
func (*appExecutor) PostCheck(ctx context.Context) error {
	return nil
}
