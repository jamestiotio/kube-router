package netpol

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"strings"

	"github.com/coreos/go-iptables/iptables"
	"github.com/golang/glog"
	api "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
)

func (npc *NetworkPolicyController) newPodEventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			npc.OnPodAdd(obj)

		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			newPoObj := newObj.(*api.Pod)
			oldPoObj := oldObj.(*api.Pod)
			if newPoObj.Status.Phase != oldPoObj.Status.Phase || newPoObj.Status.PodIP != oldPoObj.Status.PodIP {
				// for the network policies, we are only interested in pod status phase change or IP change
				npc.OnPodUpdate(newObj)
			}
		},
		DeleteFunc: func(obj interface{}) {
			npc.OnPodDelete(obj)
		},
	}
}

// OnPodAdd handles launch of new pod event from the Kubernetes api server
func (npc *NetworkPolicyController) OnPodAdd(obj interface{}) {
	pod := obj.(*api.Pod)
	glog.V(2).Infof("Received pod: %s/%s add event", pod.Namespace, pod.Name)

	npc.RequestFullSync()
}

// OnPodUpdate handles updates to pods event from the Kubernetes api server
func (npc *NetworkPolicyController) OnPodUpdate(obj interface{}) {
	pod := obj.(*api.Pod)
	glog.V(2).Infof("Received pod: %s/%s update event", pod.Namespace, pod.Name)

	npc.RequestFullSync()
}

// OnPodDelete handles delete of a pods event from the Kubernetes api server
func (npc *NetworkPolicyController) OnPodDelete(obj interface{}) {
	pod, ok := obj.(*api.Pod)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			glog.Errorf("unexpected object type: %v", obj)
			return
		}
		if pod, ok = tombstone.Obj.(*api.Pod); !ok {
			glog.Errorf("unexpected object type: %v", obj)
			return
		}
	}
	glog.V(2).Infof("Received pod: %s/%s delete event", pod.Namespace, pod.Name)

	npc.RequestFullSync()
}

func (npc *NetworkPolicyController) syncPodFirewallChains(networkPoliciesInfo []networkPolicyInfo, version string) (map[string]bool, error) {

	activePodFwChains := make(map[string]bool)

	iptablesCmdHandler, err := iptables.New()
	if err != nil {
		glog.Fatalf("Failed to initialize iptables executor: %s", err.Error())
	}

	allLocalPods, err := npc.getLocalPods(npc.nodeIP.String())
	if err != nil {
		return nil, err
	}
	for _, pod := range *allLocalPods {
		// ensure pod specific firewall chain exist for all the pods that need ingress firewall
		podFwChainName := podFirewallChainName(pod.namespace, pod.name, version)
		err = iptablesCmdHandler.NewChain("filter", podFwChainName)
		if err != nil && err.(*iptables.Error).ExitStatus() != 1 {
			return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
		activePodFwChains[podFwChainName] = true

		// setup rules to run pod inbound traffic through applicable ingress network policies
		err = npc.setupPodIngressRules(&pod, podFwChainName, networkPoliciesInfo, iptablesCmdHandler, version)
		if err != nil {
			return nil, err
		}

		// setup rules to run pod outbound traffic through applicable egress network policies
		err = npc.setupPodEgressRules(&pod, podFwChainName, networkPoliciesInfo, iptablesCmdHandler, version)
		if err != nil {
			return nil, err
		}

		// setup rules to intercept inbound traffic to the pods
		err = npc.interceptPodInboundTraffic(&pod, podFwChainName, iptablesCmdHandler)
		if err != nil {
			return nil, err
		}

		// setup rules to intercept outbound traffic from the pods
		err = npc.interceptPodOutboundTraffic(&pod, podFwChainName, iptablesCmdHandler)
		if err != nil {
			return nil, err
		}

		// setup rules to drop the traffic from/to the pods that is not expliclty whitelisted
		err = npc.dropUnmarkedTrafficRules(pod.name, pod.namespace, podFwChainName, iptablesCmdHandler)
		if err != nil {
			return nil, err
		}

		// if the traffic is whitelisted, reset mark to let traffic pass through
		// matching pod firewall chains (only case this happens is when source
		// and destination are on the same pod in which policies for both the pods
		// need to be run through)
		args := []string{"-j", "MARK", "--set-mark", "0/0x10000"}
		err = iptablesCmdHandler.AppendUnique("filter", podFwChainName, args...)
		if err != nil {
			return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}

		// set mark to indicate traffic passed network policies. Mark will be
		// checked to ACCEPT the traffic
		comment := "set mark to ACCEPT traffic that comply to network policies"
		args = []string{"-m", "comment", "--comment", comment, "-j", "MARK", "--set-mark", "0x20000/0x20000"}
		err = iptablesCmdHandler.AppendUnique("filter", podFwChainName, args...)
		if err != nil {
			return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
	}

	return activePodFwChains, nil
}

// setup iptable rules to intercept inbound traffic to pods and run it across the
// firewall chain corresponding to the pod so that ingress network policies are enforced
func (npc *NetworkPolicyController) interceptPodInboundTraffic(pod *podInfo, podFwChainName string, iptablesCmdHandler *iptables.IPTables) error {
	// ensure there is rule in filter table and FORWARD chain to jump to pod specific firewall chain
	// this rule applies to the traffic getting routed (coming for other node pods)
	comment := "rule to jump traffic destined to POD name:" + pod.name + " namespace: " + pod.namespace +
		" to chain " + podFwChainName
	args := []string{"-m", "comment", "--comment", comment, "-d", pod.ip, "-j", podFwChainName}
	exists, err := iptablesCmdHandler.Exists("filter", kubeForwardChainName, args...)
	if err != nil {
		return fmt.Errorf("Failed to run iptables command: %s", err.Error())
	}
	if !exists {
		err := iptablesCmdHandler.Insert("filter", kubeForwardChainName, 1, args...)
		if err != nil {
			return fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
	}

	// ensure there is rule in filter table and OUTPUT chain to jump to pod specific firewall chain
	// this rule applies to the traffic from a pod getting routed back to another pod on same node by service proxy
	exists, err = iptablesCmdHandler.Exists("filter", kubeOutputChainName, args...)
	if err != nil {
		return fmt.Errorf("Failed to run iptables command: %s", err.Error())
	}
	if !exists {
		err := iptablesCmdHandler.Insert("filter", kubeOutputChainName, 1, args...)
		if err != nil {
			return fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
	}

	// ensure there is rule in filter table and forward chain to jump to pod specific firewall chain
	// this rule applies to the traffic getting switched (coming for same node pods)
	comment = "rule to jump traffic destined to POD name:" + pod.name + " namespace: " + pod.namespace +
		" to chain " + podFwChainName
	args = []string{"-m", "physdev", "--physdev-is-bridged",
		"-m", "comment", "--comment", comment,
		"-d", pod.ip,
		"-j", podFwChainName}
	exists, err = iptablesCmdHandler.Exists("filter", kubeForwardChainName, args...)
	if err != nil {
		return fmt.Errorf("Failed to run iptables command: %s", err.Error())
	}
	if !exists {
		err = iptablesCmdHandler.Insert("filter", kubeForwardChainName, 1, args...)
		if err != nil {
			return fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
	}
	return nil
}

// setup iptable rules to intercept outbound traffic from pods and run it across the
// firewall chain corresponding to the pod so that egress network policies are enforced
func (npc *NetworkPolicyController) interceptPodOutboundTraffic(pod *podInfo, podFwChainName string, iptablesCmdHandler *iptables.IPTables) error {
	egressFilterChains := []string{kubeInputChainName, kubeForwardChainName, kubeOutputChainName}
	for _, chain := range egressFilterChains {
		// ensure there is rule in filter table and FORWARD chain to jump to pod specific firewall chain
		// this rule applies to the traffic getting forwarded/routed (traffic from the pod destinted
		// to pod on a different node)
		comment := "rule to jump traffic from POD name:" + pod.name + " namespace: " + pod.namespace +
			" to chain " + podFwChainName
		args := []string{"-m", "comment", "--comment", comment, "-s", pod.ip, "-j", podFwChainName}
		exists, err := iptablesCmdHandler.Exists("filter", chain, args...)
		if err != nil {
			return fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
		if !exists {
			err := iptablesCmdHandler.AppendUnique("filter", chain, args...)
			if err != nil {
				return fmt.Errorf("Failed to run iptables command: %s", err.Error())
			}
		}
	}

	// ensure there is rule in filter table and forward chain to jump to pod specific firewall chain
	// this rule applies to the traffic getting switched (coming for same node pods)
	comment := "rule to jump traffic from POD name:" + pod.name + " namespace: " + pod.namespace +
		" to chain " + podFwChainName
	args := []string{"-m", "physdev", "--physdev-is-bridged",
		"-m", "comment", "--comment", comment,
		"-s", pod.ip,
		"-j", podFwChainName}
	exists, err := iptablesCmdHandler.Exists("filter", kubeForwardChainName, args...)
	if err != nil {
		return fmt.Errorf("Failed to run iptables command: %s", err.Error())
	}
	if !exists {
		err = iptablesCmdHandler.Insert("filter", kubeForwardChainName, 1, args...)
		if err != nil {
			return fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
	}

	return nil
}

// setup rules to jump to applicable network policy chaings for the pod inbound traffic
func (npc *NetworkPolicyController) setupPodIngressRules(pod *podInfo, podFwChainName string, networkPoliciesInfo []networkPolicyInfo, iptablesCmdHandler *iptables.IPTables, version string) error {
	var ingressPoliciesPresent bool
	// add entries in pod firewall to run through required network policies
	for _, policy := range networkPoliciesInfo {
		if _, ok := policy.targetPods[pod.ip]; !ok {
			continue
		}
		ingressPoliciesPresent = true
		comment := "run through nw policy " + policy.name
		policyChainName := networkPolicyChainName(policy.namespace, policy.name, version)
		args := []string{"-m", "comment", "--comment", comment, "-j", policyChainName}
		exists, err := iptablesCmdHandler.Exists("filter", podFwChainName, args...)
		if err != nil {
			return fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
		if !exists {
			err := iptablesCmdHandler.Insert("filter", podFwChainName, 1, args...)
			if err != nil && err.(*iptables.Error).ExitStatus() != 1 {
				return fmt.Errorf("Failed to run iptables command: %s", err.Error())
			}
		}
	}

	if !ingressPoliciesPresent {
		comment := "run through default ingress policy  chain"
		args := []string{"-d", pod.ip, "-m", "comment", "--comment", comment, "-j", kubeIngressNetpolChain}
		exists, err := iptablesCmdHandler.Exists("filter", podFwChainName, args...)
		if err != nil {
			return fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
		if !exists {
			err := iptablesCmdHandler.Insert("filter", podFwChainName, 1, args...)
			if err != nil && err.(*iptables.Error).ExitStatus() != 1 {
				return fmt.Errorf("Failed to run iptables command: %s", err.Error())
			}
		}
	}

	comment := "rule to permit the traffic traffic to pods when source is the pod's local node"
	args := []string{"-m", "comment", "--comment", comment, "-m", "addrtype", "--src-type", "LOCAL", "-d", pod.ip, "-j", "ACCEPT"}
	exists, err := iptablesCmdHandler.Exists("filter", podFwChainName, args...)
	if err != nil {
		return fmt.Errorf("Failed to run iptables command: %s", err.Error())
	}
	if !exists {
		err := iptablesCmdHandler.Insert("filter", podFwChainName, 1, args...)
		if err != nil {
			return fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
	}

	// ensure statefull firewall, that permits return traffic for the traffic originated by the pod
	comment = "rule for stateful firewall for pod"
	args = []string{"-m", "comment", "--comment", comment, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}
	exists, err = iptablesCmdHandler.Exists("filter", podFwChainName, args...)
	if err != nil {
		return fmt.Errorf("Failed to run iptables command: %s", err.Error())
	}
	if !exists {
		err := iptablesCmdHandler.Insert("filter", podFwChainName, 1, args...)
		if err != nil {
			return fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
	}
	return nil
}

// setup rules to jump to applicable network policy chains for the pod outbound traffic
func (npc *NetworkPolicyController) setupPodEgressRules(pod *podInfo, podFwChainName string, networkPoliciesInfo []networkPolicyInfo, iptablesCmdHandler *iptables.IPTables, version string) error {
	var egressPoliciesPresent bool
	// add entries in pod firewall to run through required network policies
	for _, policy := range networkPoliciesInfo {
		if _, ok := policy.targetPods[pod.ip]; !ok {
			continue
		}
		egressPoliciesPresent = true
		comment := "run through nw policy " + policy.name
		policyChainName := networkPolicyChainName(policy.namespace, policy.name, version)
		args := []string{"-m", "comment", "--comment", comment, "-j", policyChainName}
		exists, err := iptablesCmdHandler.Exists("filter", podFwChainName, args...)
		if err != nil {
			return fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
		if !exists {
			err := iptablesCmdHandler.Insert("filter", podFwChainName, 1, args...)
			if err != nil && err.(*iptables.Error).ExitStatus() != 1 {
				return fmt.Errorf("Failed to run iptables command: %s", err.Error())
			}
		}
	}

	if !egressPoliciesPresent {
		comment := "run through default egress policy  chain"
		args := []string{"-s", pod.ip, "-m", "comment", "--comment", comment, "-j", kubeEgressNetpolChain}
		exists, err := iptablesCmdHandler.Exists("filter", podFwChainName, args...)
		if err != nil {
			return fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
		if !exists {
			err := iptablesCmdHandler.Insert("filter", podFwChainName, 1, args...)
			if err != nil && err.(*iptables.Error).ExitStatus() != 1 {
				return fmt.Errorf("Failed to run iptables command: %s", err.Error())
			}
		}
	}

	// ensure statefull firewall, that permits return traffic for the traffic originated by the pod
	comment := "rule for stateful firewall for pod"
	args := []string{"-m", "comment", "--comment", comment, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}
	exists, err := iptablesCmdHandler.Exists("filter", podFwChainName, args...)
	if err != nil {
		return fmt.Errorf("Failed to run iptables command: %s", err.Error())
	}
	if !exists {
		err := iptablesCmdHandler.Insert("filter", podFwChainName, 1, args...)
		if err != nil {
			return fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
	}
	return nil
}

func (npc *NetworkPolicyController) dropUnmarkedTrafficRules(podName, podNamespace, podFwChainName string, iptablesCmdHandler *iptables.IPTables) error {
	// add rule to log the packets that will be dropped due to network policy enforcement
	comment := "rule to log dropped traffic POD name:" + podName + " namespace: " + podNamespace
	args := []string{"-m", "comment", "--comment", comment, "-m", "mark", "!", "--mark", "0x10000/0x10000", "-j", "NFLOG", "--nflog-group", "100", "-m", "limit", "--limit", "10/minute", "--limit-burst", "10"}
	err := iptablesCmdHandler.AppendUnique("filter", podFwChainName, args...)
	if err != nil {
		return fmt.Errorf("Failed to run iptables command: %s", err.Error())
	}

	// add rule to DROP if no applicable network policy permits the traffic
	comment = "rule to REJECT traffic destined for POD name:" + podName + " namespace: " + podNamespace
	args = []string{"-m", "comment", "--comment", comment, "-m", "mark", "!", "--mark", "0x10000/0x10000", "-j", "REJECT"}
	err = iptablesCmdHandler.AppendUnique("filter", podFwChainName, args...)
	if err != nil {
		return fmt.Errorf("Failed to run iptables command: %s", err.Error())
	}

	return nil
}

func (npc *NetworkPolicyController) getLocalPods(nodeIP string) (*map[string]podInfo, error) {
	localPods := make(map[string]podInfo)
	for _, obj := range npc.podLister.List() {
		pod := obj.(*api.Pod)
		// skip pods not local to the node
		if strings.Compare(pod.Status.HostIP, nodeIP) != 0 {
			continue
		}
		// skip pods in trasient state
		if len(pod.Status.PodIP) == 0 || pod.Status.PodIP == "" {
			continue
		}
		localPods[pod.Status.PodIP] = podInfo{ip: pod.Status.PodIP,
			name:      pod.ObjectMeta.Name,
			namespace: pod.ObjectMeta.Namespace,
			labels:    pod.ObjectMeta.Labels}
	}
	return &localPods, nil
}

func podFirewallChainName(namespace, podName string, version string) string {
	hash := sha256.Sum256([]byte(namespace + podName + version))
	encoded := base32.StdEncoding.EncodeToString(hash[:])
	return kubePodFirewallChainPrefix + encoded[:16]
}