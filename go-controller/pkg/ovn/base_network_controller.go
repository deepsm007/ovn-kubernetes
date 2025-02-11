package ovn

import (
	"context"
	"fmt"
	"math"
	"net"
	"strconv"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	ovnlb "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/loadbalancer"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/subnetallocator"
	ovnretry "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

// CommonNetworkControllerInfo structure is place holder for all fields shared among controllers.
type CommonNetworkControllerInfo struct {
	client       clientset.Interface
	kube         kube.Interface
	watchFactory *factory.WatchFactory
	podRecorder  *metrics.PodRecorder

	// event recorder used to post events to k8s
	recorder record.EventRecorder

	// libovsdb northbound client interface
	nbClient libovsdbclient.Client

	// libovsdb southbound client interface
	sbClient libovsdbclient.Client

	// has SCTP support
	SCTPSupport bool

	// Supports multicast?
	multicastSupport bool
}

// BaseNetworkController structure holds per-network fields and network specific configuration
// Note that all the methods with NetworkControllerInfo pointer receivers will be called
// by more than one type of network controllers.
type BaseNetworkController struct {
	CommonNetworkControllerInfo

	// retry framework for pods
	retryPods *ovnretry.RetryFramework
	// retry framework for nodes
	retryNodes *ovnretry.RetryFramework

	// pod events factory handler
	podHandler *factory.Handler
	// node events factory handler
	nodeHandler *factory.Handler

	// A cache of all logical switches seen by the watcher and their subnets
	lsManager *lsm.LogicalSwitchManager

	// A cache of all logical ports known to the controller
	logicalPortCache *portCache

	// Info about known namespaces. You must use oc.getNamespaceLocked() or
	// oc.waitForNamespaceLocked() to read this map, and oc.createNamespaceLocked()
	// or oc.deleteNamespaceLocked() to modify it. namespacesMutex is only held
	// from inside those functions.
	namespaces      map[string]*namespaceInfo
	namespacesMutex sync.Mutex

	// An address set factory that creates address sets
	addressSetFactory addressset.AddressSetFactory

	// stopChan per controller
	stopChan chan struct{}
}

// NewCommonNetworkControllerInfo creates CommonNetworkControllerInfo shared by controllers
func NewCommonNetworkControllerInfo(client clientset.Interface, kube kube.Interface, wf *factory.WatchFactory,
	recorder record.EventRecorder, nbClient libovsdbclient.Client, sbClient libovsdbclient.Client,
	podRecorder *metrics.PodRecorder, SCTPSupport, multicastSupport bool) *CommonNetworkControllerInfo {
	return &CommonNetworkControllerInfo{
		client:           client,
		kube:             kube,
		watchFactory:     wf,
		recorder:         recorder,
		nbClient:         nbClient,
		sbClient:         sbClient,
		podRecorder:      podRecorder,
		SCTPSupport:      SCTPSupport,
		multicastSupport: multicastSupport,
	}
}

// createOvnClusterRouter creates the central router for the network
func (bnc *BaseNetworkController) createOvnClusterRouter() (*nbdb.LogicalRouter, error) {
	// Create default Control Plane Protection (COPP) entry for routers
	defaultCOPPUUID, err := EnsureDefaultCOPP(bnc.nbClient)
	if err != nil {
		return nil, fmt.Errorf("unable to create router control plane protection: %w", err)
	}

	// Create a single common distributed router for the cluster.
	logicalRouterName := types.OVNClusterRouter
	logicalRouter := nbdb.LogicalRouter{
		Name: logicalRouterName,
		ExternalIDs: map[string]string{
			"k8s-cluster-router": "yes",
		},
		Options: map[string]string{
			"always_learn_from_arp_request": "false",
		},
		Copp: &defaultCOPPUUID,
	}
	if bnc.multicastSupport {
		logicalRouter.Options = map[string]string{
			"mcast_relay": "true",
		}
	}

	err = libovsdbops.CreateOrUpdateLogicalRouter(bnc.nbClient, &logicalRouter)
	if err != nil {
		return nil, fmt.Errorf("failed to create distributed router %s, error: %v",
			logicalRouterName, err)
	}

	return &logicalRouter, nil
}

// syncNodeClusterRouterPort ensures a node's LS to the cluster router's LRP is created.
// NOTE: We could have created the router port in ensureNodeLogicalNetwork() instead of here,
// but chassis ID is not available at that moment. We need the chassis ID to set the
// gateway-chassis, which in effect pins the logical switch to the current node in OVN.
// Otherwise, ovn-controller will flood-fill unrelated datapaths unnecessarily, causing scale
// problems.
func (bnc *BaseNetworkController) syncNodeClusterRouterPort(node *kapi.Node, hostSubnets []*net.IPNet) error {
	chassisID, err := util.ParseNodeChassisIDAnnotation(node)
	if err != nil {
		return err
	}

	if len(hostSubnets) == 0 {
		hostSubnets, err = util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
		if err != nil {
			return err
		}
	}

	// logical router port MAC is based on IPv4 subnet if there is one, else IPv6
	var nodeLRPMAC net.HardwareAddr
	for _, hostSubnet := range hostSubnets {
		gwIfAddr := util.GetNodeGatewayIfAddr(hostSubnet)
		nodeLRPMAC = util.IPAddrToHWAddr(gwIfAddr.IP)
		if !utilnet.IsIPv6CIDR(hostSubnet) {
			break
		}
	}

	switchName := node.Name
	logicalRouterName := types.OVNClusterRouter
	lrpName := types.RouterToSwitchPrefix + switchName
	lrpNetworks := []string{}
	for _, hostSubnet := range hostSubnets {
		gwIfAddr := util.GetNodeGatewayIfAddr(hostSubnet)
		lrpNetworks = append(lrpNetworks, gwIfAddr.String())
	}
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name:     lrpName,
		MAC:      nodeLRPMAC.String(),
		Networks: lrpNetworks,
	}
	logicalRouter := nbdb.LogicalRouter{Name: logicalRouterName}
	gatewayChassis := nbdb.GatewayChassis{
		Name:        lrpName + "-" + chassisID,
		ChassisName: chassisID,
		Priority:    1,
	}

	err = libovsdbops.CreateOrUpdateLogicalRouterPort(bnc.nbClient, &logicalRouter, &logicalRouterPort,
		&gatewayChassis, &logicalRouterPort.MAC, &logicalRouterPort.Networks)
	if err != nil {
		klog.Errorf("Failed to add gateway chassis %s to logical router port %s, error: %v", chassisID, lrpName, err)
		return err
	}

	return nil
}

func (bnc *BaseNetworkController) createNodeLogicalSwitch(nodeName string, hostSubnets []*net.IPNet,
	loadBalancerGroupUUID string) error {
	// logical router port MAC is based on IPv4 subnet if there is one, else IPv6
	var nodeLRPMAC net.HardwareAddr
	switchName := nodeName
	for _, hostSubnet := range hostSubnets {
		gwIfAddr := util.GetNodeGatewayIfAddr(hostSubnet)
		nodeLRPMAC = util.IPAddrToHWAddr(gwIfAddr.IP)
		if !utilnet.IsIPv6CIDR(hostSubnet) {
			break
		}
	}

	logicalSwitch := nbdb.LogicalSwitch{
		Name: switchName,
	}

	var v4Gateway, v6Gateway net.IP
	logicalSwitch.OtherConfig = map[string]string{}
	for _, hostSubnet := range hostSubnets {
		gwIfAddr := util.GetNodeGatewayIfAddr(hostSubnet)
		mgmtIfAddr := util.GetNodeManagementIfAddr(hostSubnet)

		if utilnet.IsIPv6CIDR(hostSubnet) {
			v6Gateway = gwIfAddr.IP

			logicalSwitch.OtherConfig["ipv6_prefix"] =
				hostSubnet.IP.String()
		} else {
			v4Gateway = gwIfAddr.IP
			excludeIPs := mgmtIfAddr.IP.String()
			if config.HybridOverlay.Enabled {
				hybridOverlayIfAddr := util.GetNodeHybridOverlayIfAddr(hostSubnet)
				excludeIPs += ".." + hybridOverlayIfAddr.IP.String()
			}
			logicalSwitch.OtherConfig["subnet"] = hostSubnet.String()
			logicalSwitch.OtherConfig["exclude_ips"] = excludeIPs
		}
	}

	if loadBalancerGroupUUID != "" {
		logicalSwitch.LoadBalancerGroup = []string{loadBalancerGroupUUID}
	}

	// If supported, enable IGMP/MLD snooping and querier on the node.
	if bnc.multicastSupport {
		logicalSwitch.OtherConfig["mcast_snoop"] = "true"

		// Configure IGMP/MLD querier if the gateway IP address is known.
		// Otherwise disable it.
		if v4Gateway != nil || v6Gateway != nil {
			logicalSwitch.OtherConfig["mcast_querier"] = "true"
			logicalSwitch.OtherConfig["mcast_eth_src"] = nodeLRPMAC.String()
			if v4Gateway != nil {
				logicalSwitch.OtherConfig["mcast_ip4_src"] = v4Gateway.String()
			}
			if v6Gateway != nil {
				logicalSwitch.OtherConfig["mcast_ip6_src"] = util.HWAddrToIPv6LLA(nodeLRPMAC).String()
			}
		} else {
			logicalSwitch.OtherConfig["mcast_querier"] = "false"
		}
	}

	err := libovsdbops.CreateOrUpdateLogicalSwitch(bnc.nbClient, &logicalSwitch, &logicalSwitch.OtherConfig,
		&logicalSwitch.LoadBalancerGroup)
	if err != nil {
		return fmt.Errorf("failed to add logical switch %+v: %v", logicalSwitch, err)
	}

	// Connect the switch to the router.
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      types.SwitchToRouterPrefix + switchName,
		Type:      "router",
		Addresses: []string{"router"},
		Options:   map[string]string{"router-port": types.RouterToSwitchPrefix + switchName},
	}
	sw := nbdb.LogicalSwitch{Name: switchName}
	err = libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(bnc.nbClient, &sw, &logicalSwitchPort)
	if err != nil {
		klog.Errorf("Failed to add logical port %+v to switch %s: %v", logicalSwitchPort, switchName, err)
		return err
	}

	// multicast is only supported in default network for now
	if bnc.multicastSupport {
		err = libovsdbops.AddPortsToPortGroup(bnc.nbClient, types.ClusterRtrPortGroupName, logicalSwitchPort.UUID)
		if err != nil {
			klog.Errorf(err.Error())
			return err
		}
	}

	// Add the switch to the logical switch cache
	return bnc.lsManager.AddSwitch(logicalSwitch.Name, logicalSwitch.UUID, hostSubnets)
}

func (bnc *BaseNetworkController) allocateNodeSubnets(node *kapi.Node,
	masterSubnetAllocator *subnetallocator.HostSubnetAllocator) ([]*net.IPNet, error) {
	existingSubnets, err := util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
	if err != nil && !util.IsAnnotationNotSetError(err) {
		// Log the error and try to allocate new subnets
		klog.Infof("Failed to get node %s host subnets annotations: %v", node.Name, err)
	}

	hostSubnets, allocatedSubnets, err := masterSubnetAllocator.AllocateNodeSubnets(node.Name, existingSubnets, config.IPv4Mode, config.IPv6Mode)
	if err != nil {
		return nil, err
	}
	// Release the allocation on error
	defer func() {
		if err != nil {
			if errR := masterSubnetAllocator.ReleaseNodeSubnets(node.Name, allocatedSubnets...); errR != nil {
				klog.Warningf("Error releasing node %s subnets: %v", node.Name, errR)
			}
		}
	}()

	return hostSubnets, nil
}

// UpdateNodeAnnotationWithRetry update node's hostSubnet annotation (possibly for multiple networks) and the
// other given node annotations
func (bnc *BaseNetworkController) UpdateNodeAnnotationWithRetry(nodeName string, hostSubnetsMap map[string][]*net.IPNet,
	otherUpdatedNodeAnnotation map[string]string) error {
	// Retry if it fails because of potential conflict which is transient. Return error in the
	// case of other errors (say temporary API server down), and it will be taken care of by the
	// retry mechanism.
	resultErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		// Informer cache should not be mutated, so get a copy of the object
		node, err := bnc.watchFactory.GetNode(nodeName)
		if err != nil {
			return err
		}

		cnode := node.DeepCopy()
		for netName, hostSubnets := range hostSubnetsMap {
			cnode.Annotations, err = util.UpdateNodeHostSubnetAnnotation(cnode.Annotations, hostSubnets, netName)
			if err != nil {
				return fmt.Errorf("failed to update node %q annotation subnet %s",
					node.Name, util.JoinIPNets(hostSubnets, ","))
			}
		}
		for k, v := range otherUpdatedNodeAnnotation {
			cnode.Annotations[k] = v
		}
		return bnc.kube.PatchNode(node, cnode)
	})
	if resultErr != nil {
		return fmt.Errorf("failed to update node %s annotation", nodeName)
	}
	return nil
}

// deleteNodeLogicalNetwork removes the logical switch and logical router port associated with the node
func (bnc *BaseNetworkController) deleteNodeLogicalNetwork(nodeName string) error {
	switchName := nodeName
	// Remove switch to lb associations from the LBCache before removing the switch
	lbCache, err := ovnlb.GetLBCache(bnc.nbClient)
	if err != nil {
		return fmt.Errorf("failed to get load_balancer cache for node %s: %v", nodeName, err)
	}
	lbCache.RemoveSwitch(switchName)

	// Remove the logical switch associated with the node
	err = libovsdbops.DeleteLogicalSwitch(bnc.nbClient, switchName)
	if err != nil {
		return fmt.Errorf("failed to delete logical switch %s: %v", switchName, err)
	}

	logicalRouterName := types.OVNClusterRouter
	logicalRouter := nbdb.LogicalRouter{Name: logicalRouterName}
	logicalRouterPort := nbdb.LogicalRouterPort{
		Name: types.RouterToSwitchPrefix + switchName,
	}
	err = libovsdbops.DeleteLogicalRouterPorts(bnc.nbClient, &logicalRouter, &logicalRouterPort)
	if err != nil {
		return fmt.Errorf("failed to delete router port %s: %v", logicalRouterPort.Name, err)
	}

	return nil
}

// updates the list of nodes if the given node manages its hostSubnets; returns its hostSubnets if any
func (bnc *BaseNetworkController) updateNodesManageHostSubnets(node *kapi.Node,
	masterSubnetAllocator *subnetallocator.HostSubnetAllocator, foundNodes sets.String) []*net.IPNet {
	if noHostSubnet(node) {
		return []*net.IPNet{}
	}
	hostSubnets, _ := util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
	foundNodes.Insert(node.Name)

	klog.V(5).Infof("Node %s contains subnets: %v", node.Name, hostSubnets)
	if err := masterSubnetAllocator.MarkSubnetsAllocated(node.Name, hostSubnets...); err != nil {
		utilruntime.HandleError(err)
	}
	return hostSubnets
}

func (bnc *BaseNetworkController) addAllPodsOnNode(nodeName string) []error {
	errs := []error{}
	options := metav1.ListOptions{
		FieldSelector:   fields.OneTermEqualSelector("spec.nodeName", nodeName).String(),
		ResourceVersion: "0",
	}
	pods, err := bnc.client.CoreV1().Pods(metav1.NamespaceAll).List(context.TODO(), options)
	if err != nil {
		errs = append(errs, err)
		klog.Errorf("Unable to list existing pods on node: %s, existing pods on this node may not function",
			nodeName)
	} else {
		klog.V(5).Infof("When adding node %s, found %d pods to add to retryPods", nodeName, len(pods.Items))
		for _, pod := range pods.Items {
			pod := pod
			if util.PodCompleted(&pod) {
				continue
			}
			klog.V(5).Infof("Adding pod %s/%s to retryPods", pod.Namespace, pod.Name)
			err = bnc.retryPods.AddRetryObjWithAddNoBackoff(&pod)
			if err != nil {
				errs = append(errs, err)
				klog.Errorf("Failed to add pod %s/%s to retryPods: %v", pod.Namespace, pod.Name, err)
			}
		}
	}
	bnc.retryPods.RequestRetryObjs()
	return errs
}

func (bnc *BaseNetworkController) updateL3TopologyVersion() error {
	currentTopologyVersion := strconv.Itoa(types.OvnCurrentTopologyVersion)
	clusterRouterName := types.OVNClusterRouter
	logicalRouter := nbdb.LogicalRouter{
		Name:        clusterRouterName,
		ExternalIDs: map[string]string{"k8s-ovn-topo-version": currentTopologyVersion},
	}
	err := libovsdbops.UpdateLogicalRouterSetExternalIDs(bnc.nbClient, &logicalRouter)
	if err != nil {
		return fmt.Errorf("failed to generate set topology version, err: %v", err)
	}
	klog.Infof("Updated Logical_Router %s topology version to %s", clusterRouterName, currentTopologyVersion)
	return nil
}

// determineOVNTopoVersionFromOVN determines what OVN Topology version is being used
// If "k8s-ovn-topo-version" key in external_ids column does not exist, it is prior to OVN topology versioning
// and therefore set version number to OvnCurrentTopologyVersion
func (bnc *BaseNetworkController) determineOVNTopoVersionFromOVN() (int, error) {
	clusterRouterName := types.OVNClusterRouter
	logicalRouter := &nbdb.LogicalRouter{Name: clusterRouterName}
	logicalRouter, err := libovsdbops.GetLogicalRouter(bnc.nbClient, logicalRouter)
	if err != nil && err != libovsdbclient.ErrNotFound {
		return 0, fmt.Errorf("error getting router %s: %v", clusterRouterName, err)
	}
	if err == libovsdbclient.ErrNotFound {
		// no OVNClusterRouter exists, DB is empty, nothing to upgrade
		return math.MaxInt32, nil
	}
	v, exists := logicalRouter.ExternalIDs["k8s-ovn-topo-version"]
	if !exists {
		klog.Infof("No version string found. The OVN topology is before versioning is introduced. Upgrade needed")
		return 0, nil
	}
	ver, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid OVN topology version string for the cluster, err: %v", err)
	}
	return ver, nil
}

// getNamespaceLocked locks namespacesMutex, looks up ns, and (if found), returns it with
// its mutex locked. If ns is not known, nil will be returned
func (bnc *BaseNetworkController) getNamespaceLocked(ns string, readOnly bool) (*namespaceInfo, func()) {
	// Only hold namespacesMutex while reading/modifying oc.namespaces. In particular,
	// we drop namespacesMutex while trying to claim nsInfo.Mutex, because something
	// else might have locked the nsInfo and be doing something slow with it, and we
	// don't want to block all access to oc.namespaces while that's happening.
	bnc.namespacesMutex.Lock()
	nsInfo := bnc.namespaces[ns]
	bnc.namespacesMutex.Unlock()

	if nsInfo == nil {
		return nil, nil
	}
	var unlockFunc func()
	if readOnly {
		unlockFunc = func() { nsInfo.RUnlock() }
		nsInfo.RLock()
	} else {
		unlockFunc = func() { nsInfo.Unlock() }
		nsInfo.Lock()
	}
	// Check that the namespace wasn't deleted while we were waiting for the lock
	bnc.namespacesMutex.Lock()
	defer bnc.namespacesMutex.Unlock()
	if nsInfo != bnc.namespaces[ns] {
		unlockFunc()
		return nil, nil
	}
	return nsInfo, unlockFunc
}

// deleteNamespaceLocked locks namespacesMutex, finds and deletes ns, and returns the
// namespace, locked.
func (bnc *BaseNetworkController) deleteNamespaceLocked(ns string) *namespaceInfo {
	// The locking here is the same as in getNamespaceLocked

	bnc.namespacesMutex.Lock()
	nsInfo := bnc.namespaces[ns]
	bnc.namespacesMutex.Unlock()

	if nsInfo == nil {
		return nil
	}
	nsInfo.Lock()

	bnc.namespacesMutex.Lock()
	defer bnc.namespacesMutex.Unlock()
	if nsInfo != bnc.namespaces[ns] {
		nsInfo.Unlock()
		return nil
	}
	if nsInfo.addressSet != nil {
		// Empty the address set, then delete it after an interval.
		if err := nsInfo.addressSet.SetIPs(nil); err != nil {
			klog.Errorf("Warning: failed to empty address set for deleted NS %s: %v", ns, err)
		}

		// Delete the address set after a short delay.
		// This is so NetworkPolicy handlers can converge and stop referencing it.
		addressSet := nsInfo.addressSet
		go func() {
			select {
			case <-bnc.stopChan:
				return
			case <-time.After(20 * time.Second):
				// Check to see if the NS was re-added in the meanwhile. If so,
				// only delete if the new NS's AddressSet shouldn't exist.
				nsInfo, nsUnlock := bnc.getNamespaceLocked(ns, true)
				if nsInfo != nil {
					defer nsUnlock()
					if nsInfo.addressSet != nil {
						klog.V(5).Infof("Skipping deferred deletion of AddressSet for NS %s: re-created", ns)
						return
					}
				}

				klog.V(5).Infof("Finishing deferred deletion of AddressSet for NS %s", ns)
				if err := addressSet.Destroy(); err != nil {
					klog.Errorf("Failed to delete AddressSet for NS %s: %v", ns, err.Error())
				}
			}
		}()
	}
	delete(bnc.namespaces, ns)

	return nsInfo
}

// WatchNodes starts the watching of the nodes resource and calls back the appropriate handler logic
func (bnc *BaseNetworkController) WatchNodes() error {
	if bnc.nodeHandler != nil {
		return nil
	}

	handler, err := bnc.retryNodes.WatchResource()
	if err == nil {
		bnc.nodeHandler = handler
	}
	return err
}
