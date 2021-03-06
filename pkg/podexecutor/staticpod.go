package podexecutor

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	v1 "k8s.io/api/core/v1"

	"github.com/rancher/k3s/pkg/daemons/executor"
	"github.com/rancher/rke2/pkg/auth"
	"github.com/rancher/rke2/pkg/images"
	"github.com/rancher/rke2/pkg/staticpod"
	"github.com/sirupsen/logrus"
	"k8s.io/apiserver/pkg/authentication/authenticator"
)

var (
	ssldirs = []string{
		"/etc/ssl/certs",
		"/etc/ca-certificates",
		"/usr/local/share/ca-certificates",
		"/usr/share/ca-certificates",
	}
)

type StaticPod struct {
	Manifests  string
	PullImages string
	Images     images.Images
}

func (s *StaticPod) Kubelet(args []string) error {
	go func() {
		for {
			cmd := exec.Command("kubelet", args...)
			cmd.Stdout = os.Stdout
			//cmd.Stderr = os.Stderr
			addDeathSig(cmd)

			err := cmd.Run()
			logrus.Errorf("Kubelet exited: %v", err)

			time.Sleep(5 * time.Second)
		}
	}()

	return nil
}

func (s *StaticPod) KubeProxy(args []string) error {
	panic("kube-proxy unsupported")
}

func (s *StaticPod) APIServer(ctx context.Context, etcdReady <-chan struct{}, args []string) (authenticator.Request, http.Handler, error) {
	if err := images.Pull(s.PullImages, "kube-apiserver", s.Images.KubeAPIServer); err != nil {
		return nil, nil, err
	}

	for i, arg := range args {
		// This is an option k3s adds that does not exist upstream
		if strings.HasPrefix(arg, "--advertise-port=") {
			args = append(args[:i], args[i+1:]...)
			break
		}
	}

	after(etcdReady, func() error {
		return staticpod.Run(s.Manifests, staticpod.Args{
			Command:   "kube-apiserver",
			Args:      args,
			Image:     s.Images.KubeAPIServer,
			Dirs:      ssldirs,
			CPUMillis: 250,
		})
	})

	auth, err := auth.FromArgs(args)
	return auth, http.NotFoundHandler(), err
}

func (s *StaticPod) Scheduler(apiReady <-chan struct{}, args []string) error {
	if err := images.Pull(s.PullImages, "kube-scheduler", s.Images.KubeScheduler); err != nil {
		return err
	}
	return after(apiReady, func() error {
		return staticpod.Run(s.Manifests, staticpod.Args{
			Command:     "kube-scheduler",
			Args:        args,
			Image:       s.Images.KubeScheduler,
			HealthPort:  10251,
			HealthProto: "HTTP",
			CPUMillis:   100,
		})
	})
}

func after(after <-chan struct{}, f func() error) error {
	go func() {
		<-after
		if err := f(); err != nil {
			logrus.Fatal(err)
		}
	}()
	return nil
}

func (s *StaticPod) ControllerManager(apiReady <-chan struct{}, args []string) error {
	if err := images.Pull(s.PullImages, "kube-controller-manager", s.Images.KubeControllManager); err != nil {
		return err
	}
	return after(apiReady, func() error {
		return staticpod.Run(s.Manifests, staticpod.Args{
			Command: "kube-controller-manager",
			Args: append(args,
				"/usr/libexec/kubernetes/kubelet-plugins/volume/exec"),
			Image:       s.Images.KubeControllManager,
			HealthPort:  10252,
			HealthProto: "HTTP",
			CPUMillis:   200,
		})
	})
}

func (s *StaticPod) CurrentETCDOptions() (opts executor.InitialOptions, err error) {
	bytes, err := ioutil.ReadFile(filepath.Join(s.Manifests, "etcd.yaml"))
	if os.IsNotExist(err) {
		return
	}

	pod := &v1.Pod{}
	if err := yaml.Unmarshal(bytes, pod); err != nil {
		return opts, err
	}

	v, ok := pod.Annotations["etcd.k3s.io/initial"]
	if ok {
		return opts, json.NewDecoder(strings.NewReader(v)).Decode(&opts)
	}

	return
}

func (s *StaticPod) ETCD(args executor.ETCDConfig) error {
	if err := images.Pull(s.PullImages, "etcd", s.Images.ETCD); err != nil {
		return err
	}

	initial, err := json.Marshal(args.InitialOptions)
	if err != nil {
		return err
	}

	confFile, err := args.ToConfigFile()
	if err != nil {
		return err
	}

	return staticpod.Run(s.Manifests, staticpod.Args{
		Annotations: map[string]string{
			"etcd.k3s.io/initial": string(initial),
		},
		Command: "etcd",
		Args: []string{
			"--config-file=" + confFile,
		},
		Image: s.Images.ETCD,
		Dirs:  []string{args.DataDir},
		Files: []string{
			args.ServerTrust.CertFile,
			args.ServerTrust.KeyFile,
			args.ServerTrust.TrustedCAFile,
			args.PeerTrust.CertFile,
			args.PeerTrust.KeyFile,
			args.PeerTrust.TrustedCAFile,
		},
		HealthPort:  2381,
		HealthPath:  "/health",
		HealthProto: "HTTP",
	})
}
