// SPDX-License-Identifier: AGPL-3.0-only

package k8sconfigconnector

import (
	gcpcomputev1alpha1 "github.com/GoogleCloudPlatform/k8s-config-connector/pkg/clients/generated/apis/k8s/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// IsStatusConditionTrue returns true when the conditionType is present and set to `gcpcomputev1alpha1.ConditionTrue`
func IsStatusConditionTrue(conditions []gcpcomputev1alpha1.Condition, conditionType string) bool {
	return IsStatusConditionPresentAndEqual(conditions, conditionType, corev1.ConditionTrue)
}

// IsStatusConditionPresentAndEqual returns true when conditionType is present and equal to status.
func IsStatusConditionPresentAndEqual(conditions []gcpcomputev1alpha1.Condition, conditionType string, status corev1.ConditionStatus) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Status == status
		}
	}
	return false
}
