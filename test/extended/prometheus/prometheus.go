package prometheus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	v1 "k8s.io/api/core/v1"

	kapierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"

	clientset "k8s.io/client-go/kubernetes"
	watchtools "k8s.io/client-go/tools/watch"

	kapi "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/client/conditions"
	"k8s.io/kubernetes/test/e2e/framework"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"

	testresult "github.com/openshift/origin/pkg/test/ginkgo/result"
	"github.com/openshift/origin/test/extended/networking"
	exutil "github.com/openshift/origin/test/extended/util"
	"github.com/openshift/origin/test/extended/util/ibmcloud"
	helper "github.com/openshift/origin/test/extended/util/prometheus"
)

var _ = g.Describe("[sig-instrumentation][Late] Alerts", func() {
	defer g.GinkgoRecover()
	var (
		oc = exutil.NewCLIWithoutNamespace("prometheus")

		url, bearerToken string
	)
	g.BeforeEach(func() {
		var ok bool
		url, _, bearerToken, ok = helper.LocatePrometheus(oc)
		if !ok {
			e2e.Failf("Prometheus could not be located on this cluster, failing prometheus test")
		}
	})

	g.It("shouldn't report any alerts in firing or pending state apart from Watchdog and AlertmanagerReceiversNotConfigured and have no gaps in Watchdog firing", func() {
		if len(os.Getenv("TEST_UNSUPPORTED_ALLOW_VERSION_SKEW")) > 0 {
			e2eskipper.Skipf("Test is disabled to allow cluster components to have different versions, and skewed versions trigger multiple other alerts")
		}
		ns := oc.SetupNamespace()
		execPod := exutil.CreateExecPodOrFail(oc.AdminKubeClient(), ns, "execpod")
		defer func() {
			oc.AdminKubeClient().CoreV1().Pods(ns).Delete(context.Background(), execPod.Name, *metav1.NewDeleteOptions(1))
		}()

		firingAlertsWithBugs := helper.MetricConditions{
			{
				Selector: map[string]string{"alertname": "AggregatedAPIDown", "name": "v1alpha1.wardle.example.com"},
				Text:     "https://bugzilla.redhat.com/show_bug.cgi?id=1933144",
			},
		}

		pendingAlertsWithBugs := helper.MetricConditions{}
		allowedPendingAlerts := helper.MetricConditions{
			{
				Selector: map[string]string{"alertname": "HighOverallControlPlaneCPU"},
				Text:     "high CPU utilization during e2e runs is normal",
			},
		}

		knownViolations := sets.NewString()
		unexpectedViolations := sets.NewString()
		unexpectedViolationsAsFlakes := sets.NewString()
		debug := sets.NewString()

		// we only consider samples since the beginning of the test
		testDuration := exutil.DurationSinceStartInSeconds().String()

		// Invariant: The watchdog alert should be firing continuously during the whole test via the thanos
		// querier (which should have no gaps when it queries the individual stores). Allow zero or one changes
		// to the presence of this series (zero if data is preserved over test, one if data is lost over test).
		// This would not catch the alert stopping firing, but we catch that in other places and tests.
		watchdogQuery := fmt.Sprintf(`changes((max((ALERTS{alertstate="firing",alertname="Watchdog",severity="none"}) or (absent(ALERTS{alertstate="firing",alertname="Watchdog",severity="none"})*0)))[%s:1s]) > 1`, testDuration)
		result, err := helper.RunQuery(watchdogQuery, ns, execPod.Name, url, bearerToken)
		o.Expect(err).NotTo(o.HaveOccurred(), "unable to check watchdog alert over test window")
		if len(result.Data.Result) > 0 {
			unexpectedViolations.Insert("Watchdog alert had missing intervals during the run, which may be a sign of a Prometheus outage in violation of the prometheus query SLO of 100% uptime during normal execution")
		}

		// Invariant: No non-info level alerts should have fired during the test run
		firingAlertQuery := fmt.Sprintf(`
sort_desc(
count_over_time(ALERTS{alertstate="firing",severity!="info",alertname!~"Watchdog|AlertmanagerReceiversNotConfigured"}[%[1]s:1s])
) > 0
`, testDuration)
		result, err = helper.RunQuery(firingAlertQuery, ns, execPod.Name, url, bearerToken)
		o.Expect(err).NotTo(o.HaveOccurred(), "unable to check firing alerts during test")
		for _, series := range result.Data.Result {
			labels := helper.StripLabels(series.Metric, "alertname", "alertstate", "prometheus")
			violation := fmt.Sprintf("alert %s fired for %s seconds with labels: %s", series.Metric["alertname"], series.Value, helper.LabelsAsSelector(labels))
			if cause := firingAlertsWithBugs.Matches(series); cause != nil {
				knownViolations.Insert(fmt.Sprintf("%s (open bug: %s)", violation, cause.Text))
			} else {
				unexpectedViolations.Insert(violation)
			}
		}

		// Invariant: There should be no pending alerts after the test run
		pendingAlertQuery := `ALERTS{alertname!~"Watchdog|AlertmanagerReceiversNotConfigured",alertstate="pending",severity!="info"}`
		result, err = helper.RunQuery(pendingAlertQuery, ns, execPod.Name, url, bearerToken)
		o.Expect(err).NotTo(o.HaveOccurred(), "unable to retrieve pending alerts after upgrade")
		for _, series := range result.Data.Result {
			labels := helper.StripLabels(series.Metric, "alertname", "alertstate", "prometheus")
			violation := fmt.Sprintf("alert %s pending for %s seconds with labels: %s", series.Metric["alertname"], series.Value, helper.LabelsAsSelector(labels))
			if cause := allowedPendingAlerts.Matches(series); cause != nil {
				debug.Insert(fmt.Sprintf("%s (allowed: %s)", violation, cause.Text))
				continue
			}
			if cause := pendingAlertsWithBugs.Matches(series); cause != nil {
				knownViolations.Insert(fmt.Sprintf("%s (open bug: %s)", violation, cause.Text))
			} else {
				// treat pending errors as a flake right now because we are still trying to determine the scope
				// TODO: move this to unexpectedViolations later
				unexpectedViolationsAsFlakes.Insert(violation)
			}
		}

		if len(debug) > 0 {
			framework.Logf("Alerts were detected during test run which are allowed:\n\n%s", strings.Join(debug.List(), "\n"))
		}
		if len(unexpectedViolations) > 0 {
			framework.Failf("Unexpected alerts fired or pending after the test run:\n\n%s", strings.Join(unexpectedViolations.List(), "\n"))
		}
		if flakes := sets.NewString().Union(knownViolations).Union(unexpectedViolations).Union(unexpectedViolationsAsFlakes); len(flakes) > 0 {
			testresult.Flakef("Unexpected alert behavior during test:\n\n%s", strings.Join(flakes.List(), "\n"))
		}
		framework.Logf("No alerts fired during test run")
	})

	g.It("shouldn't exceed the 500 series limit of total series sent via telemetry from each cluster", func() {
		if !hasPullSecret(oc.AdminKubeClient(), "cloud.openshift.com") {
			e2eskipper.Skipf("Telemetry is disabled")
		}
		ns := oc.SetupNamespace()

		execPod := exutil.CreateExecPodOrFail(oc.AdminKubeClient(), ns, "execpod")
		defer func() {
			oc.AdminKubeClient().CoreV1().Pods(ns).Delete(context.Background(), execPod.Name, *metav1.NewDeleteOptions(1))
		}()

		// we only consider series sent since the beginning of the test
		testDuration := exutil.DurationSinceStartInSeconds().String()

		tests := map[string]bool{
			// We want to limit the number of total series sent, the cluster:telemetry_selected_series:count
			// rule contains the count of the all the series that are sent via telemetry. It is permissible
			// for some scenarios to generate more series than 600, we just want the basic state to be below
			// a threshold.
			fmt.Sprintf(`avg_over_time(cluster:telemetry_selected_series:count[%s]) >= 600`, testDuration):  false,
			fmt.Sprintf(`max_over_time(cluster:telemetry_selected_series:count[%s]) >= 1200`, testDuration): false,
		}
		err := helper.RunQueries(tests, oc, ns, execPod.Name, url, bearerToken)
		o.Expect(err).NotTo(o.HaveOccurred())

		e2e.Logf("Total number of series sent via telemetry is below the limit")
	})
})

var _ = g.Describe("[sig-instrumentation] Prometheus", func() {
	defer g.GinkgoRecover()
	var (
		oc = exutil.NewCLIWithoutNamespace("prometheus")

		url, prometheusURL, bearerToken string
	)

	g.BeforeEach(func() {
		var ok bool
		url, prometheusURL, bearerToken, ok = helper.LocatePrometheus(oc)
		if !ok {
			e2e.Failf("Prometheus could not be located on this cluster, failing prometheus test")
		}
	})

	g.Describe("when installed on the cluster", func() {
		g.It("should report telemetry if a cloud.openshift.com token is present [Late]", func() {
			if !hasPullSecret(oc.AdminKubeClient(), "cloud.openshift.com") {
				e2eskipper.Skipf("Telemetry is disabled")
			}
			ns := oc.SetupNamespace()

			execPod := exutil.CreateExecPodOrFail(oc.AdminKubeClient(), ns, "execpod")
			defer func() {
				oc.AdminKubeClient().CoreV1().Pods(ns).Delete(context.Background(), execPod.Name, *metav1.NewDeleteOptions(1))
			}()

			tests := map[string]bool{
				// Should have successfully sent at least some metrics to
				// remote write endpoint uncomment this once sending telemetry
				// via remote write is merged, and remove the other two checks.
				// `prometheus_remote_storage_succeeded_samples_total{job="prometheus-k8s"} >= 1`: true,

				// should have successfully sent at least once to remote
				`metricsclient_request_send{client="federate_to",job="telemeter-client",status_code="200"} >= 1`: true,
				// should have scraped some metrics from prometheus
				`federate_samples{job="telemeter-client"} >= 10`: true,
			}
			err := helper.RunQueries(tests, oc, ns, execPod.Name, url, bearerToken)
			o.Expect(err).NotTo(o.HaveOccurred())

			e2e.Logf("Telemetry is enabled: %s", bearerToken)
		})

		g.It("should start and expose a secured proxy and unsecured metrics", func() {
			ns := oc.SetupNamespace()
			execPod := exutil.CreateExecPodOrFail(oc.AdminKubeClient(), ns, "execpod")
			defer func() {
				oc.AdminKubeClient().CoreV1().Pods(ns).Delete(context.Background(), execPod.Name, *metav1.NewDeleteOptions(1))
			}()

			g.By("checking the prometheus metrics path")
			var metrics map[string]*dto.MetricFamily
			o.Expect(wait.PollImmediate(10*time.Second, 2*time.Minute, func() (bool, error) {
				results, err := getInsecureURLViaPod(ns, execPod.Name, fmt.Sprintf("%s/metrics", prometheusURL))
				if err != nil {
					e2e.Logf("unable to get metrics: %v", err)
					return false, nil
				}

				p := expfmt.TextParser{}
				metrics, err = p.TextToMetricFamilies(bytes.NewBufferString(results))
				o.Expect(err).NotTo(o.HaveOccurred())
				// original field in 2.0.0-beta
				counts := findCountersWithLabels(metrics["tsdb_samples_appended_total"], labels{})
				if len(counts) != 0 && counts[0] > 0 {
					return true, nil
				}
				// 2.0.0-rc.0
				counts = findCountersWithLabels(metrics["tsdb_head_samples_appended_total"], labels{})
				if len(counts) != 0 && counts[0] > 0 {
					return true, nil
				}
				// 2.0.0-rc.2
				counts = findCountersWithLabels(metrics["prometheus_tsdb_head_samples_appended_total"], labels{})
				if len(counts) != 0 && counts[0] > 0 {
					return true, nil
				}
				return false, nil
			})).NotTo(o.HaveOccurred(), fmt.Sprintf("Did not find tsdb_samples_appended_total, tsdb_head_samples_appended_total, or prometheus_tsdb_head_samples_appended_total"))

			g.By("verifying the oauth-proxy reports a 403 on the root URL")
			err := helper.ExpectURLStatusCodeExec(ns, execPod.Name, url, 403)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("verifying a service account token is able to authenticate")
			err = expectBearerTokenURLStatusCodeExec(ns, execPod.Name, fmt.Sprintf("%s/graph", url), bearerToken, 200)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("verifying a service account token is able to access the Prometheus API")
			// expect all endpoints within 60 seconds
			var lastErrs []error
			o.Expect(wait.PollImmediate(10*time.Second, 2*time.Minute, func() (bool, error) {
				contents, err := getBearerTokenURLViaPod(ns, execPod.Name, fmt.Sprintf("%s/api/v1/targets", prometheusURL), bearerToken)
				o.Expect(err).NotTo(o.HaveOccurred())

				targets := &prometheusTargets{}
				err = json.Unmarshal([]byte(contents), targets)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("verifying all expected jobs have a working target")

				// For IBM cloud clusters, skip control plane components and the CVO
				if e2e.TestContext.Provider != ibmcloud.ProviderName {
					lastErrs = all(
						// The OpenShift control plane
						targets.Expect(labels{"job": "api"}, "up", "^https://.*/metrics$"),
						targets.Expect(labels{"job": "controller-manager"}, "up", "^https://.*/metrics$"),

						// The kube control plane
						// TODO restore this after etcd operator lands
						//targets.Expect(labels{"job": "etcd"}, "up", "^https://.*/metrics$"),
						targets.Expect(labels{"job": "apiserver"}, "up", "^https://.*/metrics$"),
						targets.Expect(labels{"job": "kube-controller-manager"}, "up", "^https://.*/metrics$"),
						targets.Expect(labels{"job": "scheduler"}, "up", "^https://.*/metrics$"),
						targets.Expect(labels{"job": "kube-state-metrics"}, "up", "^https://.*/metrics$"),

						// Cluster version operator
						targets.Expect(labels{"job": "cluster-version-operator"}, "up", "^https://.*/metrics$"),
					)
				}

				lastErrs = append(lastErrs, all(
					targets.Expect(labels{"job": "prometheus-k8s", "namespace": "openshift-monitoring", "pod": "prometheus-k8s-0"}, "up", "^https://.*/metrics$"),
					targets.Expect(labels{"job": "kubelet"}, "up", "^https://.*/metrics$"),
					targets.Expect(labels{"job": "kubelet"}, "up", "^https://.*/metrics/cadvisor$"),
					targets.Expect(labels{"job": "node-exporter"}, "up", "^https://.*/metrics$"),
					targets.Expect(labels{"job": "prometheus-operator"}, "up", "^https://.*/metrics$"),
					targets.Expect(labels{"job": "alertmanager-main"}, "up", "^https://.*/metrics$"),
					targets.Expect(labels{"job": "crio"}, "up", "^http://.*/metrics$"),
				)...)
				if len(lastErrs) > 0 {
					e2e.Logf("missing some targets: %v", lastErrs)
					return false, nil
				}
				return true, nil
			})).NotTo(o.HaveOccurred(), "possibly some services didn't register ServiceMonitors to allow metrics collection")

			g.By("verifying all targets are exposing metrics over secure channel")
			var insecureTargets []error
			contents, err := getBearerTokenURLViaPod(ns, execPod.Name, fmt.Sprintf("%s/api/v1/targets", prometheusURL), bearerToken)
			o.Expect(err).NotTo(o.HaveOccurred())

			targets := &prometheusTargets{}
			err = json.Unmarshal([]byte(contents), targets)
			o.Expect(err).NotTo(o.HaveOccurred())

			// Currently following targets do not secure their /metrics endpoints:
			// job="crio" - https://issues.redhat.com/browse/MON-1034 + https://issues.redhat.com/browse/OCPNODE-321
			// job="ovnkube-master" - https://issues.redhat.com/browse/SDN-912
			// job="ovnkube-node" - https://issues.redhat.com/browse/SDN-912
			// Exclude list should be reduced to 0
			exclude := map[string]bool{
				"crio":           true,
				"ovnkube-master": true,
				"ovnkube-node":   true,
			}

			pattern := regexp.MustCompile("^https://.*")
			for _, t := range targets.Data.ActiveTargets {
				if exclude[t.Labels["job"]] {
					continue
				}
				if !pattern.MatchString(t.ScrapeUrl) {
					msg := fmt.Errorf("following target does not secure metrics endpoint: %v", t.Labels["job"])
					insecureTargets = append(insecureTargets, msg)
				}
			}
			o.Expect(insecureTargets).To(o.BeEmpty(), "some services expose metrics over insecure channel")
		})

		g.It("should have a AlertmanagerReceiversNotConfigured alert in firing state", func() {
			ns := oc.SetupNamespace()
			execPod := exutil.CreateExecPodOrFail(oc.AdminKubeClient(), ns, "execpod")
			defer func() {
				oc.AdminKubeClient().CoreV1().Pods(ns).Delete(context.Background(), execPod.Name, *metav1.NewDeleteOptions(1))
			}()

			tests := map[string]bool{
				`ALERTS{alertstate=~"firing|pending",alertname="AlertmanagerReceiversNotConfigured"} == 1`: true,
			}
			err := helper.RunQueries(tests, oc, ns, execPod.Name, url, bearerToken)
			o.Expect(err).NotTo(o.HaveOccurred())

			e2e.Logf("AlertmanagerReceiversNotConfigured alert is firing")
		})

		g.It("should have important platform topology metrics", func() {
			oc.SetupProject()
			ns := oc.Namespace()
			execPod := exutil.CreateExecPodOrFail(oc.AdminKubeClient(), ns, "execpod")
			defer func() {
				oc.AdminKubeClient().CoreV1().Pods(ns).Delete(context.Background(), execPod.Name, *metav1.NewDeleteOptions(1))
			}()

			tests := map[string]bool{
				// track infrastructure type
				`cluster_infrastructure_provider{type!=""}`: true,
				`cluster_feature_set`:                       true,

				// track installer type
				`cluster_installer{type!="",invoker!=""}`: true,

				// track sum of etcd
				`instance:etcd_object_counts:sum > 0`: true,

				// track cores and sockets across node types
				`sum(node_role_os_version_machine:cpu_capacity_cores:sum{label_kubernetes_io_arch!="",label_node_role_kubernetes_io_master!=""}) > 0`:                                      true,
				`sum(node_role_os_version_machine:cpu_capacity_sockets:sum{label_kubernetes_io_arch!="",label_node_hyperthread_enabled!="",label_node_role_kubernetes_io_master!=""}) > 0`: true,
			}
			err := helper.RunQueries(tests, oc, ns, execPod.Name, url, bearerToken)
			o.Expect(err).NotTo(o.HaveOccurred())
		})

		g.It("should have non-Pod host cAdvisor metrics", func() {
			ns := oc.SetupNamespace()
			execPod := exutil.CreateExecPodOrFail(oc.AdminKubeClient(), ns, "execpod")
			defer func() {
				oc.AdminKubeClient().CoreV1().Pods(ns).Delete(context.Background(), execPod.Name, *metav1.NewDeleteOptions(1))
			}()

			tests := map[string]bool{
				`container_cpu_usage_seconds_total{id!~"/kubepods.slice/.*"} >= 1`: true,
			}
			err := helper.RunQueries(tests, oc, ns, execPod.Name, url, bearerToken)
			o.Expect(err).NotTo(o.HaveOccurred())
		})

		g.It("shouldn't have failing rules evaluation", func() {
			oc.SetupProject()
			ns := oc.Namespace()
			execPod := exutil.CreateExecPodOrFail(oc.AdminKubeClient(), ns, "execpod")
			defer func() {
				oc.AdminKubeClient().CoreV1().Pods(ns).Delete(context.Background(), execPod.Name, *metav1.NewDeleteOptions(1))
			}()

			// we only consider samples since the beginning of the test
			testDuration := exutil.DurationSinceStartInSeconds().String()

			tests := map[string]bool{
				fmt.Sprintf(`increase(prometheus_rule_evaluation_failures_total[%s]) >= 1`, testDuration): false,
			}
			err := helper.RunQueries(tests, oc, ns, execPod.Name, url, bearerToken)
			o.Expect(err).NotTo(o.HaveOccurred())
		})

		networking.InOpenShiftSDNContext(func() {
			g.It("should be able to get the sdn ovs flows", func() {
				ns := oc.SetupNamespace()
				execPod := exutil.CreateExecPodOrFail(oc.AdminKubeClient(), ns, "execpod")
				defer func() {
					oc.AdminKubeClient().CoreV1().Pods(ns).Delete(context.Background(), execPod.Name, *metav1.NewDeleteOptions(1))
				}()

				tests := map[string]bool{
					//something
					`openshift_sdn_ovs_flows >= 1`: true,
				}
				err := helper.RunQueries(tests, oc, ns, execPod.Name, url, bearerToken)
				o.Expect(err).NotTo(o.HaveOccurred())
			})
		})

		g.It("shouldn't report any alerts in firing state apart from Watchdog and AlertmanagerReceiversNotConfigured [Early]", func() {
			if len(os.Getenv("TEST_UNSUPPORTED_ALLOW_VERSION_SKEW")) > 0 {
				e2eskipper.Skipf("Test is disabled to allow cluster components to have different versions, and skewed versions trigger multiple other alerts")
			}
			ns := oc.SetupNamespace()
			execPod := exutil.CreateExecPodOrFail(oc.AdminKubeClient(), ns, "execpod")
			defer func() {
				oc.AdminKubeClient().CoreV1().Pods(ns).Delete(context.Background(), execPod.Name, *metav1.NewDeleteOptions(1))
			}()

			tests := map[string]bool{
				// Checking Watchdog alert state is done in "should have a Watchdog alert in firing state".
				`ALERTS{alertname!~"Watchdog|AlertmanagerReceiversNotConfigured|PrometheusRemoteWriteDesiredShards",alertstate="firing",severity!="info"} >= 1`: false,
			}
			err := helper.RunQueries(tests, oc, ns, execPod.Name, url, bearerToken)
			o.Expect(err).NotTo(o.HaveOccurred())
		})

		g.It("should provide ingress metrics", func() {
			ns := oc.SetupNamespace()

			execPod := exutil.CreateExecPodOrFail(oc.AdminKubeClient(), ns, "execpod")
			defer func() {
				oc.AdminKubeClient().CoreV1().Pods(ns).Delete(context.Background(), execPod.Name, *metav1.NewDeleteOptions(1))
			}()

			var lastErrs []error
			o.Expect(wait.PollImmediate(10*time.Second, 4*time.Minute, func() (bool, error) {
				contents, err := getBearerTokenURLViaPod(ns, execPod.Name, fmt.Sprintf("%s/api/v1/targets", prometheusURL), bearerToken)
				o.Expect(err).NotTo(o.HaveOccurred())

				targets := &prometheusTargets{}
				err = json.Unmarshal([]byte(contents), targets)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("verifying all expected jobs have a working target")
				lastErrs = all(
					// Is there a good way to discover the name and thereby avoid leaking the naming algorithm?
					targets.Expect(labels{"job": "router-internal-default"}, "up", "^https://.*/metrics$"),
				)
				if len(lastErrs) > 0 {
					e2e.Logf("missing some targets: %v", lastErrs)
					return false, nil
				}
				return true, nil
			})).NotTo(o.HaveOccurred(), "ingress router cannot report metrics to monitoring system")

			g.By("verifying standard metrics keys")
			queries := map[string]bool{
				`template_router_reload_seconds_count{job="router-internal-default"} >= 1`: true,
				`haproxy_server_up{job="router-internal-default"} >= 1`:                    true,
			}
			err := helper.RunQueries(queries, oc, ns, execPod.Name, url, bearerToken)
			o.Expect(err).NotTo(o.HaveOccurred())
		})

		g.It("should provide named network metrics", func() {
			ns := oc.SetupNamespace()

			cs, err := newDynClientSet()
			o.Expect(err).NotTo(o.HaveOccurred())
			err = addNetwork(cs, "secondary", ns)
			o.Expect(err).NotTo(o.HaveOccurred())

			defer func() {
				err := removeNetwork(cs, "secondary", ns)
				o.Expect(err).NotTo(o.HaveOccurred())
			}()

			execPod := exutil.CreateExecPodOrFail(oc.AdminKubeClient(), ns, "execpod", func(pod *v1.Pod) {
				pod.Annotations = map[string]string{
					"k8s.v1.cni.cncf.io/networks": "secondary",
				}
			})

			defer func() {
				oc.AdminKubeClient().CoreV1().Pods(ns).Delete(context.Background(), execPod.Name, *metav1.NewDeleteOptions(1))
			}()

			g.By("verifying named metrics keys")
			queries := map[string]bool{
				fmt.Sprintf(`pod_network_name_info{pod="%s",namespace="%s",interface="eth0"} == 0`, execPod.Name, execPod.Namespace):                true,
				fmt.Sprintf(`pod_network_name_info{pod="%s",namespace="%s",network_name="%s/secondary"} == 0`, execPod.Name, execPod.Namespace, ns): true,
			}
			err = helper.RunQueries(queries, oc, ns, execPod.Name, url, bearerToken)
			o.Expect(err).NotTo(o.HaveOccurred())
		})
	})
})

func all(errs ...error) []error {
	var result []error
	for _, err := range errs {
		if err != nil {
			result = append(result, err)
		}
	}
	return result
}

type prometheusTargets struct {
	Data struct {
		ActiveTargets []struct {
			Labels    map[string]string
			Health    string
			ScrapeUrl string
		}
	}
	Status string
}

func (t *prometheusTargets) Expect(l labels, health, scrapeURLPattern string) error {
	for _, target := range t.Data.ActiveTargets {
		match := true
		for k, v := range l {
			if target.Labels[k] != v {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		if health != target.Health {
			continue
		}
		if !regexp.MustCompile(scrapeURLPattern).MatchString(target.ScrapeUrl) {
			continue
		}
		return nil
	}
	return fmt.Errorf("no match for %v with health %s and scrape URL %s", l, health, scrapeURLPattern)
}

type labels map[string]string

func (l labels) With(name, value string) labels {
	n := make(labels)
	for k, v := range l {
		n[k] = v
	}
	n[name] = value
	return n
}

func findEnvVar(vars []kapi.EnvVar, key string) string {
	for _, v := range vars {
		if v.Name == key {
			return v.Value
		}
	}
	return ""
}

func findMetricsWithLabels(f *dto.MetricFamily, labels map[string]string) []*dto.Metric {
	var result []*dto.Metric
	if f == nil {
		return result
	}
	for _, m := range f.Metric {
		matched := map[string]struct{}{}
		for _, l := range m.Label {
			if expect, ok := labels[l.GetName()]; ok {
				if expect != l.GetValue() {
					break
				}
				matched[l.GetName()] = struct{}{}
			}
		}
		if len(matched) != len(labels) {
			continue
		}
		result = append(result, m)
	}
	return result
}

func findCountersWithLabels(f *dto.MetricFamily, labels map[string]string) []float64 {
	var result []float64
	for _, m := range findMetricsWithLabels(f, labels) {
		result = append(result, m.Counter.GetValue())
	}
	return result
}

func findGaugesWithLabels(f *dto.MetricFamily, labels map[string]string) []float64 {
	var result []float64
	for _, m := range findMetricsWithLabels(f, labels) {
		result = append(result, m.Gauge.GetValue())
	}
	return result
}

func findMetricLabels(f *dto.MetricFamily, labels map[string]string, match string) []string {
	var result []string
	for _, m := range findMetricsWithLabels(f, labels) {
		for _, l := range m.Label {
			if l.GetName() == match {
				result = append(result, l.GetValue())
				break
			}
		}
	}
	return result
}

func expectBearerTokenURLStatusCodeExec(ns, execPodName, url, bearer string, statusCode int) error {
	cmd := fmt.Sprintf("curl -k -s -H 'Authorization: Bearer %s' -o /dev/null -w '%%{http_code}' %q", bearer, url)
	output, err := e2e.RunHostCmd(ns, execPodName, cmd)
	if err != nil {
		return fmt.Errorf("host command failed: %v\n%s", err, output)
	}
	if output != strconv.Itoa(statusCode) {
		return fmt.Errorf("last response from server was not %d: %s", statusCode, output)
	}
	return nil
}

func getBearerTokenURLViaPod(ns, execPodName, url, bearer string) (string, error) {
	cmd := fmt.Sprintf("curl -s -k -H 'Authorization: Bearer %s' %q", bearer, url)
	output, err := e2e.RunHostCmd(ns, execPodName, cmd)
	if err != nil {
		return "", fmt.Errorf("host command failed: %v\n%s", err, output)
	}
	return output, nil
}

func getAuthenticatedURLViaPod(ns, execPodName, url, user, pass string) (string, error) {
	cmd := fmt.Sprintf("curl -s -u %s:%s %q", user, pass, url)
	output, err := e2e.RunHostCmd(ns, execPodName, cmd)
	if err != nil {
		return "", fmt.Errorf("host command failed: %v\n%s", err, output)
	}
	return output, nil
}

func getInsecureURLViaPod(ns, execPodName, url string) (string, error) {
	cmd := fmt.Sprintf("curl -s -k %q", url)
	output, err := e2e.RunHostCmd(ns, execPodName, cmd)
	if err != nil {
		return "", fmt.Errorf("host command failed: %v\n%s", err, output)
	}
	return output, nil
}

func waitForServiceAccountInNamespace(c clientset.Interface, ns, serviceAccountName string, timeout time.Duration) error {
	w, err := c.CoreV1().ServiceAccounts(ns).Watch(context.Background(), metav1.SingleObject(metav1.ObjectMeta{Name: serviceAccountName}))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err = watchtools.UntilWithoutRetry(ctx, w, conditions.ServiceAccountHasSecrets)
	return err
}

func locatePrometheus(oc *exutil.CLI) (url, bearerToken string, ok bool) {
	_, err := oc.AdminKubeClient().CoreV1().Services("openshift-monitoring").Get(context.Background(), "prometheus-k8s", metav1.GetOptions{})
	if kapierrs.IsNotFound(err) {
		return "", "", false
	}

	waitForServiceAccountInNamespace(oc.AdminKubeClient(), "openshift-monitoring", "prometheus-k8s", 2*time.Minute)
	for i := 0; i < 30; i++ {
		secrets, err := oc.AdminKubeClient().CoreV1().Secrets("openshift-monitoring").List(context.Background(), metav1.ListOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		for _, secret := range secrets.Items {
			if secret.Type != v1.SecretTypeServiceAccountToken {
				continue
			}
			if !strings.HasPrefix(secret.Name, "prometheus-") {
				continue
			}
			bearerToken = string(secret.Data[v1.ServiceAccountTokenKey])
			break
		}
		if len(bearerToken) == 0 {
			e2e.Logf("Waiting for prometheus service account secret to show up")
			time.Sleep(time.Second)
			continue
		}
	}
	o.Expect(bearerToken).ToNot(o.BeEmpty())

	return "https://prometheus-k8s.openshift-monitoring.svc:9091", bearerToken, true
}

func hasPullSecret(client clientset.Interface, name string) bool {
	scrt, err := client.CoreV1().Secrets("openshift-config").Get(context.Background(), "pull-secret", metav1.GetOptions{})
	if err != nil {
		if kapierrs.IsNotFound(err) {
			return false
		}
		e2e.Failf("could not retrieve pull-secret: %v", err)
	}

	if scrt.Type != v1.SecretTypeDockerConfigJson {
		e2e.Failf("error expecting secret type %s got %s", v1.SecretTypeDockerConfigJson, scrt.Type)
	}

	ps := struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}{}

	if err := json.Unmarshal(scrt.Data[v1.DockerConfigJsonKey], &ps); err != nil {
		e2e.Failf("could not unmarshal pullSecret from openshift-config/pull-secret: %v", err)
	}
	return len(ps.Auths[name].Auth) > 0
}
