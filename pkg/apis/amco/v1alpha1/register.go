package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupName is the API group of AMCO's own resources. The same prefix is used
// for node labels and annotations (see pkg/nodestate).
const GroupName = "amco.reuna-labs.io"

// Resource is the plural resource name of AIXConfigPool.
const Resource = "aixconfigpools"

var (
	// SchemeGroupVersion is the group version used to register these objects.
	SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}
	// SchemeBuilder registers the types with a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	// AddToScheme adds the types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion, &AIXConfigPool{}, &AIXConfigPoolList{})
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}

// PoolLabelPrefix is the prefix of the label every AIXConfigPool should carry
// (pools.amco.reuna-labs.io/<pool name>: ""), so KubeletConfigs and
// ContainerRuntimeConfigs can select it with machineConfigPoolSelector, as
// pools.operator.machineconfiguration.openshift.io/<pool> does for MCO pools.
const PoolLabelPrefix = "pools." + GroupName + "/"
