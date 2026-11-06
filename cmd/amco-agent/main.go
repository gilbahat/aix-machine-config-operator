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
	fs.BoolVar(&c.noRestart, "no-restart", false, "do not restart or refresh subsystems after changes (required off AIX, where only files are applied)")
	return fs
}

// engine builds the item engine. On AIX it runs commands through the
// logging ExecRunner and reads the boot time; elsewhere (dry runs) only files
// can be applied, under --root, and subsystem actions are skipped.
func (c *commonFlags) engine(ctx context.Context) (*agent.Engine, error) {
	e := &agent.Engine{Files: agent.Host{Root: c.root}, NoRestart: c.noRestart}
	if runtime.GOOS != "aix" {
		if !c.noRestart {
			return nil, fmt.Errorf("running AIX commands needs AIX; use --no-restart on %s for a dry run (only files are applied, without ownership)", runtime.GOOS)
		}
		e.DryRun = true
		return e, nil
	}
	e.Runner = timeoutRunner{aix.ExecRunner{}, 10 * time.Minute}
	boot, err := aix.BootTime(ctx, e.Runner, time.Now())
	if err != nil {
		return nil, err
	}
	e.BootTime = boot
	return e, nil
}

// timeoutRunner bounds every AIX command, so a hung command cannot stall the
// agent forever.
type timeoutRunner struct {
	r       aix.Runner
	timeout time.Duration
}

func (t timeoutRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	return t.r.Run(ctx, name, args...)
}

func (t timeoutRunner) RunWithInput(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	return t.r.RunWithInput(ctx, input, name, args...)
}

func run(args []string) error {
	var c commonFlags
	fs := newFlagSet("run", &c)
	// The kubelet registers the node as the lower-cased os.Hostname().
	host, _ := os.Hostname()
	kubeconfig := fs.String("kubeconfig", "/etc/amco/kubeconfig", "kubeconfig of the amco-agent service account")
	nodeName := fs.String("node-name", strings.ToLower(host), "name of this node (the kubelet's --hostname-override, if any)")
	namespace := fs.String("namespace", "aix-machine-config-operator", "namespace of the rendered configs")
	interval := fs.Duration("interval", time.Minute, "how often to check for drift and retry failed applies")
	_ = fs.Parse(args)

	engine, err := c.engine(context.Background())
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
		Engine:    engine,
		Kube:      kube,
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
	full := fs.Bool("full", false, "run every item's subsystem actions, not only those of changed items")
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
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	engine, err := c.engine(ctx)
	if err != nil {
		return err
	}
	if _, err := engine.ReconcileBoot(); err != nil {
		return err
	}
	res, err := engine.Apply(ctx, name, cfg, agent.ApplyOptions{Force: *full})
	if err != nil {
		return err
	}
	klog.Infof("applied %s under %s: changed %v, pending reboot %v, reprovision %v", name, c.root, res.Changed, res.PendingReboot, res.Reprovision)
	return nil
}
