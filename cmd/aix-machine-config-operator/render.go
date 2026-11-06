package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	configv1 "github.com/openshift/api/config/v1"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	amcov1alpha1 "github.com/gilbahat/aix-machine-config-operator/pkg/apis/amco/v1alpha1"
	"github.com/gilbahat/aix-machine-config-operator/pkg/render"
)

// renderCmd renders a pool's configuration from manifests on disk. It is the
// offline half of the test workflow: render here, then apply the result on a
// scratch AIX LPAR with `amco-agent apply`.
func renderCmd() *cobra.Command {
	var (
		dir, out, poolName string
	)
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Render an AIX node configuration from YAML manifests (no cluster needed)",
		Long: `Reads every *.yaml/*.yml/*.json file under --dir. Recognised kinds are
ControllerConfig, Image, APIServer, ImageDigestMirrorSet, ImageTagMirrorSet,
ContainerRuntimeConfig, KubeletConfig, AIXMachineConfig and AIXConfigPool;
anything else is skipped. A ControllerConfig is required (oc get controllerconfig
machine-config-controller -o yaml). If no AIXConfigPool named --pool is
found, one is assumed with the label pools.amco.reuna-labs.io/<pool>: ""
that selects AIXMachineConfigs labelled amco.reuna-labs.io/role: <pool>.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in, err := loadInputs(dir, poolName)
			if err != nil {
				return err
			}
			res, err := render.Render(in)
			if err != nil {
				return err
			}
			for _, w := range res.Warnings {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
			}
			name, err := res.Config.Name()
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "rendered", name)
			data, err := res.Config.Marshal()
			if err != nil {
				return err
			}
			var pretty bytes.Buffer
			if err := jsonIndent(&pretty, data); err != nil {
				return err
			}
			if out == "" || out == "-" {
				_, err = cmd.OutOrStdout().Write(pretty.Bytes())
				return err
			}
			return os.WriteFile(out, pretty.Bytes(), 0o644)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "directory of input manifests")
	cmd.Flags().StringVarP(&out, "output", "o", "-", "file to write the rendered config JSON to")
	cmd.Flags().StringVar(&poolName, "pool", "aix", "AIXConfigPool to render")
	return cmd
}

var scheme = runtime.NewScheme()

func init() {
	for _, add := range []func(*runtime.Scheme) error{configv1.Install, mcfgv1.Install, amcov1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			panic(err)
		}
	}
}

func loadInputs(dir, poolName string) (render.Inputs, error) {
	var in render.Inputs
	decoder := serializer.NewCodecFactory(scheme).UniversalDeserializer()
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".yaml", ".yml", ".json":
			if !d.IsDir() {
				paths = append(paths, p)
			}
		}
		return nil
	})
	if err != nil {
		return in, err
	}
	sort.Strings(paths)
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return in, err
		}
		docs := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
		for {
			var raw runtime.RawExtension
			if err := docs.Decode(&raw); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				f.Close()
				return in, fmt.Errorf("%s: %w", p, err)
			}
			if len(bytes.TrimSpace(raw.Raw)) == 0 {
				continue
			}
			obj, _, err := decoder.Decode(raw.Raw, nil, nil)
			if err != nil {
				if runtime.IsNotRegisteredError(err) || runtime.IsMissingKind(err) {
					continue
				}
				f.Close()
				return in, fmt.Errorf("%s: %w", p, err)
			}
			addInput(&in, obj, poolName)
		}
		f.Close()
	}
	if in.Pool == nil {
		in.Pool = &amcov1alpha1.AIXConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName, Labels: map[string]string{amcov1alpha1.PoolLabelPrefix + poolName: ""}},
			Spec: amcov1alpha1.AIXConfigPoolSpec{ConfigSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{amcov1alpha1.RoleLabel: poolName},
			}},
		}
	}
	return in, nil
}

func addInput(in *render.Inputs, obj runtime.Object, poolName string) {
	switch o := obj.(type) {
	case *mcfgv1.ControllerConfig:
		in.ControllerConfig = o
	case *configv1.Image:
		in.Image = o
	case *configv1.APIServer:
		in.APIServer = o
	case *configv1.ImageDigestMirrorSet:
		in.ImageDigestMirrorSets = append(in.ImageDigestMirrorSets, o)
	case *configv1.ImageTagMirrorSet:
		in.ImageTagMirrorSets = append(in.ImageTagMirrorSets, o)
	case *mcfgv1.ContainerRuntimeConfig:
		in.ContainerRuntimeConfigs = append(in.ContainerRuntimeConfigs, o)
	case *mcfgv1.KubeletConfig:
		in.KubeletConfigs = append(in.KubeletConfigs, o)
	case *amcov1alpha1.AIXConfigPool:
		if o.Name == poolName {
			in.Pool = o
		}
	case *amcov1alpha1.AIXMachineConfig:
		in.MachineConfigs = append(in.MachineConfigs, o)
	}
}

func jsonIndent(dst *bytes.Buffer, data []byte) error {
	if err := json.Indent(dst, data, "", "  "); err != nil {
		return err
	}
	dst.WriteByte('\n')
	return nil
}
