package probe

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

const DefaultMetricsURL = "http://kube-state-metrics.kube-state-metrics.svc:8080/metrics"
const maxMetricsBytes = 32 << 20

func kubeStateMetrics(ctx context.Context, httpClient *http.Client, endpoint string, pod podIdentity, interval time.Duration) error {
	return poll(ctx, interval, func() (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return true, err
		}
		req.Header.Set("Accept", "text/plain; version=0.0.4")
		response, err := httpClient.Do(req)
		if err != nil {
			return false, fmt.Errorf("scrape kube-state-metrics: %w", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return false, fmt.Errorf("kube-state-metrics returned HTTP %d", response.StatusCode)
		}
		err = verifyPodMetrics(response.Body, pod)
		return err == nil, err
	})
}

func verifyPodMetrics(body io.Reader, pod podIdentity) error {
	limited := &io.LimitedReader{R: body, N: maxMetricsBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	found := map[string]bool{}
	for scanner.Scan() {
		line := scanner.Text()
		for _, name := range []string{"kube_pod_info", "kube_pod_status_phase"} {
			// Pod names are DNS labels, so the cheap filter cannot contain escapes.
			// Parse matching samples to compare exact labels and numeric values.
			if !strings.HasPrefix(line, name+"{") || !strings.Contains(line, `pod="`+pod.name+`"`) {
				continue
			}
			parser := expfmt.NewTextParser(model.LegacyValidation)
			families, err := parser.TextToMetricFamilies(strings.NewReader("# TYPE " + name + " gauge\n" + line + "\n"))
			if err != nil {
				return fmt.Errorf("invalid %s metric: %w", name, err)
			}
			for _, metric := range families[name].GetMetric() {
				labels := map[string]string{}
				for _, label := range metric.GetLabel() {
					labels[label.GetName()] = label.GetValue()
				}
				if metric.GetGauge().GetValue() == 1 && labels["namespace"] == pod.namespace && labels["pod"] == pod.name && labels["uid"] == pod.uid && (name != "kube_pod_status_phase" || labels["phase"] == "Running") {
					found[name] = true
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read metrics: %w", err)
	}
	if limited.N == 0 {
		return fmt.Errorf("metrics response exceeds %d bytes", maxMetricsBytes)
	}
	if !found["kube_pod_info"] || !found["kube_pod_status_phase"] {
		return fmt.Errorf("waiting for kube_pod_info=1 and Running kube_pod_status_phase=1 for %s/%s UID %s", pod.namespace, pod.name, pod.uid)
	}
	return nil
}
