// simplyblock-nodeprobe reports what one worker has to offer, and changes
// nothing.
//
// It is the node-side half of the discovery run that produces a
// ClusterDeploymentConfig: the operator creates one of these as a Job per
// worker, each pod collects its own machine's inventory, and each writes a
// report the operator then reads. See
// operator/docs/designs/crd-redesign/design-clusterdeploymentconfig.md §8.
//
// The whole of what it collects comes from atlas/inventory, and the whole of
// what it decides is nothing: every device is reported, refused ones included
// and each with the grounds it was refused on, because the run's device filter
// belongs to the operator that holds the fleet's reports and an administrator
// has to be able to be told why the disk they expected is not a candidate.
//
// It is read-only against the machine. Devices are opened O_RDONLY, the host's
// sysfs and procfs are mounted read-only, and the only thing this process
// writes anywhere is its own report.
//
//	simplyblock-nodeprobe --node=worker-3 --namespace=simplyblock --run=oops-20260908
//	    --sysfs-root=/host/sys --proc-root=/host/proc --dev-root=/dev
//	    --mountinfo=/host/proc/1/mountinfo
//
// With --output=stdout it writes the report to standard output and touches no
// cluster at all, which is how it is run by hand on a machine to see what a
// discovery run would make of it.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/simplyblock/atlas/inventory"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// outputTarget is where the report goes.
type outputTarget string

const (
	// outputConfigMap writes the report into a ConfigMap in the cluster, which
	// is what the Job does.
	outputConfigMap outputTarget = "configmap"

	// outputStdout writes it to standard output and reaches no cluster, which
	// is what a person running the probe by hand wants.
	outputStdout outputTarget = "stdout"
)

// options is everything the probe was told.
type options struct {
	node      string
	namespace string
	run       string
	output    outputTarget
	timeout   time.Duration

	roots inventory.Config

	kubeconfig string
	owner      *metav1.OwnerReference
}

func main() {
	opts, err := parseOptions(os.Args[1:], os.Getenv)
	if err != nil {
		fail(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()

	if err := run(ctx, opts); err != nil {
		fail(err)
	}
}

// fail ends the process with the reason on standard error.
//
// The Job's termination message policy falls back to the container's logs, so
// this is also what a kubectl describe of the failed pod shows.
func fail(err error) {
	fmt.Fprintf(os.Stderr, "simplyblock-nodeprobe: %v\n", err)
	os.Exit(1)
}

// run collects the inventory and writes the report.
//
// A collection that only partly succeeded is still written. The failures travel
// in the report's Unreadable list, so a worker whose CPU tree could not be read
// still contributes the disks it has, and the run records what was missing
// rather than losing the machine.
func run(ctx context.Context, opts options) error {
	inv, unreadable := inventory.Collect(ctx, opts.roots)
	report := nodeprobe.FromInventory(opts.node, time.Now(), inv, unreadable)

	if opts.output == outputStdout {
		encoded, err := nodeprobe.Encode(report)
		if err != nil {
			return err
		}
		fmt.Printf("%s\n", encoded)
		fmt.Fprintln(os.Stderr, nodeprobe.Summary(report))
		return nil
	}

	if err := writeConfigMap(ctx, opts, report); err != nil {
		return err
	}
	fmt.Println(nodeprobe.Summary(report))
	return nil
}

// writeConfigMap puts the report where the discovery step looks for it,
// replacing the one this probe wrote before if the Job restarted it.
func writeConfigMap(ctx context.Context, opts options, report nodeprobe.Report) error {
	cm, err := nodeprobe.ConfigMap(opts.namespace, opts.run, opts.owner, report)
	if err != nil {
		return err
	}

	client, err := clientFor(opts.kubeconfig)
	if err != nil {
		return err
	}
	configMaps := client.CoreV1().ConfigMaps(opts.namespace)

	_, err = configMaps.Create(ctx, cm, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("write the report for node %s: %w", report.Node, err)
	}

	// A Job that retried its pod has a report from the attempt that failed.
	// The name is derived from the run and the node, so this is that same
	// report and replacing it is right: two objects for one node would leave
	// the discovery step choosing between them.
	if _, err := configMaps.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("replace the report for node %s: %w", report.Node, err)
	}
	return nil
}

// clientFor builds the client the report is written with: the pod's own service
// account in a cluster, or a kubeconfig when the probe is run by hand.
func clientFor(kubeconfig string) (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if errors.Is(err, rest.ErrNotInCluster) {
		if kubeconfig == "" {
			return nil, fmt.Errorf(
				"not running in a cluster and no --kubeconfig given; " +
					"use --output=stdout to collect without writing anything")
		}
		if cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig); err != nil {
			return nil, fmt.Errorf("read the kubeconfig at %s: %w", kubeconfig, err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("read this pod's cluster configuration: %w", err)
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build a Kubernetes client: %w", err)
	}
	return client, nil
}

// parseOptions reads the flags, defaulting the ambient facts from the
// environment the Job sets.
//
// It takes the arguments and the environment rather than reading the globals,
// so that what the Job's command line produces is testable without a cluster.
func parseOptions(args []string, env func(string) string) (options, error) {
	fs := flag.NewFlagSet("simplyblock-nodeprobe", flag.ContinueOnError)

	var opts options
	var output string

	fs.StringVar(&opts.node, "node", env("NODE_NAME"),
		"the Kubernetes node being probed; defaults to NODE_NAME, which the Job takes from spec.nodeName")
	fs.StringVar(&opts.namespace, "namespace", env("POD_NAMESPACE"),
		"the namespace to write the report into; defaults to POD_NAMESPACE")
	fs.StringVar(&opts.run, "run", "",
		"the discovery run this probe belongs to, which names the report")
	fs.StringVar(&output, "output", string(outputConfigMap),
		"where the report goes: configmap or stdout")
	fs.DurationVar(&opts.timeout, "timeout", 2*time.Minute,
		"how long the whole collection may take before it is abandoned")
	fs.StringVar(&opts.roots.SysfsRoot, "sysfs-root", inventory.DefaultSysfsRoot,
		"the host's sysfs; the Job mounts it read-only at /host/sys")
	fs.StringVar(&opts.roots.ProcRoot, "proc-root", inventory.DefaultProcRoot,
		"the host's procfs; the Job mounts it read-only at /host/proc")
	fs.StringVar(&opts.roots.DevRoot, "dev-root", "/dev",
		"the host's device nodes, which is what the report's device paths name")
	fs.StringVar(&opts.roots.MountinfoPath, "mountinfo", "",
		"the mount table to read; a pod has to be pointed at the host's, which is "+
			"/host/proc/1/mountinfo, because its own lists none of the host's mounts")
	fs.StringVar(&opts.kubeconfig, "kubeconfig", "",
		"a kubeconfig to write the report with, for running the probe outside a cluster")

	if err := fs.Parse(args); err != nil {
		return options{}, err
	}

	opts.output = outputTarget(output)
	switch opts.output {
	case outputConfigMap, outputStdout:
	default:
		return options{}, fmt.Errorf("--output is %q, and it is configmap or stdout", output)
	}

	if opts.node == "" {
		return options{}, fmt.Errorf(
			"no node name: pass --node, or let the Job set NODE_NAME from spec.nodeName")
	}
	if opts.output == outputConfigMap {
		if opts.namespace == "" {
			return options{}, fmt.Errorf("no namespace to write the report into: pass --namespace")
		}
		if opts.run == "" {
			return options{}, fmt.Errorf("no run to attribute the report to: pass --run")
		}
		// A report going into the cluster is one a discovery run will act on,
		// and this is the flag whose absence makes such a report wrong rather
		// than incomplete: unset, the disk reading consults this process's own
		// mount table, which in a pod lists none of the host's mounts and so
		// reports the disk carrying the host's root filesystem as free. There
		// is no safe default to pick here, because only the caller knows where
		// it mounted the host's /proc, so the run is refused instead.
		if opts.roots.MountinfoPath == "" {
			return options{}, fmt.Errorf(
				"no mount table to read: pass --mountinfo, which in a pod is the host's at %s. "+
					"Without it this process reads its own mount namespace, which lists none of "+
					"the host's mounts, and every mounted host disk would be reported free",
				nodeprobe.HostMountinfoPath)
		}
	}
	if opts.timeout <= 0 {
		return options{}, fmt.Errorf("--timeout is %s, and a collection needs a positive one", opts.timeout)
	}

	opts.owner = ownerFrom(env)
	return opts, nil
}

// ownerFrom reads the owner the Job passed, so the report is collected when the
// run that asked for it is deleted.
//
// All four parts or none. A partial reference is one the API server rejects, and
// rejecting the whole report over it would lose a worker's inventory to a
// misconfiguration that costs nothing to ignore.
func ownerFrom(env func(string) string) *metav1.OwnerReference {
	apiVersion, kind := env("OWNER_API_VERSION"), env("OWNER_KIND")
	name, uid := env("OWNER_NAME"), env("OWNER_UID")
	if apiVersion == "" || kind == "" || name == "" || uid == "" {
		return nil
	}
	return &metav1.OwnerReference{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       name,
		UID:        types.UID(uid),
	}
}
