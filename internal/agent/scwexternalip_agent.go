/*
Copyright 2023.

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

package agent

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ptrkiov1alpha1 "ptrk.io/scaleway-external-ip/api/v1alpha1"
	"ptrk.io/scaleway-external-ip/internal/utils"
)

var (
	serviceField = ".spec.service"
	zoneLabel    = ".metadata.labels." + corev1.LabelTopologyZone
	finalizer    = "ptrk.io/finalizer"
)

type NodeNameID struct {
	Name string
	ID   string
}

type ScwExternalIPAgent struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=ptrk.io,resources=scwexternalips,verbs=get;list;watch
//+kubebuilder:rbac:groups=ptrk.io,resources=scwexternalips/status,verbs=get
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

func (r *ScwExternalIPAgent) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	m, err := utils.GetMetadata()
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("could not get instance metadata: %w", err)
	}

	var scweips ptrkiov1alpha1.ScwExternalIPList
	if err := r.List(ctx, &scweips); err != nil {
		return ctrl.Result{}, fmt.Errorf("could not list scwexternalips: %w", err)
	}

	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: os.Getenv("NODE_NAME")}, &node); err != nil {
		return ctrl.Result{}, fmt.Errorf("could not get current node: %w", err)
	}

	iface, err := netlink.LinkByName("ens2") // TODO: maybe not hardcode that
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("could not get ens2 iface: %w", err)
	}

	if len(m.PublicIPsV6) > 0 {
		// TODO have a better idea
		v6gw := net.ParseIP(m.PublicIPsV6[0].Gateway)
		// we currently need to add a default route for v6, if it doens't exists
		v6Routes, err := netlink.RouteList(iface, netlink.FAMILY_V6)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("could not list ipv6 routes: %w", err)
		}

		gotV6DefaultRoute := false
		for _, route := range v6Routes {
			if route.Dst == nil {
				// TODO: maybe check if it is the good one ?
				gotV6DefaultRoute = true
				break
			}
		}

		if !gotV6DefaultRoute {
			err = netlink.RouteAdd(&netlink.Route{
				Dst:       nil,
				LinkIndex: iface.Attrs().Index,
				Protocol:  netlink.FAMILY_V6,
				Gw:        v6gw,
			})
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("could not add default ipv6 route: %w", err)
			}
		}
	}

	// TODO: handle cleanup, maybe easier with a dedicated sub resource
	for _, eip := range scweips.Items {
		isMatch := true
		for k, v := range eip.Spec.NodeSelector {
			if node.Labels[k] != v {
				isMatch = false
				break
			}
		}
		if !isMatch {
			continue
		}
		for _, aip := range eip.Status.AttachedIPs {
			if node.Labels[corev1.LabelTopologyZone] != aip.Zone {
				continue
			}
			// ugly but what can you do, eh!
			mask := "/32"
			if strings.Contains(aip.IP, ":") {
				mask = "/128"
			}
			add, err := netlink.ParseAddr(aip.IP + mask)
			if err != nil {
				log.Error(err, "error parsing ip", "ip", aip.IP+mask)
				continue
			}
			err = netlink.AddrAdd(iface, add)
			if err != nil && !strings.Contains(err.Error(), "file exists") {
				log.Error(err, "error adding ip", "ip", add.String())
				continue
			}
		}
	}
	return ctrl.Result{}, nil
}

func (r *ScwExternalIPAgent) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ptrkiov1alpha1.ScwExternalIP{}).
		Complete(r)
}
