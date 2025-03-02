package deschedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/intel/platform-aware-scheduling/telemetry-aware-scheduling/pkg/cache"
	strategy "github.com/intel/platform-aware-scheduling/telemetry-aware-scheduling/pkg/strategies/core"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

type violationList map[string][]string

type patchValue struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value string `json:"value"`
}

//Cleanup remove node labels for violating when policy is deleted
func (d *Strategy) Cleanup(enforcer *strategy.MetricEnforcer, policyName string) error {
	lbls := metav1.LabelSelector{MatchLabels: map[string]string{policyName: "violating"}}
	nodes, err := enforcer.KubeClient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: labels.Set(lbls.MatchLabels).String()})
	if err != nil {
		msg := fmt.Sprintf("cannot list nodes: %v", err)
		klog.V(2).InfoS(msg, "component", "controller")
		return err
	}
	for _, node := range nodes.Items {
		var payload []patchValue
		if _, ok := node.Labels[policyName]; ok {
			payload = append(payload,
				patchValue{
					Op:   "remove",
					Path: "/metadata/labels/" + policyName,
				})
		}
		err := d.patchNode(node.Name, enforcer, payload)
		if err != nil {
			klog.V(2).InfoS(err.Error(), "component", "controller")
		}
	}
	klog.V(2).InfoS(fmt.Sprintf("Remove the node label on policy %v deletion", policyName), "component", "controller")
	return nil
}

//Enforce describes the behavior followed by this strategy to return associated pods to non-violating status.
//For descheduling enforcement is done by labelling the nodes as violators. This label can then be used externally, for example by descheduler, to remedy the situation.
//Here we make an api call to list all nodes first. This may be improved by using a controller instead or some other way of not waiting for the API call every time Enforce is called.
func (d *Strategy) Enforce(enforcer *strategy.MetricEnforcer, cache cache.Reader) (int, error) {
	nodes, err := enforcer.KubeClient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		msg := fmt.Sprintf("cannot list nodes: %v", err)
		klog.V(2).InfoS(msg, "component", "controller")
		return -1, err
	}
	list := d.nodeStatusForStrategy(enforcer, cache)
	numberViolations, err := d.updateNodeLabels(enforcer, list, nodes)
	if err != nil {
		klog.V(2).InfoS(err.Error(), "component", "controller")
		return -1, err
	}
	return numberViolations, nil
}

//patch node takes a json patch value and sends it to the API server to patch a node. Here it's used to label nodes.
func (d *Strategy) patchNode(nodeName string, enforcer *strategy.MetricEnforcer, payload []patchValue) error {
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		klog.V(4).InfoS(err.Error(), "component", "controller")
		return err
	}
	_, err = enforcer.KubeClient.CoreV1().Nodes().Patch(context.TODO(), nodeName, types.JSONPatchType, jsonPayload, metav1.PatchOptions{})
	if err != nil {
		klog.V(4).InfoS(err.Error(), "component", "controller")
		return err
	}
	return nil
}

//This returns a list the set of all policies registered with the enforcer
func allPolicies(enforcer *strategy.MetricEnforcer) map[string]interface{} {
	policies := map[string]interface{}{}
	for k := range enforcer.RegisteredStrategies[StrategyType] {
		policies[k.GetPolicyName()] = nil
	}
	return policies
}

//updateNodeLabels takes the list of nodes violating the strategy. It then sets the payloads for labelling them as violators and calls for them to be labelled.

func (d *Strategy) updateNodeLabels(enforcer *strategy.MetricEnforcer, viols violationList, allNodes *v1.NodeList) (int, error) {
	totalViolations := 0
	labelErrs := ""
	var errOut error = nil
	var nonViolatedPolicies map[string]interface{}
	for _, node := range allNodes.Items {
		payload := []patchValue{}
		nonViolatedPolicies = allPolicies(enforcer)
		violatedPolicies := ""
		for _, policyName := range viols[node.Name] {
			delete(nonViolatedPolicies, policyName)
			payload = append(payload,
				patchValue{
					Op:    "add",
					Path:  "/metadata/labels/" + policyName,
					Value: "violating",
				})
			violatedPolicies += policyName + ", "
		}
		for policyName := range nonViolatedPolicies {
			if _, ok := node.Labels[policyName]; ok {
				// There is a duplication of work here - both label added as null and label removed. Due to some oddness in behaviour on remove label.
				//TODO: Decide which behaviour is better. This leaves a constant label on every node for every strategy in the enforcer.
				payload = append(payload,
					patchValue{
						Op:   "remove",
						Path: "/metadata/labels/" + policyName,
					})
				payload = append(payload, patchValue{
					Op:    "add",
					Path:  "/metadata/labels/" + policyName,
					Value: "null",
				})
			}
			totalViolations++
		}
		err := d.patchNode(node.Name, enforcer, payload)
		if err != nil {
			if len(labelErrs) == 0 {
				labelErrs = "could not label: "
			}
			klog.V(4).InfoS(err.Error(), "component", "controller")
			labelErrs = labelErrs + node.Name + ": [ " + violatedPolicies + " ]; "
		}
		if len(violatedPolicies) > 0 {
			klog.V(2).InfoS("Node "+node.Name+" violating "+violatedPolicies, "component", "controller")
		}
	}
	if len(labelErrs) > 0 {
		errOut = errors.New(labelErrs)
	}
	return totalViolations, errOut
}

//nodeStatusForStrategy returns a list of nodes that are violating the given strategy by calling the strategies Violated method.
func (d *Strategy) nodeStatusForStrategy(enforcer *strategy.MetricEnforcer, cache cache.Reader) violationList {
	violations := violationList{}
	for strat := range enforcer.RegisteredStrategies[StrategyType] {
		klog.V(2).InfoS("Evaluating "+strat.GetPolicyName(), "component", "controller")
		nodes := strat.Violated(cache)
		for node := range nodes {
			violations[node] = append(violations[node], strat.GetPolicyName())
		}
	}
	return violations
}
