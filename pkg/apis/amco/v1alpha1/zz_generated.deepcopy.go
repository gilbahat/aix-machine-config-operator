// Code written in the shape deepcopy-gen produces; regenerate with
// controller-gen object paths=./pkg/apis/... if the types change.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// DeepCopyInto copies the receiver into out.
func (in *AIXConfigPool) DeepCopyInto(out *AIXConfigPool) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy returns a deep copy of the receiver.
func (in *AIXConfigPool) DeepCopy() *AIXConfigPool {
	if in == nil {
		return nil
	}
	out := new(AIXConfigPool)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *AIXConfigPool) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the receiver into out.
func (in *AIXConfigPoolSpec) DeepCopyInto(out *AIXConfigPoolSpec) {
	*out = *in
	if in.NodeSelector != nil {
		out.NodeSelector = new(metav1.LabelSelector)
		in.NodeSelector.DeepCopyInto(out.NodeSelector)
	}
	if in.MaxUnavailable != nil {
		out.MaxUnavailable = new(intstr.IntOrString)
		*out.MaxUnavailable = *in.MaxUnavailable
	}
}

// DeepCopyInto copies the receiver into out.
func (in *AIXConfigPoolStatus) DeepCopyInto(out *AIXConfigPoolStatus) {
	*out = *in
	if in.Warnings != nil {
		out.Warnings = make([]string, len(in.Warnings))
		copy(out.Warnings, in.Warnings)
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

// DeepCopyInto copies the receiver into out.
func (in *AIXConfigPoolList) DeepCopyInto(out *AIXConfigPoolList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]AIXConfigPool, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy returns a deep copy of the receiver.
func (in *AIXConfigPoolList) DeepCopy() *AIXConfigPoolList {
	if in == nil {
		return nil
	}
	out := new(AIXConfigPoolList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *AIXConfigPoolList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopy returns a deep copy of the receiver.
func (in *AIXConfigPoolSpec) DeepCopy() *AIXConfigPoolSpec {
	if in == nil {
		return nil
	}
	out := new(AIXConfigPoolSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopy returns a deep copy of the receiver.
func (in *AIXConfigPoolStatus) DeepCopy() *AIXConfigPoolStatus {
	if in == nil {
		return nil
	}
	out := new(AIXConfigPoolStatus)
	in.DeepCopyInto(out)
	return out
}
