package probe

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/tkhq/infra-smoketest/internal/definition"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type podIdentity struct {
	namespace, name, uid string
}

func currentPod() (podIdentity, error) {
	pod := podIdentity{os.Getenv("SMOKETEST_POD_NAMESPACE"), os.Getenv("SMOKETEST_POD_NAME"), os.Getenv("SMOKETEST_POD_UID")}
	if pod.namespace != definition.Namespace || pod.name == "" || pod.uid == "" {
		return pod, fmt.Errorf("missing or invalid probe Pod identity")
	}
	return pod, nil
}

func inClusterClient() (client.Client, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	cfg.Timeout = 15 * time.Second
	return client.New(cfg, client.Options{Scheme: scheme.Scheme})
}

// Retry observations until the probe deadline, retaining the last diagnostic.
// Creation and permission errors are returned by callers without retrying.
func poll(ctx context.Context, interval time.Duration, check func() (bool, error)) error {
	var last error
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w; last observation: %v", err, last)
		}
		done, err := check()
		if done {
			return err
		}
		last = err
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%w; last observation: %v", ctx.Err(), last)
		case <-timer.C:
		}
	}
}
