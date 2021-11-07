package kls

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

type Supervisor struct {
	sync.Mutex
	cmdline   []string
	leaseName string
	LeaseTimings
	sigkillTimeout time.Duration
	kube           *kubernetes.Clientset

	cmd    *exec.Cmd
	cmdCtx context.Context
}

type LeaseTimings struct {
	Duration time.Duration
	Renew    time.Duration
	Retry    time.Duration
}

var DefaultLeaseTimings = LeaseTimings{
	Duration: 5 * time.Second,
	Renew:    3 * time.Second,
	Retry:    1 * time.Second,
}

type ConfigOption func(supervisor *Supervisor) error

func WithCmd(cmd []string) ConfigOption {
	return func(s *Supervisor) error {
		// TODO: Error if too short, or already set
		s.cmdline = cmd
		return nil
	}
}

func WithTimeout(t time.Duration) ConfigOption {
	return func(s *Supervisor) error {
		// TODO: Error if too short
		s.sigkillTimeout = t
		return nil
	}
}

func WithKubeClient(cs *kubernetes.Clientset) ConfigOption {
	return func(s *Supervisor) error {
		// TODO: Error if too short
		s.kube = cs
		return nil
	}
}

func WithLeaseTiming(lt LeaseTimings) ConfigOption {
	return func(s *Supervisor) error {
		// TODO: Error if timing is weird
		s.LeaseTimings = lt
		return nil
	}
}

func New(cmd []string, options ...ConfigOption) (*Supervisor, error) {
	s := &Supervisor{cmdline: cmd}
	for _, opt := range options {
		if err := opt(s); err != nil {
			return nil, fmt.Errorf("setting option: %w", err)
		}
	}

	if s.kube == nil {
		err := s.inClusterKubeconfig()
		if err != nil {
			return nil, fmt.Errorf("configuring kube client: %w", err)
		}
	}

	s.leaseName = "foobar"

	return s, nil
}

func (s *Supervisor) inClusterKubeconfig() error {
	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("getting in-cluster config: %w", err)
	}

	// creates the clientset
	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("cannot get in-cluster config: %w", err)
	}

	s.kube = cs
	return nil
}

func (s *Supervisor) startCmd(leaseCtx context.Context) {
	// Lock mutex, so we can ensure that stopCmd has a valid s.cmd.Process.
	s.Lock()
	defer s.Unlock()

	ctx, cancel := context.WithCancel(leaseCtx)
	s.cmdCtx = ctx

	// ReleaseOnCancel: true will make us release the lease when the command exits on its own.
	cmd := exec.Command(s.cmdline[0], s.cmdline[1:]...)

	// Inherit stdout and stderr
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Start()
	if err != nil {
		log.Errorf("starting command: %v", err)
		log.Warnf("cancelling context")
		cancel()
		return
	}

	s.cmd = cmd

	go func() {
		err := cmd.Wait()
		if err != nil {
			log.Errorf("command exited: %v", err)
		}
		log.Info("command finished cleanly, giving up on lease")
		cancel()
	}()
}

func (s *Supervisor) stopCmd() {
	s.Lock()
	defer s.Unlock()

	if s.cmd == nil || s.cmd.Process == nil {
		return
	}

	if err := syscall.Kill(s.cmd.Process.Pid, syscall.SIGTERM); err != nil {
		log.Errorf("cannot kill child command: %v", err)
		return
	}

	select {
	case <-s.cmdCtx.Done():
		return
	case <-time.After(s.sigkillTimeout):
		log.Warnf("command did not termiate within %d, sending sigkill", s.sigkillTimeout)
		_ = syscall.Kill(s.cmd.Process.Pid, syscall.SIGKILL)
	}
}

func (s *Supervisor) Run() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      "my-lock",
			Namespace: "default",
		},
		Client: s.kube.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: s.leaseName,
		},
	}

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   s.LeaseTimings.Duration,
		RenewDeadline:   s.LeaseTimings.Renew,
		RetryPeriod:     s.LeaseTimings.Retry,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				log.WithField("id", s.leaseName).Infof("got lease, starting %q", strings.Join(s.cmdline, " "))
				s.startCmd(ctx)
			},
			OnStoppedLeading: func() {
				log.WithField("id", s.leaseName).Infof("stopped leading, sending SIGTERM to %q", strings.Join(s.cmdline, " "))
				s.stopCmd()
			},
			OnNewLeader: func(identity string) {
				if identity == s.leaseName {
					return
				}
				log.WithField("id", s.leaseName).
					Info("new leader appointed")
			},
		},
	})
}
