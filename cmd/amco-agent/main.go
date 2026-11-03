// Command amco-agent is the AMCO node agent. On an AIX node it runs as the
// SRC subsystem "amco-agent" (see hack/aix/install-agent.sh), applies the
// rendered configuration the operator assigns to the node and reports
// progress through node annotations.
//
//	amco-agent run     watch this node and apply its desired config (the SRC daemon)
//	amco-agent apply   apply a rendered config file locally, without a cluster (testing)
//	amco-agent version print the version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/gilbahat/aix-machine-config-operator/pkg/agent"
	"github.com/gilbahat/aix-machine-config-operator/pkg/aix"
	"github.com/gilbahat/aix-machine-config-operator/pkg/nodestate"
	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
	"github.com/gilbahat/aix-machine-config-operator/pkg/version"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage: amco-agent run|apply|version [flags]

  run      watch this node and apply its desired config (run by SRC)
  apply    apply a rendered config JSON file without a cluster (testing)
  version  print the version

Run "amco-agent <command> -h" for the command's flags.`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = run(os.Args[2:])
	case "apply":
		err = apply(os.Args[2:])
	case "version":
		fmt.Println(version.Version)
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		klog.Error(err)
		klog.Flush()
		os.Exit(1)
	}
	klog.Flush()
}

type commonFlags struct {
	root      string
	noRestart bool
}

func newFlagSet(name string, c *commonFlags) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	klog.InitFlags(fs)
	fs.StringVar(&c.root, "root", "/", "host root to write files under (tests and dry runs)")
	fs.BoolVar(&c.noRestart, "no-restart", false, "do not restart node services after writing files")
	return fs
}

// restarter returns the SRC-backed restarter, or nil when restarts are off.
func (c *commonFlags) restarter() (agent.Restarter, error) {
	if c.noRestart {
		return nil, nil
	}
	if runtime.GOOS != "aix" {
		return nil, fmt.Errorf("restarting services needs the AIX SRC; use --no-restart on %s", runtime.GOOS)
	}
	return srcRestarter{aix.NewSRC(aix.ExecRunner{})}, nil
}

type srcRestarter struct{ src *aix.SRC }

func (s srcRestarter) Restart(ctx context.Context, name string) error {
	err := s.src.Restart(ctx, name)
	if errors.Is(err, aix.ErrSubsystemNotFound) {
		return fmt.Errorf("%w: %v", agent.ErrServiceNotDefined, err)
	}
	return err
}

func run(args []string) error {
	var c commonFlags
	fs := newFlagSet("run", &c)
	host, _ := os.Hostname()
	kubeconfig := fs.String("kubeconfig", "/etc/amco/kubeconfig", "kubeconfig of the amco-agent service account")
	nodeName := fs.String("node-name", strings.ToLower(strings.SplitN(host, ".", 2)[0]), "name of this node (the kubelet's --hostname-override, if any)")
	namespace := fs.String("namespace", "aix-machine-config-operator", "namespace of the rendered configs")
	interval := fs.Duration("interval", time.Minute, "how often to check for drift and retry failed applies")
	_ = fs.Parse(args)

	restarter, err := c.restarter()
	if err != nil {
		return err
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		return fmt.Errorf("loading %s: %w", *kubeconfig, err)
	}
	cfg.UserAgent = "amco-agent/" + version.Version
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	info := map[string]string{nodestate.AgentVersionAnnotation: version.Version}
	if runtime.GOOS == "aix" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		lvl, err := aix.GetOSLevel(ctx, aix.ExecRunner{})
		cancel()
		if err != nil {
			return err
		}
		if err := lvl.CheckSupported(); err != nil {
			return err
		}
		info[nodestate.OSLevelAnnotation] = lvl.Raw
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	klog.Infof("amco-agent %s starting for node %s", version.Version, *nodeName)
	a := &agent.Agent{
		Host:      agent.Host{Root: c.root},
		Kube:      kube,
		Restarter: restarter,
		NodeName:  *nodeName,
		Namespace: *namespace,
		Interval:  *interval,
		Info:      info,
	}
	return a.Run(ctx)
}

func apply(args []string) error {
	var c commonFlags
	fs := newFlagSet("apply", &c)
	file := fs.String("f", "", "rendered config JSON (output of aix-machine-config-operator render)")
	_ = fs.Parse(args)
	if *file == "" {
		return fmt.Errorf("apply: -f is required")
	}
	data, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	cfg, err := rendered.Unmarshal(data)
	if err != nil {
		return err
	}
	name, err := cfg.Name()
	if err != nil {
		return err
	}
	restarter, err := c.restarter()
	if err != nil {
		return err
	}
	host := agent.Host{Root: c.root}
	applied, err := host.LoadApplied()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := agent.ApplyConfig(ctx, host, restarter, name, cfg, applied); err != nil {
		return err
	}
	klog.Infof("applied %s under %s", name, c.root)
	return nil
}
