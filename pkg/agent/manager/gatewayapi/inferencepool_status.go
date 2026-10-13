/*
* Copyright (c) 2026 NetLOX Inc
*
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You may obtain a copy of the License at:
*
*     http://www.apache.org/licenses/LICENSE-2.0
*
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
 */

package gatewayapi

import (
	"context"
	"reflect"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	infv1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
)

// inferencePoolControllerName - what this manager writes into the parent status
// it owns. Every implementation stamps its own, which is how several of them
// can report on the same pool without overwriting each other.
const inferencePoolControllerName = "loxilb.io/kube-loxilb"

// statusReport - the two conditions the Inference Extension defines for a pool,
// as this reconcile found them.
type statusReport struct {
	accepted        bool
	acceptedReason  string
	acceptedMessage string

	resolvedRefs        bool
	resolvedRefsReason  string
	resolvedRefsMessage string
}

// updateStatus - write our conditions under each Gateway serving this pool.
//
// Status is per parent: the same pool can be accepted by one Gateway and
// refused by another, so a single top-level condition could not express it.
func (m *InferencePoolManager) updateStatus(ctx context.Context, pool *infv1.InferencePool, targets []poolTarget, report statusReport) error {
	parents := make([]infv1.ParentStatus, 0, len(targets))
	seen := map[string]bool{}

	for _, target := range targets {
		key := target.gateway.Namespace + "/" + target.gateway.Name
		if seen[key] {
			// Two routes through the same Gateway are one parent.
			continue
		}
		seen[key] = true

		ref := infv1.ParentReference{
			Group:     groupOf("gateway.networking.k8s.io"),
			Kind:      "Gateway",
			Name:      infv1.ObjectName(target.gateway.Name),
			Namespace: infv1.Namespace(target.gateway.Namespace),
		}
		parents = append(parents, infv1.ParentStatus{
			ParentRef:      ref,
			ControllerName: inferencePoolControllerName,
			Conditions:     carryTransitionTimes(ourConditions(pool, ref), report.conditions(pool.Generation)),
		})
	}

	sort.Slice(parents, func(i, j int) bool {
		if parents[i].ParentRef.Namespace != parents[j].ParentRef.Namespace {
			return parents[i].ParentRef.Namespace < parents[j].ParentRef.Namespace
		}
		return parents[i].ParentRef.Name < parents[j].ParentRef.Name
	})

	return m.writeParents(ctx, pool, parents)
}

// ourConditions - the conditions this controller last wrote under ref.
func ourConditions(pool *infv1.InferencePool, ref infv1.ParentReference) []metav1.Condition {
	for _, parent := range pool.Status.Parents {
		if parent.ControllerName == inferencePoolControllerName && reflect.DeepEqual(parent.ParentRef, ref) {
			return parent.Conditions
		}
	}
	return nil
}

// carryTransitionTimes - stamp next with when each condition last changed.
//
// A condition whose status is what it was keeps its previous transition time;
// one that is new or changed status gets now. That is the meaning
// meta.SetStatusCondition gives lastTransitionTime, and it is also what lets
// writeParents tell an unchanged reconcile from a changed one. Stamping every
// condition with now made each reconcile a status write, and each write an
// update event that queued the pool again.
func carryTransitionTimes(previous, next []metav1.Condition) []metav1.Condition {
	now := metav1.Now()
	for i := range next {
		next[i].LastTransitionTime = now
		for _, old := range previous {
			if old.Type == next[i].Type && old.Status == next[i].Status {
				next[i].LastTransitionTime = old.LastTransitionTime
				break
			}
		}
	}
	return next
}

// clearStatus - drop the parents we own, leaving other controllers' entries.
func (m *InferencePoolManager) clearStatus(ctx context.Context, pool *infv1.InferencePool) error {
	return m.writeParents(ctx, pool, nil)
}

func (m *InferencePoolManager) writeParents(ctx context.Context, pool *infv1.InferencePool, ours []infv1.ParentStatus) error {
	merged := mergeParents(pool.Status.Parents, ours)
	if reflect.DeepEqual(pool.Status.Parents, merged) {
		return nil
	}

	updated := pool.DeepCopy()
	updated.Status.Parents = merged

	if _, err := m.infClient.InferenceV1().InferencePools(updated.Namespace).
		UpdateStatus(ctx, updated, metav1.UpdateOptions{}); err != nil {
		klog.Errorf("InferencePool %s/%s: unable to update status: %v", pool.Namespace, pool.Name, err)
		return err
	}

	return nil
}

// mergeParents - replace the entries this controller owns, keep the rest.
//
// A pool can be attached to gateways from different implementations, and each
// one reports under its own controllerName. Rewriting the whole list would
// delete their reports.
func mergeParents(existing, ours []infv1.ParentStatus) []infv1.ParentStatus {
	merged := make([]infv1.ParentStatus, 0, len(existing)+len(ours))
	for _, parent := range existing {
		if parent.ControllerName == inferencePoolControllerName {
			continue
		}
		merged = append(merged, parent)
	}

	return append(merged, ours...)
}

// conditions - Accepted and ResolvedRefs as this report describes them.
func (r statusReport) conditions(generation int64) []metav1.Condition {
	return []metav1.Condition{
		condition(string(infv1.InferencePoolConditionAccepted), r.accepted, r.acceptedReason, r.acceptedMessage, generation),
		condition(string(infv1.InferencePoolConditionResolvedRefs), r.resolvedRefs, r.resolvedRefsReason, r.resolvedRefsMessage, generation),
	}
}

func condition(conditionType string, ok bool, reason, message string, generation int64) metav1.Condition {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	if message == "" {
		message = reason
	}

	// LastTransitionTime is left to carryTransitionTimes, which knows the
	// previous status.
	return metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	}
}

func groupOf(group string) *infv1.Group {
	g := infv1.Group(group)
	return &g
}
