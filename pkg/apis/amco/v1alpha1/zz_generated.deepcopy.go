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
	if in.ConfigSelector != nil {
		out.ConfigSelector = new(metav1.LabelSelector)
		in.ConfigSelector.DeepCopyInto(out.ConfigSelector)
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

// DeepCopyInto copies the receiver into out.
func (in *AIXMachineConfig) DeepCopyInto(out *AIXMachineConfig) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
}

// DeepCopy returns a deep copy of the receiver.
func (in *AIXMachineConfig) DeepCopy() *AIXMachineConfig {
	if in == nil {
		return nil
	}
	out := new(AIXMachineConfig)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *AIXMachineConfig) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the receiver into out.
func (in *AIXMachineConfigSpec) DeepCopyInto(out *AIXMachineConfigSpec) {
	*out = *in
	if in.Files != nil {
		out.Files = make([]FileSpec, len(in.Files))
		for i := range in.Files {
			in.Files[i].DeepCopyInto(&out.Files[i])
		}
	}
	if in.Subsystems != nil {
		out.Subsystems = make([]SubsystemSpec, len(in.Subsystems))
		for i := range in.Subsystems {
			in.Subsystems[i].DeepCopyInto(&out.Subsystems[i])
		}
	}
	if in.Inittab != nil {
		out.Inittab = make([]InittabSpec, len(in.Inittab))
		copy(out.Inittab, in.Inittab)
	}
	if in.Tunables != nil {
		out.Tunables = make([]TunableSpec, len(in.Tunables))
		copy(out.Tunables, in.Tunables)
	}
	if in.DeviceAttributes != nil {
		out.DeviceAttributes = make([]DeviceAttributeSpec, len(in.DeviceAttributes))
		copy(out.DeviceAttributes, in.DeviceAttributes)
	}
	if in.KernelUpdate != nil {
		out.KernelUpdate = new(runtime.RawExtension)
		in.KernelUpdate.DeepCopyInto(out.KernelUpdate)
	}
}

// DeepCopyInto copies the receiver into out.
func (in *FileSpec) DeepCopyInto(out *FileSpec) {
	*out = *in
	in.Contents.DeepCopyInto(&out.Contents)
	if in.Mode != nil {
		out.Mode = new(int32)
		*out.Mode = *in.Mode
	}
	if in.OnChange != nil {
		out.OnChange = make([]SubsystemActionSpec, len(in.OnChange))
		copy(out.OnChange, in.OnChange)
	}
}

// DeepCopyInto copies the receiver into out.
func (in *FileContents) DeepCopyInto(out *FileContents) {
	*out = *in
	if in.Inline != nil {
		out.Inline = new(string)
		*out.Inline = *in.Inline
	}
	if in.Base64 != nil {
		out.Base64 = new(string)
		*out.Base64 = *in.Base64
	}
}

// DeepCopyInto copies the receiver into out.
func (in *SubsystemSpec) DeepCopyInto(out *SubsystemSpec) {
	*out = *in
	if in.Definition != nil {
		out.Definition = new(SubsystemDefinitionSpec)
		in.Definition.DeepCopyInto(out.Definition)
	}
}

// DeepCopyInto copies the receiver into out.
func (in *SubsystemDefinitionSpec) DeepCopyInto(out *SubsystemDefinitionSpec) {
	*out = *in
	if in.UID != nil {
		out.UID = new(int32)
		*out.UID = *in.UID
	}
}

// DeepCopyInto copies the receiver into out.
func (in *AIXMachineConfigList) DeepCopyInto(out *AIXMachineConfigList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]AIXMachineConfig, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy returns a deep copy of the receiver.
func (in *AIXMachineConfigList) DeepCopy() *AIXMachineConfigList {
	if in == nil {
		return nil
	}
	out := new(AIXMachineConfigList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *AIXMachineConfigList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
