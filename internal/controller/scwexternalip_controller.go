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

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ptrkiov1alpha1 "ptrk.io/scaleway-external-ip/api/v1alpha1"
	"ptrk.io/scaleway-external-ip/internal/utils"
)

var (
	serviceField        = ".spec.service"
	zoneLabel           = ".metadata.labels." + corev1.LabelTopologyZone
	controllerFinalizer = "ptrk.io/controllerFinalizer"
	agentFinalizer      = "ptrk.io/agentFinalizer"
)

const (
	cacheKey string = "scwIpCache"
)

type NodeNameID struct {
	Name  string
	ID    string
	specs instance.Server
}

type Cache interface {
	Set(cacheKey string, value interface{}, maxAge time.Duration)
	Get(cacheKey string) (value interface{}, found bool)
}

// ScwExternalIPReconciler reconciles a ScwExternalIP object
type ScwExternalIPReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	ScwClient *scw.Client
	Cache     Cache
}

//+kubebuilder:rbac:groups=ptrk.io,resources=scwexternalips,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=ptrk.io,resources=scwexternalips/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=ptrk.io,resources=scwexternalips/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

func (r *ScwExternalIPReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	api := instance.NewAPI(r.ScwClient)

	var scweip ptrkiov1alpha1.ScwExternalIP
	if err := r.Get(ctx, req.NamespacedName, &scweip); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("Start", "name", scweip.GetName())

	// It's deleted!
	if !scweip.ObjectMeta.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&scweip, controllerFinalizer) {
			log.Info("externalIP deleted, dettach from compute...")
			err := r.cleanup(ctx, scweip.Status.AttachedIPs)
			if err != nil {
				return ctrl.Result{}, err
			}

			// Cleanup CRD object
			scweip.Status.DeletingIPs = scweip.Status.AttachedIPs
			scweip.Status.AttachedIPs = []ptrkiov1alpha1.ScwNodeExternalIP{}
			scweip.Status.IPs = []string{}
			scweip.Status.PendingIPsCount = 0
			scweip.Status.PendingIPs = []ptrkiov1alpha1.ScwExternalIPStatusPendingIP{}

			// Cleanup finalizer
			controllerutil.RemoveFinalizer(&scweip, controllerFinalizer)
			if err := r.Update(ctx, &scweip); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	} else {
		if !controllerutil.ContainsFinalizer(&scweip, controllerFinalizer) && !controllerutil.ContainsFinalizer(&scweip, agentFinalizer) {
			controllerutil.AddFinalizer(&scweip, controllerFinalizer)
			controllerutil.AddFinalizer(&scweip, agentFinalizer)
			if err := r.Update(ctx, &scweip); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	var eIPs []string

	if scweip.Spec.Service != "" {
		svcName := scweip.Spec.Service
		foundSvc := &corev1.Service{}
		err := r.Get(ctx, types.NamespacedName{Name: svcName, Namespace: scweip.Namespace}, foundSvc)
		if err != nil {
			// TODO (handle in webhook?)
			return ctrl.Result{}, err
		}
		eIPs = foundSvc.Spec.ExternalIPs
	}

	if len(eIPs) == 0 && len(scweip.Status.AttachedIPs) == 0 {
		// nothing to do
		scweip.Status.AttachedIPs = []ptrkiov1alpha1.ScwNodeExternalIP{}
		scweip.Status.PendingIPs = []ptrkiov1alpha1.ScwExternalIPStatusPendingIP{}
		scweip.Status.IPs = []string{}
		scweip.Status.PendingIPsCount = 0
		if err := r.Status().Update(ctx, &scweip); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	ipMap, err := r.getInstanceIPMap()
	if err != nil {
		return ctrl.Result{}, err
	}

	oldAttached := make(map[string]ptrkiov1alpha1.ScwNodeExternalIP)
	for _, oip := range scweip.Status.AttachedIPs {
		oldAttached[oip.IP] = oip
	}

	scweip.Status.AttachedIPs = []ptrkiov1alpha1.ScwNodeExternalIP{}
	scweip.Status.PendingIPs = []ptrkiov1alpha1.ScwExternalIPStatusPendingIP{}
	scweip.Status.IPs = []string{}
	scweip.Status.PendingIPsCount = 0

	for _, eip := range eIPs {
		delete(oldAttached, eip)

		sip, err := utils.GetV4OrV664Prefix(eip)
		if err != nil {
			log.Error(err, "unable to get v4v6 string from ip", "ip", eip)
			continue
		}

		if ip := ipMap[sip]; ip != nil {
			reason := ""
			possibleNodes, err := r.findNodes(ctx, ip.Zone.String(), scweip.Spec.NodeSelector)
			if err != nil {
				log.Error(err, "unable to find possible nodes", "ip", ip.Address.String(), "zone", ip.Zone.String())
				reason = fmt.Sprintf("error finding possible nodes: %s", err)
			} else if len(possibleNodes) == 0 {
				reason = "No nodes are matching conditions"
			}

			if err != nil || len(possibleNodes) == 0 {
				scweip.Status.PendingIPsCount++
				scweip.Status.PendingIPs = append(scweip.Status.PendingIPs, ptrkiov1alpha1.ScwExternalIPStatusPendingIP{
					IP:     eip,
					Zone:   ip.Zone.String(),
					Reason: reason,
				})
				continue
			}

			// Sort the possibleNodes to make sure to spread IPs to all the nodes
			sort.Slice(possibleNodes, func(i, j int) bool {
				return len(possibleNodes[i].specs.PublicIPs) < len(possibleNodes[j].specs.PublicIPs)
			})

			var nodeName, nodeID, nodeMacAddr string

			if ip.Server != nil {
				// IP already attached to a server
				for _, nni := range possibleNodes {
					if nni.Name == ip.Server.Name {
						nodeName = nni.Name
						nodeID = nni.ID
						nodeMacAddr = nni.specs.MacAddress
						break
					}
				}
				if nodeName == "" {
					// IP not attached to a possible node
					node := &corev1.Node{}
					err := r.Get(ctx, types.NamespacedName{Name: ip.Server.Name}, node)
					if client.IgnoreNotFound(err) != nil {
						log.Error(err, "error getting node", "node", ip.Server.Name)
						scweip.Status.PendingIPsCount++
						scweip.Status.PendingIPs = append(scweip.Status.PendingIPs, ptrkiov1alpha1.ScwExternalIPStatusPendingIP{
							IP:     eip,
							Zone:   ip.Zone.String(),
							Reason: fmt.Sprintf("IP already attached to %s", ip.Server.Name),
						})
						continue
					}

					if apierrors.IsNotFound(err) {
						// node is not in Kubernetes, let's not detach for now.
						// TODO: maybe just force attach ?
						scweip.Status.PendingIPsCount++
						scweip.Status.PendingIPs = append(scweip.Status.PendingIPs, ptrkiov1alpha1.ScwExternalIPStatusPendingIP{
							IP:     eip,
							Zone:   ip.Zone.String(),
							Reason: fmt.Sprintf("IP already attached to %s", ip.Server.Name),
						})
						continue
					}

					for _, c := range node.Status.Conditions {
						if c.Type == corev1.NodeReady {
							if c.Status != corev1.ConditionTrue {
								// node attached to the IP is not ready, let's atttach the IP to another node
								for i, s := range possibleNodes {
									_, err := api.UpdateIP(&instance.UpdateIPRequest{
										Zone: ip.Zone,
										IP:   ip.ID,
										Server: &instance.NullableStringValue{
											Value: s.ID,
										},
									})
									if err != nil {
										log.Error(err, "error attaching ip", "ip", ip.ID, "server", s.ID)
										if i == len(possibleNodes)-1 {
											scweip.Status.PendingIPsCount++
											scweip.Status.PendingIPs = append(scweip.Status.PendingIPs, ptrkiov1alpha1.ScwExternalIPStatusPendingIP{
												IP:     eip,
												Zone:   ip.Zone.String(),
												Reason: "No server could attached to IP",
											})
											break
										}
										continue
									}
									scweip.Status.IPs = append(scweip.Status.IPs, eip)
									scweip.Status.AttachedIPs = append(scweip.Status.AttachedIPs, ptrkiov1alpha1.ScwNodeExternalIP{
										IP:          eip,
										IPID:        ip.ID,
										Zone:        ip.Zone.String(),
										Node:        s.ID,
										NodeID:      s.Name,
										NodeMacAddr: s.specs.MacAddress,
									})
									log.Info("Attached IP", "ip", eip, "nodeID", s.ID, "node", s.Name, "zone", ip.Zone.String(), "mac", s.specs.MacAddress)
									break
								}
							} else {
								// node is ready, have the IP, but is not in possibleNodes, weird
								scweip.Status.PendingIPsCount++
								scweip.Status.PendingIPs = append(scweip.Status.PendingIPs, ptrkiov1alpha1.ScwExternalIPStatusPendingIP{
									IP:     eip,
									Zone:   ip.Zone.String(),
									Reason: fmt.Sprintf("IP is already attached to a cluster node (%s), not matching conditions", node.Name),
								})
							}
							break
						}
					}
				} else {
					scweip.Status.IPs = append(scweip.Status.IPs, eip)
					scweip.Status.AttachedIPs = append(scweip.Status.AttachedIPs, ptrkiov1alpha1.ScwNodeExternalIP{
						IP:          eip,
						IPID:        ip.ID,
						Zone:        ip.Zone.String(),
						Node:        nodeName,
						NodeID:      nodeID,
						NodeMacAddr: nodeMacAddr,
					})
				}
			} else {
				// IP not attached to a server
				for i, s := range possibleNodes {
					_, err = api.UpdateIP(&instance.UpdateIPRequest{
						// TODO: add reverse ?
						IP:     ip.ID,
						Zone:   ip.Zone,
						Server: &instance.NullableStringValue{Value: s.ID},
					})
					if err != nil {
						log.Error(err, "unable to update ip", "ip", ip.Address.String(), "server", s.ID, "zone", ip.Zone.String())
						if i == len(possibleNodes)-1 {
							scweip.Status.PendingIPsCount++
							scweip.Status.PendingIPs = append(scweip.Status.PendingIPs, ptrkiov1alpha1.ScwExternalIPStatusPendingIP{
								IP:     eip,
								Zone:   ip.Zone.String(),
								Reason: "No server could attached to IP",
							})
							break
						}
						continue
					}
					scweip.Status.IPs = append(scweip.Status.IPs, eip)
					scweip.Status.AttachedIPs = append(scweip.Status.AttachedIPs, ptrkiov1alpha1.ScwNodeExternalIP{
						IP:          eip,
						IPID:        ip.ID,
						Zone:        ip.Zone.String(),
						Node:        s.ID,
						NodeID:      s.Name,
						NodeMacAddr: s.specs.MacAddress,
					})
					log.Info("Attached IP", "ip", eip, "nodeID", s.ID, "node", s.Name, "zone", ip.Zone.String(), "macAddr", s.specs.MacAddress)
					break
				}
			}
		} else {
			scweip.Status.PendingIPsCount++
			scweip.Status.PendingIPs = append(scweip.Status.PendingIPs, ptrkiov1alpha1.ScwExternalIPStatusPendingIP{
				IP:     eip,
				Reason: "IP not found in listing",
			})
		}
	}

	scweip.Status.DeletingIPs = []ptrkiov1alpha1.ScwNodeExternalIP{}
	for _, oip := range oldAttached {
		scweip.Status.DeletingIPs = append(scweip.Status.DeletingIPs, oip)
	}

	err = r.cleanup(ctx, scweip.Status.DeletingIPs)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to cleanup old ips: %w", err)
	}

	if err := r.Status().Update(ctx, &scweip); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *ScwExternalIPReconciler) getInstanceIPMap() (map[string]*instance.IP, error) {

	ipMap := make(map[string]*instance.IP)

	resp, err := r.getCachedAPIResponse()
	if err != nil {
		return nil, fmt.Errorf("unable to list scw ips: %w", err)
	}

	for _, ip := range resp.IPs {
		if ip.Type == instance.IPTypeRoutedIPv6 {
			ipMap[ip.Prefix.String()] = ip
		} else if ip.Type == instance.IPTypeRoutedIPv4 {
			ipMap[ip.Address.String()] = ip
		}
	}

	return ipMap, nil
}

// getCachedAPIResponse returns the cached API response if available, otherwise it fetches from the API.
func (r *ScwExternalIPReconciler) getCachedAPIResponse() (*instance.ListIPsResponse, error) {
	if cachedResponse, found := r.Cache.Get(cacheKey); found {
		return cachedResponse.(*instance.ListIPsResponse), nil
	}

	api := instance.NewAPI(r.ScwClient)
	// TODO: fix this quickwin for the region, or at least document
	res, err := api.ListIPs(&instance.ListIPsRequest{}, scw.WithZones(scw.Region(os.Getenv("SCW_REGION")).GetZones()...), scw.WithAllPages())
	if err != nil {
		return nil, err
	}

	r.Cache.Set(cacheKey, res, cache.DefaultExpiration)
	return res, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ScwExternalIPReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &ptrkiov1alpha1.ScwExternalIP{}, serviceField, func(rawObj client.Object) []string {
		eip := rawObj.(*ptrkiov1alpha1.ScwExternalIP)
		if eip.Spec.Service == "" {
			return nil
		}
		return []string{eip.Spec.Service}
	}); err != nil {
		return err
	}

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Node{}, zoneLabel, func(rawObj client.Object) []string {
		node := rawObj.(*corev1.Node)
		if zone := node.ObjectMeta.Labels[corev1.LabelTopologyZone]; zone != "" {
			return []string{zone}
		}
		return nil
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&ptrkiov1alpha1.ScwExternalIP{}).
		Watches(
			&corev1.Service{},
			handler.EnqueueRequestsFromMapFunc(r.findObjectsForService),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Watches(
			&corev1.Node{},
			handler.Funcs{
				CreateFunc: func(ctx context.Context, e event.CreateEvent, q workqueue.RateLimitingInterface) {
					for _, req := range r.findObjectsForNode(ctx, e.Object, "create") {
						q.Add(req)
					}
				},
				UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.RateLimitingInterface) {
					nodeOld := e.ObjectOld.(*corev1.Node)
					nodeNew := e.ObjectNew.(*corev1.Node)
					for _, cOld := range nodeOld.Status.Conditions {
						for _, cNew := range nodeNew.Status.Conditions {
							if cOld.Type == corev1.NodeReady && cOld.Type == cNew.Type {
								if cOld.Status != cNew.Status {
									for _, req := range r.findObjectsForNode(ctx, e.ObjectNew, "update") {
										q.Add(req)
									}
								}
							}
						}
					}
				},
				DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.RateLimitingInterface) {
					for _, req := range r.findObjectsForNode(ctx, e.Object, "delete") {
						q.Add(req)
					}
				},
			},
		).
		Complete(r)
}

func (r *ScwExternalIPReconciler) findObjectsForService(ctx context.Context, service client.Object) []reconcile.Request {
	list := &ptrkiov1alpha1.ScwExternalIPList{}
	listOps := &client.ListOptions{
		FieldSelector: fields.OneTermEqualSelector(serviceField, service.GetName()),
		Namespace:     service.GetNamespace(),
	}
	err := r.List(ctx, list, listOps)
	if err != nil {
		return []reconcile.Request{}
	}

	requests := make([]reconcile.Request, len(list.Items))
	for i, item := range list.Items {
		requests[i] = reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      item.GetName(),
				Namespace: item.GetNamespace(),
			},
		}
	}
	return requests
}

func (r *ScwExternalIPReconciler) findObjectsForNode(ctx context.Context, node client.Object, op string) []reconcile.Request {
	list := &ptrkiov1alpha1.ScwExternalIPList{}
	listOps := &client.ListOptions{}
	err := r.List(ctx, list, listOps)
	if err != nil {
		return []reconcile.Request{}
	}

	requests := []reconcile.Request{}

	// TODO: might need to find a better heuristic
outer:
	for _, item := range list.Items {
		// on delete & update, we just check the nodes listed in the AttachedIPs
		if op == "delete" || op == "update" {
			for _, aip := range item.Status.AttachedIPs {
				if aip.Node == node.GetName() {
					requests = append(requests, reconcile.Request{
						NamespacedName: types.NamespacedName{
							Name:      item.GetName(),
							Namespace: item.GetNamespace(),
						},
					})
					continue outer
				}
			}
		}
		if op == "create" && item.Status.PendingIPsCount > 0 {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      item.GetName(),
					Namespace: item.GetNamespace(),
				},
			})
			continue outer
		}
		nodeObj := node.(*corev1.Node)
		if zone := nodeObj.ObjectMeta.Labels[corev1.LabelTopologyZone]; zone != "" {
			for _, pip := range item.Status.PendingIPs {
				if pip.Zone == zone {
					requests = append(requests, reconcile.Request{
						NamespacedName: types.NamespacedName{
							Name:      item.GetName(),
							Namespace: item.GetNamespace(),
						},
					})
					continue outer
				}
			}
		}
	}
	return requests
}

func (r *ScwExternalIPReconciler) findNodes(ctx context.Context, zone string, nodeSelector map[string]string) ([]NodeNameID, error) {
	if len(nodeSelector) == 0 {
		nodeSelector = make(map[string]string)
	}
	nodeSelector[corev1.LabelTopologyZone] = zone

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes, &client.ListOptions{
		LabelSelector: labels.SelectorFromSet(nodeSelector),
	}); err != nil {
		return nil, fmt.Errorf("unable to list nodes: %w", err)
	}

	var nodeNames = []string{}

	for _, node := range nodes.Items {
		for _, c := range node.Status.Conditions {
			if c.Type == corev1.NodeReady {
				if c.Status == corev1.ConditionTrue {
					nodeNames = append(nodeNames, node.Name)
				}
				break
			}
		}
	}

	var nodeNamesIDs []NodeNameID

	if len(nodeNames) > 0 {
		api := instance.NewAPI(r.ScwClient)
		resp, err := api.ListServers(&instance.ListServersRequest{
			Zone: scw.Zone(zone),
			Name: scw.StringPtr(utils.CommonPrefix(nodeNames)),
		})
		if err != nil {
			return nil, fmt.Errorf("unable to list scw servers: %w", err)
		}

		for _, s := range resp.Servers {
			if slices.Contains(nodeNames, s.Name) {
				nodeNamesIDs = append(nodeNamesIDs, NodeNameID{Name: s.Name, ID: s.ID, specs: *s})
			}
		}
	}

	return nodeNamesIDs, nil
}

func (r *ScwExternalIPReconciler) cleanup(ctx context.Context, attached []ptrkiov1alpha1.ScwNodeExternalIP) error {
	log := log.FromContext(ctx)
	errs := []string{}
	api := instance.NewAPI(r.ScwClient)
	for _, ip := range attached {
		resp, err := api.GetIP(&instance.GetIPRequest{
			Zone: scw.Zone(ip.Zone),
			IP:   ip.IPID,
		})
		if err != nil {
			notFoundError := &scw.ResourceNotFoundError{}
			responseError := &scw.ResponseError{}
			if errors.As(err, &responseError) && responseError.StatusCode == http.StatusNotFound || errors.As(err, &notFoundError) {
				continue
			}
			errs = append(errs, fmt.Sprintf("error getting ip %s in %s: %s", ip.IPID, ip.Zone, err.Error()))
			continue
		}
		if resp.IP.Server != nil && resp.IP.Server.Name == ip.Node {
			_, err = api.UpdateIP(&instance.UpdateIPRequest{
				Zone:   scw.Zone(ip.Zone),
				IP:     ip.IPID,
				Server: &instance.NullableStringValue{Null: true},
			})
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}
			log.Info("Detached IP", "ip", ip.IPID, "zone", ip.Zone, "macAddr", ip.NodeMacAddr)
		}
	}

	if len(errs) == 0 {
		return nil
	}

	return fmt.Errorf("%s", strings.Join(errs, " - "))
}
