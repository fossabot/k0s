//go:build unix

// Copyright 2022 k0s authors
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

package controller

import (
	"context"
	"fmt"
	"time"

	apcli "github.com/k0sproject/k0s/pkg/autopilot/client"
	apdel "github.com/k0sproject/k0s/pkg/autopilot/controller/delegate"
	aproot "github.com/k0sproject/k0s/pkg/autopilot/controller/root"
	apsig "github.com/k0sproject/k0s/pkg/autopilot/controller/signal"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	k8sretry "k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	cr "sigs.k8s.io/controller-runtime"
	crconfig "sigs.k8s.io/controller-runtime/pkg/config"
	crman "sigs.k8s.io/controller-runtime/pkg/manager"
	crmetricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	crwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/sirupsen/logrus"
)

type rootWorker struct {
	cfg           aproot.RootConfig
	log           *logrus.Entry
	clientFactory apcli.FactoryInterface
}

var _ aproot.Root = (*rootWorker)(nil)

// NewRootWorker builds a root for autopilot "worker" operations.
func NewRootWorker(cfg aproot.RootConfig, logger *logrus.Entry, cf apcli.FactoryInterface) (aproot.Root, error) {
	c := &rootWorker{
		cfg:           cfg,
		log:           logger,
		clientFactory: cf,
	}

	return c, nil
}

func (w *rootWorker) Run(ctx context.Context) error {
	logger := w.log

	managerOpts := crman.Options{
		Scheme: scheme,
		Controller: crconfig.Controller{
			// Controller-runtime maintains a global checklist of controller
			// names and does not currently provide a way to unregister the
			// controller names used by discarded managers. The autopilot
			// controller and worker components accidentally share some
			// controller names. So it's necessary to suppress the global name
			// check because the order in which components are started is not
			// fully guaranteed for k0s controller nodes running an embedded
			// worker.
			SkipNameValidation: ptr.To(true),
		},
		WebhookServer: crwebhook.NewServer(crwebhook.Options{
			Port: w.cfg.ManagerPort,
		}),
		Metrics: crmetricsserver.Options{
			BindAddress: w.cfg.MetricsBindAddr,
		},
		HealthProbeBindAddress: w.cfg.HealthProbeBindAddr,
	}

	// In some cases, we need to wait on the worker side until controller deploys all autopilot CRDs
	var attempt uint
	return k8sretry.OnError(wait.Backoff{
		Steps:    120,
		Duration: 1 * time.Second,
		Factor:   1.0,
		Jitter:   0.1,
	}, func(err error) bool {
		attempt++
		logger := logger.WithError(err).WithField("attempt", attempt)
		logger.Debug("Failed to run controller manager, retrying after backoff")
		return true
	}, func() error {
		cl, err := w.clientFactory.GetClient()
		if err != nil {
			return err
		}
		ns, err := cl.CoreV1().Namespaces().Get(ctx, "kube-system", v1.GetOptions{})
		if err != nil {
			return err
		}
		clusterID := string(ns.UID)

		mgr, err := cr.NewManager(w.clientFactory.RESTConfig(), managerOpts)
		if err != nil {
			return fmt.Errorf("failed to create controller manager: %w", err)
		}

		if err := RegisterIndexers(ctx, mgr, "worker"); err != nil {
			return fmt.Errorf("unable to register indexers: %w", err)
		}

		if err := apsig.RegisterControllers(ctx, logger, mgr, apdel.NodeControllerDelegate(), w.cfg.K0sDataDir, clusterID); err != nil {
			return fmt.Errorf("unable to register 'controlnodes' controllers: %w", err)
		}
		// The controller-runtime start blocks until the context is cancelled.
		if err := mgr.Start(ctx); err != nil {
			return fmt.Errorf("unable to run controller-runtime manager for workers: %w", err)
		}
		return nil
	})
}
