package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/tkhq/infra-smoketest/api/v1alpha1"
)

var metricsPod = podIdentity{"infra-smoketest", "probe", "new-pod-uid"}

const podMetrics = `# HELP kube_pod_info Information about pod.
# TYPE kube_pod_info gauge
kube_pod_info{namespace="infra-smoketest",pod="probe",uid="new-pod-uid",node="worker"} 1
# TYPE kube_pod_status_phase gauge
kube_pod_status_phase{pod="probe",uid="new-pod-uid",namespace="infra-smoketest",phase="Pending"} 0
kube_pod_status_phase{phase="Running",uid="new-pod-uid",pod="probe",namespace="infra-smoketest"} 1
`

func TestPodMetricsRequireFreshRunningPod(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"valid", podMetrics, true},
		{"empty success", "", false},
		{"health response", "ok\n", false},
		{"stale uid", strings.ReplaceAll(podMetrics, "new-pod-uid", "old-pod-uid"), false},
		{"other pod", strings.ReplaceAll(podMetrics, `pod="probe"`, `pod="other"`), false},
		{"other namespace", strings.ReplaceAll(podMetrics, "infra-smoketest", "default"), false},
		{"zero values", strings.ReplaceAll(podMetrics, " 1\n", " 0\n"), false},
		{"not running", strings.ReplaceAll(podMetrics, "Running", "Succeeded"), false},
		{"info missing", strings.ReplaceAll(podMetrics, "kube_pod_info", "other_info"), false},
		{"phase missing", strings.ReplaceAll(podMetrics, "kube_pod_status_phase", "other_phase"), false},
		{"bad number", strings.ReplaceAll(podMetrics, " 1\n", " invalid\n"), false},
		{"oversized", podMetrics + strings.Repeat("# x\n", maxMetricsBytes/4), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyPodMetrics(strings.NewReader(tc.body), metricsPod)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}

func TestMetricsRetryUntilObserved(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			fmt.Fprint(w, strings.ReplaceAll(podMetrics, "new-pod-uid", "old-pod-uid"))
		default:
			fmt.Fprint(w, podMetrics)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := kubeStateMetrics(ctx, server.Client(), server.URL, metricsPod, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 3 {
		t.Fatalf("expected three scrapes, got %d", requests.Load())
	}
}

func TestMetricsRunWritesCorrelatedResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, podMetrics)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "result.json")
	for name, value := range map[string]string{
		"SMOKETEST_RUN_UID": "run-uid", "SMOKETEST_STAGE": "scrape",
		"SMOKETEST_POD_NAME": metricsPod.name, "SMOKETEST_POD_NAMESPACE": metricsPod.namespace,
		"SMOKETEST_POD_UID": metricsPod.uid, "KUBE_STATE_METRICS_URL": server.URL,
		"SMOKETEST_RESULT_PATH": path,
	} {
		t.Setenv(name, value)
	}
	if err := Run(context.Background(), "kube-state-metrics"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result api.ProbeResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.Version != 1 || result.RunUID != "run-uid" || result.Stage != "scrape" || result.Container != "probe" || result.MetricsPodUID != metricsPod.uid {
		t.Fatalf("invalid result: %+v", result)
	}
	t.Setenv("SMOKETEST_POD_UID", "")
	if err := Run(context.Background(), "kube-state-metrics"); err == nil {
		t.Fatal("accepted missing Pod identity")
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	result = api.ProbeResult{}
	if err := json.Unmarshal(data, &result); err != nil || result.Success || result.Error == "" {
		t.Fatalf("failure did not produce structured evidence: %s (%v)", data, err)
	}
}

func TestMetricsTimeoutKeepsDiagnostic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	defer cancel()
	ctx, deadlineCancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer deadlineCancel()
	err := kubeStateMetrics(ctx, server.Client(), server.URL, metricsPod, time.Second)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("missing timeout/diagnostic: %v", err)
	}
}
