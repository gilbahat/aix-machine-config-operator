// Command aix-machine-config-operator runs the AIX Machine Config Operator
// (AMCO) in an OpenShift cluster, or renders an AIX node configuration offline
// from YAML manifests for testing.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	configclient "github.com/openshift/client-go/config/clientset/versioned"
	mcfgclient "github.com/openshift/client-go/machineconfiguration/clientset/versioned"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"

	"github.com/gilbahat/aix-machine-config-operator/pkg/client"
	"github.com/gilbahat/aix-machine-config-operator/pkg/operator"
	"github.com/gilbahat/aix-machine-config-operator/pkg/version"
)

func main() {
	root := &cobra.Command{
		Use:          "aix-machine-config-operator",
		Short:        "Manages node configuration of AIX nodes in an OpenShift cluster",
		SilenceUsage: true,
	}
	fs := flag.NewFlagSet("klog", flag.ExitOnError)
	klog.InitFlags(fs)
	root.PersistentFlags().AddGoFlagSet(fs)
	root.AddCommand(startCmd(), renderCmd(), &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run:   func(*cobra.Command, []string) { fmt.Println(version.Version) },
	})
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func startCmd() *cobra.Command {
	cfg := operator.DefaultConfig()
	var kubeconfig string
	leaderElect := true
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		cfg.Namespace = ns
	}
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Run the operator",
		RunE: func(cmd *cobra.Command, _ []string) error {
			restCfg, err := loadConfig(kubeconfig)
			if err != nil {
				return err
			}
			kube, err := kubernetes.NewForConfig(restCfg)
			if err != nil {
				return err
			}
			cfgClient, err := configclient.NewForConfig(restCfg)
			if err != nil {
				return err
			}
			mcClient, err := mcfgclient.NewForConfig(restCfg)
			if err != nil {
				return err
			}
			pools, err := client.NewPools(restCfg)
			if err != nil {
				return err
			}
			machineConfigs, err := client.NewMachineConfigs(restCfg)
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			klog.Infof("aix-machine-config-operator %s, namespace %s", version.Version, cfg.Namespace)
			run := func(ctx context.Context) {
				if err := operator.New(cfg, kube, cfgClient, mcClient, pools, machineConfigs).Run(ctx); err != nil {
					klog.Fatal(err)
				}
			}
			if !leaderElect {
				run(ctx)
				return nil
			}
			id, err := os.Hostname()
			if err != nil {
				return err
			}
			lock, err := resourcelock.New(resourcelock.LeasesResourceLock, cfg.Namespace, "aix-machine-config-operator",
				kube.CoreV1(), kube.CoordinationV1(), resourcelock.ResourceLockConfig{Identity: id})
			if err != nil {
				return err
			}
			leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
				Lock:            lock,
				ReleaseOnCancel: true,
				LeaseDuration:   137 * time.Second, // OpenShift's recommended values
				RenewDeadline:   107 * time.Second,
				RetryPeriod:     26 * time.Second,
				Callbacks: leaderelection.LeaderCallbacks{
					OnStartedLeading: run,
					OnStoppedLeading: func() {
						if ctx.Err() == nil {
							klog.Fatal("lost leader election")
						}
					},
				},
			})
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig file (default: in-cluster)")
	f.StringVar(&cfg.Namespace, "namespace", cfg.Namespace, "namespace for rendered configs and the leader lease (default: $POD_NAMESPACE)")
	f.DurationVar(&cfg.DrainTimeout, "drain-timeout", cfg.DrainTimeout, "timeout of one drain attempt")
	f.DurationVar(&cfg.DrainRetryInterval, "drain-retry-interval", cfg.DrainRetryInterval, "wait before retrying a failed drain")
	f.IntVar(&cfg.KeepRendered, "keep-rendered", cfg.KeepRendered, "unreferenced rendered configs kept per pool")
	f.BoolVar(&leaderElect, "leader-elect", leaderElect, "use leader election")
	return cmd
}

func loadConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}
