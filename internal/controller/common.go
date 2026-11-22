// Package controller holds the reconcilers, one per kind.
package controller

import (
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// Finalizer keeps an object until its cloud resources are cleaned up.
	Finalizer = "cloud.paguos.dev/finalizer"
	// KeyIDAnnotation on a credentials Secret names the key it holds.
	KeyIDAnnotation = "cloud.paguos.dev/key-id"
	// ManagedByLabel marks objects created by tender. The manager only caches
	// Secrets and ConfigMaps with this label.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "tender"

	ConditionReady = "Ready"
)

// reconcileError carries the Ready condition's reason.
type reconcileError struct {
	reason string
	err    error
}

func (e *reconcileError) Error() string { return e.err.Error() }
func (e *reconcileError) Unwrap() error { return e.err }

func fail(reason string, format string, args ...any) error {
	return &reconcileError{reason: reason, err: fmt.Errorf(format, args...)}
}

func reason(err error) string {
	var re *reconcileError
	if errors.As(err, &re) {
		return re.reason
	}
	return "Error"
}

func setReady(conds *[]metav1.Condition, generation int64, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(conds, metav1.Condition{
		Type:               ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: generation,
	})
}

func managedLabels() map[string]string {
	return map[string]string{ManagedByLabel: ManagedByValue}
}
