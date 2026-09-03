package runner

import (
	"fmt"
	"time"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
)

// Environment abstracts a single isolated execution environment for one runner
// instance — the boundary that keeps gh-aw's machine-global resources (/tmp/gh-aw,
// fixed ports, fixed Docker/AWF names, $HOME state) from colliding between concurrent
// jobs on the same host.
//
// ContainerEnvironment (privileged Docker-in-Docker) is the only backend today. The
// interface is deliberately backend-agnostic so a future MicroVMEnvironment (a real
// fresh VM per runner, where gh-aw "just works" with zero shims) can be added without
// changing the Manager. Each Environment targets exactly ONE runner instance.
type Environment interface {
	// Provision creates the environment if it does not already exist (e.g. build the
	// image and create the container). It does not start it.
	Provision() error
	// Start starts the environment so its runner registers and begins listening.
	Start() error
	// AwaitHealthy blocks until the environment is ready to accept jobs, or returns an
	// error once timeout elapses. Readiness means: the environment is running, the
	// inner container engine is responsive, and the actions runner is registered.
	AwaitHealthy(timeout time.Duration) error
	// Reset returns the environment to a pristine per-job state (best-effort). On the
	// container backend this is normally handled automatically by the per-job runner
	// hooks; Reset provides an explicit out-of-band path (e.g. for recovery tooling).
	Reset() error
	// Destroy stops and removes the environment and its local state.
	Destroy() error
	// Kind returns the backend identifier (e.g. "container").
	Kind() string
}

// defaultContainerHealthTimeout bounds how long Start waits for a container runner to
// become ready before reporting a (non-fatal) warning.
const defaultContainerHealthTimeout = 90 * time.Second

// Time-injection seams for containerAwaitHealthy's polling loop. Tests swap
// these with a fake clock so the deadline-expiration branches can be exercised
// deterministically without sleeping in real time. Production callers always
// see the real clock. Do not call these from anywhere else — they exist solely
// to make the deadline / sleep pair in containerAwaitHealthy testable, and a
// future refactor that moves the polling loop to a different file should move
// the seams with it.
var (
	nowFn   = time.Now
	sleepFn = func(d time.Duration) { time.Sleep(d) }
)

// ContainerEnvironment is the privileged Docker-in-Docker backend: one gh-sr-<instance>
// container per runner instance, each with its own inner dockerd, network namespace,
// MCP gateway port, and /tmp/gh-aw.
type ContainerEnvironment struct {
	mgr           *Manager
	h             *host.Host
	rc            config.RunnerConfig
	instanceIndex int
	instance      string
}

// NewContainerEnvironment builds a ContainerEnvironment for a single instance.
func (m *Manager) NewContainerEnvironment(h *host.Host, rc config.RunnerConfig, instanceIndex int, instance string) *ContainerEnvironment {
	return &ContainerEnvironment{mgr: m, h: h, rc: rc, instanceIndex: instanceIndex, instance: instance}
}

// Kind identifies the backend.
func (e *ContainerEnvironment) Kind() string { return config.RunnerModeContainer }

// Provision builds the runner image (if missing) and creates this instance's container.
func (e *ContainerEnvironment) Provision() error {
	if e.h.OS != "linux" {
		return fmt.Errorf("runner_mode: container is only supported on Linux hosts")
	}
	if containerRunnerPresent(e.h, e.instance) {
		return nil
	}
	baseImage, imageTag, err := e.mgr.resolveContainerImageInputs(e.h)
	if err != nil {
		return err
	}
	if _, err := e.mgr.buildRunnerImageIfMissing(e.h, imageTag, baseImage, nil); err != nil {
		return err
	}
	return e.mgr.createContainerInstance(e.h, e.rc, e.instanceIndex, e.instance, imageTag)
}

// Start starts the runner container.
func (e *ContainerEnvironment) Start() error {
	return e.mgr.startContainer(e.h, e.instance)
}

// AwaitHealthy waits until the container is running, the inner dockerd responds, and
// the actions runner is registered inside it.
func (e *ContainerEnvironment) AwaitHealthy(timeout time.Duration) error {
	return containerAwaitHealthy(e.h, e.instance, timeout)
}

// Reset runs the per-job teardown inside the container out-of-band. Normally the runner
// job hooks do this automatically before/after each job; this is an explicit recovery path.
func (e *ContainerEnvironment) Reset() error {
	cname := containerName(e.instance)
	// job-completed.sh performs the deterministic teardown and always exits 0.
	_, err := e.h.Run(DockerExecCommand(cname, "/opt/gh-sr/hooks/job-completed.sh 2>/dev/null || true"))
	return err
}

// Destroy deregisters and removes the container and its state directory.
func (e *ContainerEnvironment) Destroy() error {
	return e.mgr.removeContainer(e.h, e.rc, e.instance)
}

// containerAwaitHealthy polls until the runner container is ready to accept jobs or the
// timeout elapses. Readiness gate: container running + inner dockerd responsive +
// actions runner registered (.runner present). The old baked-DNS host.docker.internal
// gate is gone: gh-aw ≥ v0.88 passes its own --add-host=host.docker.internal:host-gateway
// to the containers that need it, so there is nothing gh-sr-side to verify. Reuses the
// same signals as gh sr doctor.
func containerAwaitHealthy(h *host.Host, instanceName string, timeout time.Duration) error {
	cname := containerName(instanceName)
	deadline := nowFn().Add(timeout)
	lastErr := fmt.Errorf("container %s not ready", cname)

	for {
		rep, _ := ProbeDinDContainerReadiness(h, cname)
		switch {
		case IsContainerAcceptingJobs(rep.State):
			if !rep.InnerDockerdOK {
				lastErr = fmt.Errorf("inner dockerd not responding inside %s", cname)
			} else if !rep.Registered {
				lastErr = fmt.Errorf("actions runner not yet registered inside %s", cname)
			} else {
				return nil
			}
		case rep.State == "missing" || rep.State == "":
			lastErr = fmt.Errorf("container %s not found", cname)
		default:
			lastErr = fmt.Errorf("container %s state is %q", cname, rep.State)
		}
		if nowFn().After(deadline) {
			return lastErr
		}
		sleepFn(2 * time.Second)
	}
}
