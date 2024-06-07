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
	"errors"
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ptrkiov1alpha1 "ptrk.io/scaleway-external-ip/api/v1alpha1"
	"ptrk.io/scaleway-external-ip/internal/utils"
)

var (
	serviceField     = ".spec.service"
	zoneLabel        = ".metadata.labels." + corev1.LabelTopologyZone
	agentFinalizer   = "ptrk.io/agentFinalizer"
	noInterfaceError = errors.New("interface not found")
)

type linkAction struct {
	Link netlink.Link
	IP   *netlink.Addr
}

type ScwExternalIPAgent struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=ptrk.io,resources=scwexternalips,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=ptrk.io,resources=scwexternalips/status,verbs=get
// +kubebuilder:rbac:groups=ptrk.io,resources=scwexternalips/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
func (r *ScwExternalIPAgent) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var scweips ptrkiov1alpha1.ScwExternalIPList
	if err := r.List(ctx, &scweips); err != nil {
		return ctrl.Result{}, fmt.Errorf("could not list scwexternalips: %w", err)
	}

	for _, scweip := range scweips.Items {
		// It's deleted!
		if !scweip.ObjectMeta.DeletionTimestamp.IsZero() {
			if controllerutil.ContainsFinalizer(&scweip, agentFinalizer) {
				log.Info("externalIP deleted, perform umount...")
				err := r.cleanup(ctx, scweip.Status.AttachedIPs)
				if err != nil {
					return ctrl.Result{}, err
				}

				// Clean CRD object
				scweip.Status.DeletingIPs = []ptrkiov1alpha1.ScwNodeExternalIP{}

				controllerutil.RemoveFinalizer(&scweip, agentFinalizer)
				if err := r.Update(ctx, &scweip); err != nil {
					return ctrl.Result{}, err
				}
			}
			return ctrl.Result{}, nil
		}
	}

	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: os.Getenv("NODE_NAME")}, &node); err != nil {
		return ctrl.Result{}, fmt.Errorf("could not get current node: %w", err)
	}

	for _, eip := range scweips.Items {
		// Add Required IPs
		addActions, err := r.getLinkAction(ctx, node, eip.Status.AttachedIPs)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("could not get ip actions: %w", err)
		}
		for _, op := range addActions {
			err = netlink.AddrAdd(op.Link, op.IP)
			if err != nil && !strings.Contains(err.Error(), "file exists") {
				log.Error(err, "error adding ip", "ip", op.IP.String())
				continue
			}
		}

		// Umount required IPs
		delActions, err := r.getLinkAction(ctx, node, eip.Status.DeletingIPs)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("could not get ip actions: %w", err)
		}
		for _, op := range delActions {
			err = netlink.AddrDel(op.Link, op.IP)
			if err != nil && !strings.Contains(err.Error(), "file exists") {
				log.Error(err, "error deleting ip", "ip", op.IP.String())
				continue
			}
		}
	}
	return ctrl.Result{}, nil
}

func (r *ScwExternalIPAgent) getLinkAction(ctx context.Context, node corev1.Node, externalIPs []ptrkiov1alpha1.ScwNodeExternalIP) ([]linkAction, error) {
	log := log.FromContext(ctx)
	res := []linkAction{}

	m, err := utils.GetMetadata()
	if err != nil {
		return res, fmt.Errorf("could not get instance metadata: %w", err)
	}

	for _, ip := range externalIPs {
		if node.Labels[corev1.LabelTopologyZone] != ip.Zone {
			continue
		}

		iface, err := getLinkByMacAddr(ip.NodeMacAddr)
		if err != nil {
			if err != noInterfaceError {
				return res, fmt.Errorf("could not get iface: %w", err)
			}
			continue
		}

		if len(m.PublicIPsV6) > 0 {
			// TODO have a better idea
			v6gw := net.ParseIP(m.PublicIPsV6[0].Gateway)
			// we currently need to add a default route for v6, if it doens't exists
			v6Routes, err := netlink.RouteList(iface, netlink.FAMILY_V6)
			if err != nil {
				return res, fmt.Errorf("could not list ipv6 routes: %w", err)
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
					return res, fmt.Errorf("could not add default ipv6 route: %w", err)
				}
			}
		}

		add, err := utils.GetNetlinkAddr(ip.IP)
		if err != nil {
			log.Error(err, "error getting netlink address", "ip", ip.IP)
			continue
		} else {
			log.Info("append to linkAction", "iface", iface.Attrs().Name, "ip", add.IP.String())
			res = append(res, linkAction{Link: iface, IP: add})
		}
	}
	return res, nil
}

func (r *ScwExternalIPAgent) cleanup(ctx context.Context, attached []ptrkiov1alpha1.ScwNodeExternalIP) error {
	log := log.FromContext(ctx)
	errs := []string{}
	for _, ip := range attached {
		log.Info("ip to be cleaned", "ip", ip.IP, "mac", ip.NodeMacAddr)
		iface, err := getLinkByMacAddr(ip.NodeMacAddr)
		if err != nil && err != noInterfaceError {
			return err
		}
		if iface == nil {
			return nil
		}

		add, err := utils.GetNetlinkAddr(ip.IP)
		if err != nil {
			log.Error(err, "error getting netlink address", "ip", ip.IP)
			continue
		}
		err = netlink.AddrDel(iface, add)
		if err != nil && !strings.Contains(err.Error(), "cannot assign requested address") {
			log.Error(err, "error deleting ip", "ip", add.String())
			continue
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(errs, " - "))
}

func (r *ScwExternalIPAgent) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ptrkiov1alpha1.ScwExternalIP{}).
		Complete(r)
}

func getLinkByMacAddr(macAddr string) (netlink.Link, error) {
	hwAddr, err := net.ParseMAC(macAddr)
	if err != nil {
		return nil, fmt.Errorf("could not parse mac addr: %w", err)
	}

	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("could not list mac addrs: %w", err)
	}

	for _, link := range links {
		if link.Attrs().HardwareAddr.String() == hwAddr.String() {
			return link, nil
		}
	}
	return nil, noInterfaceError
}
