// Copyright 2017 The prometheus-operator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prometheus

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/blang/semver/v4"
	"github.com/go-kit/log"
	"github.com/go-openapi/swag"
	"github.com/google/go-cmp/cmp"
	"github.com/kylelemons/godebug/pretty"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/prometheus-operator/prometheus-operator/pkg/assets"
	"github.com/prometheus-operator/prometheus-operator/pkg/operator"
	"gopkg.in/yaml.v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/pointer"
)

func TestConfigGeneration(t *testing.T) {
	for _, v := range operator.PrometheusCompatibilityMatrix {
		t.Run(v, func(t *testing.T) {
			t.Parallel()
			cfg, err := generateTestConfig(v)
			if err != nil {
				t.Fatal(err)
			}

			reps := 1000
			if testing.Short() {
				reps = 100
			}

			for i := 0; i < reps; i++ {
				testcfg, err := generateTestConfig(v)
				if err != nil {
					t.Fatal(err)
				}

				if !bytes.Equal(cfg, testcfg) {
					t.Fatalf("Config generation is not deterministic.\n\n\nFirst generation: \n\n%s\n\nDifferent generation: \n\n%s\n\n", string(cfg), string(testcfg))
				}
			}
		})
	}
}

func TestGlobalSettings(t *testing.T) {
	type testCase struct {
		EvaluationInterval string
		ScrapeInterval     string
		ScrapeTimeout      string
		ExternalLabels     map[string]string
		QueryLogFile       string
		Version            string
		Expected           string
	}

	testcases := []testCase{
		{
			Version: "v2.15.2",
			Expected: `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: /
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`,
		},
		{
			Version:            "v2.15.2",
			EvaluationInterval: "60s",
			Expected: `global:
  evaluation_interval: 60s
  scrape_interval: 30s
  external_labels:
    prometheus: /
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`,
		},
		{
			Version:        "v2.15.2",
			ScrapeInterval: "60s",
			Expected: `global:
  evaluation_interval: 30s
  scrape_interval: 60s
  external_labels:
    prometheus: /
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`,
		},
		{
			Version:       "v2.15.2",
			ScrapeTimeout: "30s",
			Expected: `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: /
    prometheus_replica: $(POD_NAME)
  scrape_timeout: 30s
rule_files: []
scrape_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`,
		},
		{
			Version: "v2.15.2",
			ExternalLabels: map[string]string{
				"key1": "value1",
				"key2": "value2",
			},
			Expected: `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    key1: value1
    key2: value2
    prometheus: /
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`,
		},
		{
			Version:      "v2.16.0",
			QueryLogFile: "test.log",
			Expected: `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: /
    prometheus_replica: $(POD_NAME)
  query_log_file: test.log
rule_files: []
scrape_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`,
		},
	}

	for _, tc := range testcases {
		cg := &ConfigGenerator{}
		cfg, err := cg.GenerateConfig(
			&monitoringv1.Prometheus{
				ObjectMeta: metav1.ObjectMeta{},
				Spec: monitoringv1.PrometheusSpec{
					EvaluationInterval: tc.EvaluationInterval,
					ScrapeInterval:     tc.ScrapeInterval,
					ScrapeTimeout:      tc.ScrapeTimeout,
					ExternalLabels:     tc.ExternalLabels,
					QueryLogFile:       tc.QueryLogFile,
					Version:            tc.Version,
				},
			},
			map[string]*monitoringv1.ServiceMonitor{},
			nil,
			nil,
			&assets.Store{},
			nil,
			nil,
			nil,
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		result := string(cfg)
		if tc.Expected != string(cfg) {
			fmt.Println(pretty.Compare(tc.Expected, result))
			t.Fatal("expected Prometheus configuration and actual configuration do not match")
		}
	}
}

func TestNamespaceSetCorrectly(t *testing.T) {
	type testCase struct {
		ServiceMonitor           *monitoringv1.ServiceMonitor
		IgnoreNamespaceSelectors bool
		Expected                 string
	}

	testcases := []testCase{
		// Test that namespaces from 'MatchNames' are returned instead of the current namespace
		{
			ServiceMonitor: &monitoringv1.ServiceMonitor{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testservicemonitor1",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.ServiceMonitorSpec{
					NamespaceSelector: monitoringv1.NamespaceSelector{
						MatchNames: []string{"test1", "test2"},
					},
				},
			},
			IgnoreNamespaceSelectors: false,
			Expected: `kubernetes_sd_configs:
- role: endpoints
  namespaces:
    names:
    - test1
    - test2
`,
		},
		// Test that 'Any' returns an empty list instead of the current namespace
		{
			ServiceMonitor: &monitoringv1.ServiceMonitor{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testservicemonitor2",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.ServiceMonitorSpec{
					NamespaceSelector: monitoringv1.NamespaceSelector{
						Any: true,
					},
				},
			},
			IgnoreNamespaceSelectors: false,
			Expected: `kubernetes_sd_configs:
- role: endpoints
`,
		},
		// Test that Any takes precedence over MatchNames
		{
			ServiceMonitor: &monitoringv1.ServiceMonitor{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testservicemonitor2",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.ServiceMonitorSpec{
					NamespaceSelector: monitoringv1.NamespaceSelector{
						Any:        true,
						MatchNames: []string{"foo", "bar"},
					},
				},
			},
			IgnoreNamespaceSelectors: false,
			Expected: `kubernetes_sd_configs:
- role: endpoints
`,
		},
		// Test that IgnoreNamespaceSelectors overrides Any and MatchNames
		{
			ServiceMonitor: &monitoringv1.ServiceMonitor{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testservicemonitor2",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.ServiceMonitorSpec{
					NamespaceSelector: monitoringv1.NamespaceSelector{
						Any:        true,
						MatchNames: []string{"foo", "bar"},
					},
				},
			},
			IgnoreNamespaceSelectors: true,
			Expected: `kubernetes_sd_configs:
- role: endpoints
  namespaces:
    names:
    - default
`,
		},
	}
	cg := &ConfigGenerator{}

	for _, tc := range testcases {
		selectedNamespaces := getNamespacesFromNamespaceSelector(&tc.ServiceMonitor.Spec.NamespaceSelector, tc.ServiceMonitor.Namespace, tc.IgnoreNamespaceSelectors)
		c := cg.generateK8SSDConfig(semver.Version{}, selectedNamespaces, nil, nil, kubernetesSDRoleEndpoint)
		s, err := yaml.Marshal(yaml.MapSlice{c})
		if err != nil {
			t.Fatal(err)
		}
		if tc.Expected != string(s) {
			t.Fatalf("Unexpected result.\n\nGot:\n\n%s\n\nExpected:\n\n%s\n\n", string(s), tc.Expected)
		}
	}
}

func TestNamespaceSetCorrectlyForPodMonitor(t *testing.T) {
	pm := &monitoringv1.PodMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "testpodmonitor1",
			Namespace: "default",
			Labels: map[string]string{
				"group": "group1",
			},
		},
		Spec: monitoringv1.PodMonitorSpec{
			NamespaceSelector: monitoringv1.NamespaceSelector{
				MatchNames: []string{"test"},
			},
		},
	}

	cg := &ConfigGenerator{}
	selectedNamespaces := getNamespacesFromNamespaceSelector(&pm.Spec.NamespaceSelector, pm.Namespace, false)
	c := cg.generateK8SSDConfig(semver.Version{}, selectedNamespaces, nil, nil, kubernetesSDRolePod)
	s, err := yaml.Marshal(yaml.MapSlice{c})
	if err != nil {
		t.Fatal(err)
	}

	expected := `kubernetes_sd_configs:
- role: pod
  namespaces:
    names:
    - test
`

	result := string(s)
	if expected != result {
		t.Fatalf("Unexpected result.\n\nGot:\n\n%s\n\nExpected:\n\n%s\n\n", result, expected)
	}
}

func TestProbeStaticTargetsConfigGeneration(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "default",
			},
			Spec: monitoringv1.PrometheusSpec{
				ProbeSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"group": "group1",
					},
				},
			},
		},
		nil,
		nil,
		map[string]*monitoringv1.Probe{
			"probe1": {
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testprobe1",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.ProbeSpec{
					ProberSpec: monitoringv1.ProberSpec{
						Scheme:   "http",
						URL:      "blackbox.exporter.io",
						Path:     "/probe",
						ProxyURL: "socks://myproxy:9095",
					},
					Module: "http_2xx",
					Targets: monitoringv1.ProbeTargets{
						StaticConfig: &monitoringv1.ProbeTargetStaticConfig{
							Targets: []string{
								"prometheus.io",
								"promcon.io",
							},
							Labels: map[string]string{
								"static": "label",
							},
							RelabelConfigs: []*monitoringv1.RelabelConfig{
								{
									TargetLabel: "foo",
									Replacement: "bar",
									Action:      "replace",
								},
							},
						},
					},
				},
			},
		},
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)

	if err != nil {
		t.Fatal(err)
	}

	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: probe/default/testprobe1
  honor_timestamps: true
  metrics_path: /probe
  scheme: http
  proxy_url: socks://myproxy:9095
  params:
    module:
    - http_2xx
  static_configs:
  - targets:
    - prometheus.io
    - promcon.io
    labels:
      namespace: default
      static: label
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - source_labels:
    - __address__
    target_label: __param_target
  - source_labels:
    - __param_target
    target_label: instance
  - target_label: __address__
    replacement: blackbox.exporter.io
  - target_label: foo
    replacement: bar
    action: replace
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	result := string(cfg)
	if diff := cmp.Diff(expected, result); diff != "" {
		t.Fatalf("Unexpected result got(-) want(+)\n%s\n", diff)
	}
}

func TestProbeStaticTargetsConfigGenerationWithLabelEnforce(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "default",
			},
			Spec: monitoringv1.PrometheusSpec{
				EnforcedNamespaceLabel: "namespace",
				ProbeSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"group": "group1",
					},
				},
			},
		},
		nil,
		nil,
		map[string]*monitoringv1.Probe{
			"probe1": {
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testprobe1",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.ProbeSpec{
					ProberSpec: monitoringv1.ProberSpec{
						Scheme: "http",
						URL:    "blackbox.exporter.io",
						Path:   "/probe",
					},
					Module: "http_2xx",
					Targets: monitoringv1.ProbeTargets{
						StaticConfig: &monitoringv1.ProbeTargetStaticConfig{
							Targets: []string{
								"prometheus.io",
								"promcon.io",
							},
							Labels: map[string]string{
								"namespace": "custom",
								"static":    "label",
							},
						},
					},
					MetricRelabelConfigs: []*monitoringv1.RelabelConfig{
						{
							Regex:  "noisy_labels.*",
							Action: "labeldrop",
						},
					},
				},
			},
		},
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)

	if err != nil {
		t.Fatal(err)
	}

	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: probe/default/testprobe1
  honor_timestamps: true
  metrics_path: /probe
  scheme: http
  params:
    module:
    - http_2xx
  static_configs:
  - targets:
    - prometheus.io
    - promcon.io
    labels:
      namespace: custom
      static: label
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - source_labels:
    - __address__
    target_label: __param_target
  - source_labels:
    - __param_target
    target_label: instance
  - target_label: __address__
    replacement: blackbox.exporter.io
  - target_label: namespace
    replacement: default
  metric_relabel_configs:
  - regex: noisy_labels.*
    action: labeldrop
  - target_label: namespace
    replacement: default
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	result := string(cfg)
	if diff := cmp.Diff(expected, result); diff != "" {
		t.Fatalf("Unexpected result got(-) want(+)\n%s\n", diff)
	}
}

func TestProbeStaticTargetsConfigGenerationWithJobName(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "default",
			},
			Spec: monitoringv1.PrometheusSpec{
				ProbeSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"group": "group1",
					},
				},
			},
		},
		nil,
		nil,
		map[string]*monitoringv1.Probe{
			"probe1": {
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testprobe1",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.ProbeSpec{
					JobName: "blackbox",
					ProberSpec: monitoringv1.ProberSpec{
						Scheme: "http",
						URL:    "blackbox.exporter.io",
						Path:   "/probe",
					},
					Module: "http_2xx",
					Targets: monitoringv1.ProbeTargets{
						StaticConfig: &monitoringv1.ProbeTargetStaticConfig{
							Targets: []string{
								"prometheus.io",
								"promcon.io",
							},
						},
					},
				},
			},
		},
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)

	if err != nil {
		t.Fatal(err)
	}

	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: probe/default/testprobe1
  honor_timestamps: true
  metrics_path: /probe
  scheme: http
  params:
    module:
    - http_2xx
  static_configs:
  - targets:
    - prometheus.io
    - promcon.io
    labels:
      namespace: default
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - target_label: job
    replacement: blackbox
  - source_labels:
    - __address__
    target_label: __param_target
  - source_labels:
    - __param_target
    target_label: instance
  - target_label: __address__
    replacement: blackbox.exporter.io
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	result := string(cfg)
	if diff := cmp.Diff(expected, result); diff != "" {
		t.Fatalf("Unexpected result got(-) want(+)\n%s\n", diff)
	}
}

func TestProbeStaticTargetsConfigGenerationWithoutModule(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "default",
			},
			Spec: monitoringv1.PrometheusSpec{
				ProbeSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"group": "group1",
					},
				},
			},
		},
		nil,
		nil,
		map[string]*monitoringv1.Probe{
			"probe1": {
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testprobe1",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.ProbeSpec{
					JobName: "blackbox",
					ProberSpec: monitoringv1.ProberSpec{
						Scheme: "http",
						URL:    "blackbox.exporter.io",
						Path:   "/probe",
					},
					Targets: monitoringv1.ProbeTargets{
						StaticConfig: &monitoringv1.ProbeTargetStaticConfig{
							Targets: []string{
								"prometheus.io",
								"promcon.io",
							},
						},
					},
				},
			},
		},
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)

	if err != nil {
		t.Fatal(err)
	}

	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: probe/default/testprobe1
  honor_timestamps: true
  metrics_path: /probe
  scheme: http
  static_configs:
  - targets:
    - prometheus.io
    - promcon.io
    labels:
      namespace: default
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - target_label: job
    replacement: blackbox
  - source_labels:
    - __address__
    target_label: __param_target
  - source_labels:
    - __param_target
    target_label: instance
  - target_label: __address__
    replacement: blackbox.exporter.io
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	result := string(cfg)
	if diff := cmp.Diff(expected, result); diff != "" {
		t.Fatalf("Unexpected result got(-) want(+)\n%s\n", diff)
	}
}

func TestProbeIngressSDConfigGeneration(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "default",
			},
			Spec: monitoringv1.PrometheusSpec{
				ProbeSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"group": "group1",
					},
				},
			},
		},
		nil,
		nil,
		map[string]*monitoringv1.Probe{
			"probe1": {
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testprobe1",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.ProbeSpec{
					ProberSpec: monitoringv1.ProberSpec{
						Scheme: "http",
						URL:    "blackbox.exporter.io",
						Path:   "/probe",
					},
					Module: "http_2xx",
					Targets: monitoringv1.ProbeTargets{
						Ingress: &monitoringv1.ProbeTargetIngress{
							Selector: metav1.LabelSelector{
								MatchLabels: map[string]string{
									"prometheus.io/probe": "true",
								},
							},
							NamespaceSelector: monitoringv1.NamespaceSelector{
								Any: true,
							},
							RelabelConfigs: []*monitoringv1.RelabelConfig{
								{
									TargetLabel: "foo",
									Replacement: "bar",
									Action:      "replace",
								},
							},
						},
					},
				},
			},
		},
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)

	if err != nil {
		t.Fatal(err)
	}

	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: probe/default/testprobe1
  honor_timestamps: true
  metrics_path: /probe
  scheme: http
  params:
    module:
    - http_2xx
  kubernetes_sd_configs:
  - role: ingress
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_ingress_label_prometheus_io_probe
    regex: "true"
  - source_labels:
    - __meta_kubernetes_ingress_scheme
    - __address__
    - __meta_kubernetes_ingress_path
    separator: ;
    regex: (.+);(.+);(.+)
    target_label: __param_target
    replacement: ${1}://${2}${3}
    action: replace
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_ingress_name
    target_label: ingress
  - source_labels:
    - __param_target
    target_label: instance
  - target_label: __address__
    replacement: blackbox.exporter.io
  - target_label: foo
    replacement: bar
    action: replace
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	result := string(cfg)
	if expected != result {
		t.Fatalf("Unexpected result.\n\nGot:\n\n%s\n\nExpected:\n\n%s\n\n", result, expected)
	}
}

func TestProbeIngressSDConfigGenerationWithLabelEnforce(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "default",
			},
			Spec: monitoringv1.PrometheusSpec{
				EnforcedNamespaceLabel: "namespace",
				ProbeSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"group": "group1",
					},
				},
			},
		},
		nil,
		nil,
		map[string]*monitoringv1.Probe{
			"probe1": {
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testprobe1",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.ProbeSpec{
					ProberSpec: monitoringv1.ProberSpec{
						Scheme: "http",
						URL:    "blackbox.exporter.io",
						Path:   "/probe",
					},
					Module: "http_2xx",
					Targets: monitoringv1.ProbeTargets{
						Ingress: &monitoringv1.ProbeTargetIngress{
							Selector: metav1.LabelSelector{
								MatchLabels: map[string]string{
									"prometheus.io/probe": "true",
								},
							},
							NamespaceSelector: monitoringv1.NamespaceSelector{
								Any: true,
							},
							RelabelConfigs: []*monitoringv1.RelabelConfig{
								{
									TargetLabel: "foo",
									Replacement: "bar",
									Action:      "replace",
								},
							},
						},
					},
				},
			},
		},
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)

	if err != nil {
		t.Fatal(err)
	}

	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: probe/default/testprobe1
  honor_timestamps: true
  metrics_path: /probe
  scheme: http
  params:
    module:
    - http_2xx
  kubernetes_sd_configs:
  - role: ingress
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_ingress_label_prometheus_io_probe
    regex: "true"
  - source_labels:
    - __meta_kubernetes_ingress_scheme
    - __address__
    - __meta_kubernetes_ingress_path
    separator: ;
    regex: (.+);(.+);(.+)
    target_label: __param_target
    replacement: ${1}://${2}${3}
    action: replace
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_ingress_name
    target_label: ingress
  - source_labels:
    - __param_target
    target_label: instance
  - target_label: __address__
    replacement: blackbox.exporter.io
  - target_label: foo
    replacement: bar
    action: replace
  - target_label: namespace
    replacement: default
  metric_relabel_configs:
  - target_label: namespace
    replacement: default
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	result := string(cfg)
	if diff := cmp.Diff(expected, result); diff != "" {
		t.Fatalf("Unexpected result got(-) want(+)\n%s\n", diff)
	}
}

func TestK8SSDConfigGeneration(t *testing.T) {
	sm := &monitoringv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "testservicemonitor1",
			Namespace: "default",
			Labels: map[string]string{
				"group": "group1",
			},
		},
		Spec: monitoringv1.ServiceMonitorSpec{
			NamespaceSelector: monitoringv1.NamespaceSelector{
				MatchNames: []string{"test"},
			},
		},
	}

	cg := &ConfigGenerator{}

	testcases := []struct {
		apiserverConfig *monitoringv1.APIServerConfig
		store           *assets.Store
		expected        string
	}{
		{
			nil,
			nil,
			`kubernetes_sd_configs:
- role: endpoints
  namespaces:
    names:
    - test
`,
		},
		{
			&monitoringv1.APIServerConfig{
				Host:            "example.com",
				BasicAuth:       &monitoringv1.BasicAuth{},
				BearerToken:     "bearer_token",
				BearerTokenFile: "bearer_token_file",
				TLSConfig:       nil,
			},
			&assets.Store{
				BasicAuthAssets: map[string]assets.BasicAuthCredentials{
					"apiserver": {
						Username: "foo",
						Password: "bar",
					},
				},
				OAuth2Assets: map[string]assets.OAuth2Credentials{},
				TokenAssets:  map[string]assets.Token{},
			},
			`kubernetes_sd_configs:
- role: endpoints
  namespaces:
    names:
    - test
  api_server: example.com
  basic_auth:
    username: foo
    password: bar
  bearer_token: bearer_token
  bearer_token_file: bearer_token_file
`,
		},
	}

	for _, tc := range testcases {
		c := cg.generateK8SSDConfig(
			semver.Version{},
			getNamespacesFromNamespaceSelector(&sm.Spec.NamespaceSelector, sm.Namespace, false),
			tc.apiserverConfig,
			tc.store,
			kubernetesSDRoleEndpoint,
		)
		s, err := yaml.Marshal(yaml.MapSlice{c})
		if err != nil {
			t.Fatal(err)
		}
		result := string(s)

		if result != tc.expected {
			t.Fatalf("Unexpected result.\n\nGot:\n\n%s\n\nExpected:\n\n%s\n\n", result, tc.expected)
		}
	}
}

func TestAlertmanagerBearerToken(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "default",
			},
			Spec: monitoringv1.PrometheusSpec{
				Alerting: &monitoringv1.AlertingSpec{
					Alertmanagers: []monitoringv1.AlertmanagerEndpoints{
						{
							Name:            "alertmanager-main",
							Namespace:       "default",
							Port:            intstr.FromString("web"),
							BearerTokenFile: "/some/file/on/disk",
						},
					},
				},
			},
		},
		nil,
		nil,
		nil,
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	// If this becomes an endless sink of maintenance, then we should just
	// change this to check that just the `bearer_token_file` is set with
	// something like json-path.
	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers:
  - path_prefix: /
    scheme: http
    kubernetes_sd_configs:
    - role: endpoints
      namespaces:
        names:
        - default
    bearer_token_file: /some/file/on/disk
    relabel_configs:
    - action: keep
      source_labels:
      - __meta_kubernetes_service_name
      regex: alertmanager-main
    - action: keep
      source_labels:
      - __meta_kubernetes_endpoint_port_name
      regex: web
`

	result := string(cfg)

	if expected != result {
		fmt.Println(pretty.Compare(expected, result))
		t.Fatal("expected Prometheus configuration and actual configuration do not match")
	}
}

func TestAlertmanagerAPIVersion(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "default",
			},
			Spec: monitoringv1.PrometheusSpec{
				Version: "v2.11.0",
				Alerting: &monitoringv1.AlertingSpec{
					Alertmanagers: []monitoringv1.AlertmanagerEndpoints{
						{
							Name:       "alertmanager-main",
							Namespace:  "default",
							Port:       intstr.FromString("web"),
							APIVersion: "v2",
						},
					},
				},
			},
		},
		nil,
		nil,
		nil,
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	// If this becomes an endless sink of maintenance, then we should just
	// change this to check that just the `api_version` is set with
	// something like json-path.
	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers:
  - path_prefix: /
    scheme: http
    kubernetes_sd_configs:
    - role: endpoints
      namespaces:
        names:
        - default
    api_version: v2
    relabel_configs:
    - action: keep
      source_labels:
      - __meta_kubernetes_service_name
      regex: alertmanager-main
    - action: keep
      source_labels:
      - __meta_kubernetes_endpoint_port_name
      regex: web
`

	result := string(cfg)

	if expected != result {
		fmt.Println(pretty.Compare(expected, result))
		t.Fatal("expected Prometheus configuration and actual configuration do not match")
	}
}

func TestAlertmanagerTimeoutConfig(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "default",
			},
			Spec: monitoringv1.PrometheusSpec{
				Version: "v2.11.0",
				Alerting: &monitoringv1.AlertingSpec{
					Alertmanagers: []monitoringv1.AlertmanagerEndpoints{
						{
							Name:       "alertmanager-main",
							Namespace:  "default",
							Port:       intstr.FromString("web"),
							APIVersion: "v2",
							Timeout:    pointer.StringPtr("60s"),
						},
					},
				},
			},
		},
		nil,
		nil,
		nil,
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	// If this becomes an endless sink of maintenance, then we should just
	// change this to check that just the `api_version` is set with
	// something like json-path.
	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers:
  - path_prefix: /
    scheme: http
    timeout: 60s
    kubernetes_sd_configs:
    - role: endpoints
      namespaces:
        names:
        - default
    api_version: v2
    relabel_configs:
    - action: keep
      source_labels:
      - __meta_kubernetes_service_name
      regex: alertmanager-main
    - action: keep
      source_labels:
      - __meta_kubernetes_endpoint_port_name
      regex: web
`

	result := string(cfg)

	if expected != result {
		fmt.Println(pretty.Compare(expected, result))
		t.Fatal("expected Prometheus configuration and actual configuration do not match")
	}
}

func TestAdditionalAlertRelabelConfigs(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "default",
			},
			Spec: monitoringv1.PrometheusSpec{
				Alerting: &monitoringv1.AlertingSpec{
					Alertmanagers: []monitoringv1.AlertmanagerEndpoints{
						{
							Name:      "alertmanager-main",
							Namespace: "default",
							Port:      intstr.FromString("web"),
						},
					},
				},
			},
		},
		nil,
		nil,
		nil,
		&assets.Store{},
		nil,
		[]byte(`- action: drop
  source_labels: [__meta_kubernetes_node_name]
  regex: spot-(.+)

`),
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  - action: drop
    source_labels:
    - __meta_kubernetes_node_name
    regex: spot-(.+)
  alertmanagers:
  - path_prefix: /
    scheme: http
    kubernetes_sd_configs:
    - role: endpoints
      namespaces:
        names:
        - default
    relabel_configs:
    - action: keep
      source_labels:
      - __meta_kubernetes_service_name
      regex: alertmanager-main
    - action: keep
      source_labels:
      - __meta_kubernetes_endpoint_port_name
      regex: web
`

	result := string(cfg)

	if expected != result {
		fmt.Println(pretty.Compare(expected, result))
		t.Fatal("expected Prometheus configuration and actual configuration do not match")
	}
}

func TestNoEnforcedNamespaceLabelServiceMonitor(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "ns-value",
			},
			Spec: monitoringv1.PrometheusSpec{},
		},
		map[string]*monitoringv1.ServiceMonitor{
			"test": {
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Spec: monitoringv1.ServiceMonitorSpec{
					Selector: metav1.LabelSelector{
						MatchLabels: map[string]string{
							"foo": "bar",
						},
					},
					Endpoints: []monitoringv1.Endpoint{
						{
							Port:        "https-metrics",
							HonorLabels: true,
							Interval:    "30s",
							MetricRelabelConfigs: []*monitoringv1.RelabelConfig{
								{
									Action:       "drop",
									Regex:        "container_(network_tcp_usage_total|network_udp_usage_total|tasks_state|cpu_load_average_10s)",
									SourceLabels: []string{"__name__"},
								},
							},
							RelabelConfigs: []*monitoringv1.RelabelConfig{
								{
									Action:       "replace",
									Regex:        "(.+)(?::d+)",
									Replacement:  "$1:9537",
									SourceLabels: []string{"__address__"},
									TargetLabel:  "__address__",
								},
								{
									Action:      "replace",
									Replacement: "crio",
									TargetLabel: "job",
								},
							},
						},
					},
				},
			},
		},
		nil,
		nil,
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: ns-value/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: serviceMonitor/default/test/0
  honor_labels: true
  kubernetes_sd_configs:
  - role: endpoints
    namespaces:
      names:
      - default
  scrape_interval: 30s
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_service_label_foo
    regex: bar
  - action: keep
    source_labels:
    - __meta_kubernetes_endpoint_port_name
    regex: https-metrics
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Node;(.*)
    replacement: ${1}
    target_label: node
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Pod;(.*)
    replacement: ${1}
    target_label: pod
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: service
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: job
    replacement: ${1}
  - target_label: endpoint
    replacement: https-metrics
  - source_labels:
    - __address__
    target_label: __address__
    regex: (.+)(?::d+)
    replacement: $1:9537
    action: replace
  - target_label: job
    replacement: crio
    action: replace
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  metric_relabel_configs:
  - source_labels:
    - __name__
    regex: container_(network_tcp_usage_total|network_udp_usage_total|tasks_state|cpu_load_average_10s)
    action: drop
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	result := string(cfg)
	if expected != result {
		fmt.Println(pretty.Compare(expected, result))
		t.Fatal("expected Prometheus configuration and actual configuration do not match")
	}
}
func TestEnforcedNamespaceLabelPodMonitor(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "ns-value",
			},
			Spec: monitoringv1.PrometheusSpec{
				EnforcedNamespaceLabel: "ns-key",
			},
		},
		nil,
		map[string]*monitoringv1.PodMonitor{
			"testpodmonitor1": {
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testpodmonitor1",
					Namespace: "pod-monitor-ns",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.PodMonitorSpec{
					PodTargetLabels: []string{"example", "env"},
					PodMetricsEndpoints: []monitoringv1.PodMetricsEndpoint{
						{
							Port:     "web",
							Interval: "30s",
							MetricRelabelConfigs: []*monitoringv1.RelabelConfig{
								{
									Action:       "drop",
									Regex:        "my-job-pod-.+",
									SourceLabels: []string{"pod_name"},
									TargetLabel:  "my-ns",
								},
							},
							RelabelConfigs: []*monitoringv1.RelabelConfig{
								{
									Action:       "replace",
									Regex:        "(.*)",
									Replacement:  "$1",
									SourceLabels: []string{"__meta_kubernetes_pod_ready"},
								},
							},
						},
					},
				},
			},
		},
		nil,
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: ns-value/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: podMonitor/pod-monitor-ns/testpodmonitor1/0
  honor_labels: false
  kubernetes_sd_configs:
  - role: pod
    namespaces:
      names:
      - pod-monitor-ns
  scrape_interval: 30s
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_pod_container_port_name
    regex: web
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - source_labels:
    - __meta_kubernetes_pod_label_example
    target_label: example
    regex: (.+)
    replacement: ${1}
  - source_labels:
    - __meta_kubernetes_pod_label_env
    target_label: env
    regex: (.+)
    replacement: ${1}
  - target_label: job
    replacement: pod-monitor-ns/testpodmonitor1
  - target_label: endpoint
    replacement: web
  - source_labels:
    - __meta_kubernetes_pod_ready
    regex: (.*)
    replacement: $1
    action: replace
  - target_label: ns-key
    replacement: pod-monitor-ns
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  metric_relabel_configs:
  - source_labels:
    - pod_name
    target_label: my-ns
    regex: my-job-pod-.+
    action: drop
  - target_label: ns-key
    replacement: pod-monitor-ns
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	result := string(cfg)
	if expected != result {
		diff := cmp.Diff(expected, result)
		t.Fatalf("expected Prometheus configuration and actual configuration do not match\n%s", diff)
	}
}

func TestEnforcedNamespaceLabelServiceMonitor(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "ns-value",
			},
			Spec: monitoringv1.PrometheusSpec{
				EnforcedNamespaceLabel: "ns-key",
			},
		},
		map[string]*monitoringv1.ServiceMonitor{
			"test": {
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Spec: monitoringv1.ServiceMonitorSpec{
					Selector: metav1.LabelSelector{
						MatchLabels: map[string]string{
							"foo": "bar",
						},
					},
					Endpoints: []monitoringv1.Endpoint{
						{
							Port:     "web",
							Interval: "30s",
							MetricRelabelConfigs: []*monitoringv1.RelabelConfig{
								{
									Action:       "drop",
									Regex:        "my-job-pod-.+",
									SourceLabels: []string{"pod_name"},
									TargetLabel:  "ns-key",
								},
							},
							RelabelConfigs: []*monitoringv1.RelabelConfig{
								{
									Action:       "replace",
									Regex:        "(.*)",
									Replacement:  "$1",
									SourceLabels: []string{"__meta_kubernetes_pod_ready"},
								},
							},
						},
					},
				},
			},
		},
		nil,
		nil,
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: ns-value/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: serviceMonitor/default/test/0
  honor_labels: false
  kubernetes_sd_configs:
  - role: endpoints
    namespaces:
      names:
      - default
  scrape_interval: 30s
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_service_label_foo
    regex: bar
  - action: keep
    source_labels:
    - __meta_kubernetes_endpoint_port_name
    regex: web
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Node;(.*)
    replacement: ${1}
    target_label: node
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Pod;(.*)
    replacement: ${1}
    target_label: pod
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: service
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: job
    replacement: ${1}
  - target_label: endpoint
    replacement: web
  - source_labels:
    - __meta_kubernetes_pod_ready
    regex: (.*)
    replacement: $1
    action: replace
  - target_label: ns-key
    replacement: default
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  metric_relabel_configs:
  - source_labels:
    - pod_name
    target_label: ns-key
    regex: my-job-pod-.+
    action: drop
  - target_label: ns-key
    replacement: default
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	result := string(cfg)
	if expected != result {
		diff := cmp.Diff(expected, result)
		t.Fatalf("expected Prometheus configuration and actual configuration do not match for enforced namespace label test:\n%s", diff)
	}
}

func TestAdditionalAlertmanagers(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "default",
			},
			Spec: monitoringv1.PrometheusSpec{
				Alerting: &monitoringv1.AlertingSpec{
					Alertmanagers: []monitoringv1.AlertmanagerEndpoints{
						{
							Name:      "alertmanager-main",
							Namespace: "default",
							Port:      intstr.FromString("web"),
						},
					},
				},
			},
		},
		nil,
		nil,
		nil,
		&assets.Store{},
		nil,
		nil,
		[]byte(`- static_configs:
  - targets:
    - localhost
`),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers:
  - path_prefix: /
    scheme: http
    kubernetes_sd_configs:
    - role: endpoints
      namespaces:
        names:
        - default
    relabel_configs:
    - action: keep
      source_labels:
      - __meta_kubernetes_service_name
      regex: alertmanager-main
    - action: keep
      source_labels:
      - __meta_kubernetes_endpoint_port_name
      regex: web
  - static_configs:
    - targets:
      - localhost
`

	result := string(cfg)

	if expected != result {
		fmt.Println(pretty.Compare(expected, result))
		t.Fatal("expected Prometheus configuration and actual configuration do not match")
	}
}

func TestSettingHonorTimestampsInServiceMonitor(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "default",
			},
			Spec: monitoringv1.PrometheusSpec{
				Version: "v2.9.0",
				ServiceMonitorSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"group": "group1",
					},
				},
			},
		},
		map[string]*monitoringv1.ServiceMonitor{
			"testservicemonitor1": {
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testservicemonitor1",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.ServiceMonitorSpec{
					TargetLabels: []string{"example", "env"},
					Endpoints: []monitoringv1.Endpoint{
						{
							HonorTimestamps: swag.Bool(false),
							Port:            "web",
							Interval:        "30s",
						},
					},
				},
			},
		},
		nil,
		nil,
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: serviceMonitor/default/testservicemonitor1/0
  honor_labels: false
  honor_timestamps: false
  kubernetes_sd_configs:
  - role: endpoints
    namespaces:
      names:
      - default
  scrape_interval: 30s
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_endpoint_port_name
    regex: web
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Node;(.*)
    replacement: ${1}
    target_label: node
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Pod;(.*)
    replacement: ${1}
    target_label: pod
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: service
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_service_label_example
    target_label: example
    regex: (.+)
    replacement: ${1}
  - source_labels:
    - __meta_kubernetes_service_label_env
    target_label: env
    regex: (.+)
    replacement: ${1}
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: job
    replacement: ${1}
  - target_label: endpoint
    replacement: web
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	result := string(cfg)

	if expected != result {
		fmt.Println(pretty.Compare(expected, result))
		t.Fatal("expected Prometheus configuration and actual configuration do not match")
	}
}

func TestSettingHonorTimestampsInPodMonitor(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "default",
			},
			Spec: monitoringv1.PrometheusSpec{
				Version: "v2.9.0",
				ServiceMonitorSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"group": "group1",
					},
				},
			},
		},
		nil,
		map[string]*monitoringv1.PodMonitor{
			"testpodmonitor1": {
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testpodmonitor1",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.PodMonitorSpec{
					PodTargetLabels: []string{"example", "env"},
					PodMetricsEndpoints: []monitoringv1.PodMetricsEndpoint{
						{
							HonorTimestamps: swag.Bool(false),
							Port:            "web",
							Interval:        "30s",
						},
					},
				},
			},
		},
		nil,
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: podMonitor/default/testpodmonitor1/0
  honor_labels: false
  honor_timestamps: false
  kubernetes_sd_configs:
  - role: pod
    namespaces:
      names:
      - default
  scrape_interval: 30s
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_pod_container_port_name
    regex: web
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - source_labels:
    - __meta_kubernetes_pod_label_example
    target_label: example
    regex: (.+)
    replacement: ${1}
  - source_labels:
    - __meta_kubernetes_pod_label_env
    target_label: env
    regex: (.+)
    replacement: ${1}
  - target_label: job
    replacement: default/testpodmonitor1
  - target_label: endpoint
    replacement: web
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	result := string(cfg)

	if expected != result {
		fmt.Println(pretty.Compare(expected, result))
		t.Fatal("expected Prometheus configuration and actual configuration do not match")
	}
}

func TestHonorTimestampsOverriding(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "default",
			},
			Spec: monitoringv1.PrometheusSpec{
				Version:                 "v2.9.0",
				OverrideHonorTimestamps: true,
				ServiceMonitorSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"group": "group1",
					},
				},
			},
		},
		map[string]*monitoringv1.ServiceMonitor{
			"testservicemonitor1": {
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testservicemonitor1",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.ServiceMonitorSpec{
					TargetLabels: []string{"example", "env"},
					Endpoints: []monitoringv1.Endpoint{
						{
							HonorTimestamps: swag.Bool(true),
							Port:            "web",
							Interval:        "30s",
						},
					},
				},
			},
		},
		nil,
		nil,
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: serviceMonitor/default/testservicemonitor1/0
  honor_labels: false
  honor_timestamps: false
  kubernetes_sd_configs:
  - role: endpoints
    namespaces:
      names:
      - default
  scrape_interval: 30s
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_endpoint_port_name
    regex: web
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Node;(.*)
    replacement: ${1}
    target_label: node
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Pod;(.*)
    replacement: ${1}
    target_label: pod
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: service
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_service_label_example
    target_label: example
    regex: (.+)
    replacement: ${1}
  - source_labels:
    - __meta_kubernetes_service_label_env
    target_label: env
    regex: (.+)
    replacement: ${1}
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: job
    replacement: ${1}
  - target_label: endpoint
    replacement: web
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	result := string(cfg)

	if expected != result {
		fmt.Println(pretty.Compare(expected, result))
		t.Fatal("expected Prometheus configuration and actual configuration do not match")
	}
}

func TestSettingHonorLabels(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "default",
			},
			Spec: monitoringv1.PrometheusSpec{
				ServiceMonitorSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"group": "group1",
					},
				},
			},
		},
		map[string]*monitoringv1.ServiceMonitor{
			"testservicemonitor1": {
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testservicemonitor1",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.ServiceMonitorSpec{
					TargetLabels: []string{"example", "env"},
					Endpoints: []monitoringv1.Endpoint{
						{
							HonorLabels: true,
							Port:        "web",
							Interval:    "30s",
						},
					},
				},
			},
		},
		nil,
		nil,
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: serviceMonitor/default/testservicemonitor1/0
  honor_labels: true
  kubernetes_sd_configs:
  - role: endpoints
    namespaces:
      names:
      - default
  scrape_interval: 30s
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_endpoint_port_name
    regex: web
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Node;(.*)
    replacement: ${1}
    target_label: node
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Pod;(.*)
    replacement: ${1}
    target_label: pod
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: service
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_service_label_example
    target_label: example
    regex: (.+)
    replacement: ${1}
  - source_labels:
    - __meta_kubernetes_service_label_env
    target_label: env
    regex: (.+)
    replacement: ${1}
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: job
    replacement: ${1}
  - target_label: endpoint
    replacement: web
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	result := string(cfg)

	if expected != result {
		fmt.Println(pretty.Compare(expected, result))
		t.Fatal("expected Prometheus configuration and actual configuration do not match")
	}
}

func TestHonorLabelsOverriding(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "default",
			},
			Spec: monitoringv1.PrometheusSpec{
				OverrideHonorLabels: true,
				ServiceMonitorSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"group": "group1",
					},
				},
			},
		},
		map[string]*monitoringv1.ServiceMonitor{
			"testservicemonitor1": {
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testservicemonitor1",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.ServiceMonitorSpec{
					TargetLabels: []string{"example", "env"},
					Endpoints: []monitoringv1.Endpoint{
						{
							HonorLabels: true,
							Port:        "web",
							Interval:    "30s",
						},
					},
				},
			},
		},
		nil,
		nil,
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: serviceMonitor/default/testservicemonitor1/0
  honor_labels: false
  kubernetes_sd_configs:
  - role: endpoints
    namespaces:
      names:
      - default
  scrape_interval: 30s
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_endpoint_port_name
    regex: web
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Node;(.*)
    replacement: ${1}
    target_label: node
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Pod;(.*)
    replacement: ${1}
    target_label: pod
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: service
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_service_label_example
    target_label: example
    regex: (.+)
    replacement: ${1}
  - source_labels:
    - __meta_kubernetes_service_label_env
    target_label: env
    regex: (.+)
    replacement: ${1}
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: job
    replacement: ${1}
  - target_label: endpoint
    replacement: web
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	result := string(cfg)

	if expected != result {
		fmt.Println(pretty.Compare(expected, result))
		t.Fatal("expected Prometheus configuration and actual configuration do not match")
	}
}

func TestTargetLabels(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "default",
			},
			Spec: monitoringv1.PrometheusSpec{
				OverrideHonorLabels: false,
				ServiceMonitorSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"group": "group1",
					},
				},
			},
		},
		map[string]*monitoringv1.ServiceMonitor{
			"testservicemonitor1": {
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testservicemonitor1",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.ServiceMonitorSpec{
					TargetLabels: []string{"example", "env"},
					Endpoints: []monitoringv1.Endpoint{
						{
							Port:     "web",
							Interval: "30s",
						},
					},
				},
			},
		},
		nil,
		nil,
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: serviceMonitor/default/testservicemonitor1/0
  honor_labels: false
  kubernetes_sd_configs:
  - role: endpoints
    namespaces:
      names:
      - default
  scrape_interval: 30s
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_endpoint_port_name
    regex: web
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Node;(.*)
    replacement: ${1}
    target_label: node
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Pod;(.*)
    replacement: ${1}
    target_label: pod
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: service
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_service_label_example
    target_label: example
    regex: (.+)
    replacement: ${1}
  - source_labels:
    - __meta_kubernetes_service_label_env
    target_label: env
    regex: (.+)
    replacement: ${1}
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: job
    replacement: ${1}
  - target_label: endpoint
    replacement: web
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	result := string(cfg)

	if expected != result {
		fmt.Println(pretty.Compare(expected, result))
		t.Fatal("expected Prometheus configuration and actual configuration do not match")
	}
}

func TestEndpointOAuth2(t *testing.T) {
	oauth2 := monitoringv1.OAuth2{
		ClientID: monitoringv1.SecretOrConfigMap{
			ConfigMap: &v1.ConfigMapKeySelector{
				LocalObjectReference: v1.LocalObjectReference{
					Name: "oauth2",
				},
				Key: "client_id",
			},
		},
		ClientSecret: v1.SecretKeySelector{
			LocalObjectReference: v1.LocalObjectReference{
				Name: "oauth2",
			},
			Key: "client_secret",
		},
		TokenURL: "http://test.url",
		Scopes:   []string{"scope 1", "scope 2"},
		EndpointParams: map[string]string{
			"param1": "value1",
			"param2": "value2",
		},
	}

	expectedCfg := strings.TrimSpace(`
oauth2:
    client_id: test_client_id
    client_secret: test_client_secret
    token_url: http://test.url
    scopes:
    - scope 1
    - scope 2
    endpoint_params:
      param1: value1
      param2: value2`)

	testCases := []struct {
		name              string
		p                 *monitoringv1.Prometheus
		sMons             map[string]*monitoringv1.ServiceMonitor
		pMons             map[string]*monitoringv1.PodMonitor
		probes            map[string]*monitoringv1.Probe
		oauth2Credentials map[string]assets.OAuth2Credentials
		expectedCfg       string
	}{
		{
			name: "service monitor with oauth2",
			p: &monitoringv1.Prometheus{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Spec: monitoringv1.PrometheusSpec{
					OverrideHonorLabels: false,
					ServiceMonitorSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"group": "group1",
						},
					},
				},
			},
			sMons: map[string]*monitoringv1.ServiceMonitor{
				"testservicemonitor1": {
					ObjectMeta: metav1.ObjectMeta{
						Name:      "testservicemonitor1",
						Namespace: "default",
						Labels: map[string]string{
							"group": "group1",
						},
					},
					Spec: monitoringv1.ServiceMonitorSpec{
						Endpoints: []monitoringv1.Endpoint{
							{
								Port:   "web",
								OAuth2: &oauth2,
							},
						},
					},
				},
			},
			oauth2Credentials: map[string]assets.OAuth2Credentials{
				"serviceMonitor/default/testservicemonitor1/0": {
					ClientID:     "test_client_id",
					ClientSecret: "test_client_secret",
				},
			},
			expectedCfg: expectedCfg,
		},
		{
			name: "pod monitor with oauth2",
			p: &monitoringv1.Prometheus{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Spec: monitoringv1.PrometheusSpec{
					OverrideHonorLabels: false,
					ServiceMonitorSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"group": "group1",
						},
					},
				},
			},
			pMons: map[string]*monitoringv1.PodMonitor{
				"testpodmonitor1": {
					ObjectMeta: metav1.ObjectMeta{
						Name:      "testpodmonitor1",
						Namespace: "default",
						Labels: map[string]string{
							"group": "group1",
						},
					},
					Spec: monitoringv1.PodMonitorSpec{
						PodMetricsEndpoints: []monitoringv1.PodMetricsEndpoint{
							{
								Port:   "web",
								OAuth2: &oauth2,
							},
						},
					},
				},
			},
			oauth2Credentials: map[string]assets.OAuth2Credentials{
				"podMonitor/default/testpodmonitor1/0": {
					ClientID:     "test_client_id",
					ClientSecret: "test_client_secret",
				},
			},
			expectedCfg: expectedCfg,
		},
		{
			name: "probe monitor with oauth2",
			p: &monitoringv1.Prometheus{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Spec: monitoringv1.PrometheusSpec{
					OverrideHonorLabels: false,
					ServiceMonitorSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"group": "group1",
						},
					},
				},
			},
			probes: map[string]*monitoringv1.Probe{
				"testprobe1": {
					ObjectMeta: metav1.ObjectMeta{
						Name:      "testprobe1",
						Namespace: "default",
						Labels: map[string]string{
							"group": "group1",
						},
					},
					Spec: monitoringv1.ProbeSpec{
						OAuth2: &oauth2,
						Targets: monitoringv1.ProbeTargets{
							StaticConfig: &monitoringv1.ProbeTargetStaticConfig{
								Targets: []string{"127.0.0.1"},
							},
						},
					},
				},
			},
			oauth2Credentials: map[string]assets.OAuth2Credentials{
				"probe/default/testprobe1": {
					ClientID:     "test_client_id",
					ClientSecret: "test_client_secret",
				},
			},
			expectedCfg: expectedCfg,
		},
	}

	for _, tt := range testCases {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			cg := &ConfigGenerator{}
			cfg, err := cg.GenerateConfig(
				tt.p,
				tt.sMons,
				tt.pMons,
				tt.probes,
				&assets.Store{
					BasicAuthAssets: map[string]assets.BasicAuthCredentials{},
					OAuth2Assets:    tt.oauth2Credentials,
					TokenAssets:     map[string]assets.Token{},
				},
				nil,
				nil,
				nil,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}

			result := string(cfg)

			if !strings.Contains(result, tt.expectedCfg) {
				t.Fatalf("expected Prometheus configuration to contain:\n %s\nFull config:\n %s", tt.expectedCfg, result)
			}
		})
	}
}

func TestPodTargetLabels(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "default",
			},
			Spec: monitoringv1.PrometheusSpec{
				ServiceMonitorSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"group": "group1",
					},
				},
			},
		},
		map[string]*monitoringv1.ServiceMonitor{
			"testservicemonitor1": {
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testservicemonitor1",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.ServiceMonitorSpec{
					PodTargetLabels: []string{"example", "env"},
					Endpoints: []monitoringv1.Endpoint{
						{
							Port:     "web",
							Interval: "30s",
						},
					},
				},
			},
		},
		nil,
		nil,
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: serviceMonitor/default/testservicemonitor1/0
  honor_labels: false
  kubernetes_sd_configs:
  - role: endpoints
    namespaces:
      names:
      - default
  scrape_interval: 30s
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_endpoint_port_name
    regex: web
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Node;(.*)
    replacement: ${1}
    target_label: node
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Pod;(.*)
    replacement: ${1}
    target_label: pod
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: service
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_pod_label_example
    target_label: example
    regex: (.+)
    replacement: ${1}
  - source_labels:
    - __meta_kubernetes_pod_label_env
    target_label: env
    regex: (.+)
    replacement: ${1}
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: job
    replacement: ${1}
  - target_label: endpoint
    replacement: web
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	result := string(cfg)

	if expected != result {
		fmt.Println(pretty.Compare(expected, result))
		t.Fatal("expected Prometheus configuration and actual configuration do not match")
	}
}

func TestPodTargetLabelsFromPodMonitor(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "default",
			},
			Spec: monitoringv1.PrometheusSpec{
				ServiceMonitorSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"group": "group1",
					},
				},
			},
		},
		nil,
		map[string]*monitoringv1.PodMonitor{
			"testpodmonitor1": {
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testpodmonitor1",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.PodMonitorSpec{
					PodTargetLabels: []string{"example", "env"},
					PodMetricsEndpoints: []monitoringv1.PodMetricsEndpoint{
						{
							Port:     "web",
							Interval: "30s",
						},
					},
				},
			},
		},
		nil,
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: podMonitor/default/testpodmonitor1/0
  honor_labels: false
  kubernetes_sd_configs:
  - role: pod
    namespaces:
      names:
      - default
  scrape_interval: 30s
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_pod_container_port_name
    regex: web
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - source_labels:
    - __meta_kubernetes_pod_label_example
    target_label: example
    regex: (.+)
    replacement: ${1}
  - source_labels:
    - __meta_kubernetes_pod_label_env
    target_label: env
    regex: (.+)
    replacement: ${1}
  - target_label: job
    replacement: default/testpodmonitor1
  - target_label: endpoint
    replacement: web
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	result := string(cfg)

	if expected != result {
		fmt.Println(pretty.Compare(expected, result))
		t.Fatal("expected Prometheus configuration and actual configuration do not match")
	}
}

func TestEmptyEndointPorts(t *testing.T) {
	cg := &ConfigGenerator{}
	cfg, err := cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "default",
			},
		},
		map[string]*monitoringv1.ServiceMonitor{
			"test": {
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Spec: monitoringv1.ServiceMonitorSpec{
					Selector: metav1.LabelSelector{
						MatchLabels: map[string]string{
							"foo": "bar",
						},
					},
					Endpoints: []monitoringv1.Endpoint{
						// Add a single endpoint with empty configuration.
						{},
					},
				},
			},
		},
		nil,
		nil,
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	// If this becomes an endless sink of maintenance, then we should just
	// change this to check that just the `bearer_token_file` is set with
	// something like json-path.
	expected := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: serviceMonitor/default/test/0
  honor_labels: false
  kubernetes_sd_configs:
  - role: endpoints
    namespaces:
      names:
      - default
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_service_label_foo
    regex: bar
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Node;(.*)
    replacement: ${1}
    target_label: node
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Pod;(.*)
    replacement: ${1}
    target_label: pod
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: service
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: job
    replacement: ${1}
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	result := string(cfg)
	if expected != result {
		fmt.Println(pretty.Compare(expected, result))
		t.Fatal("expected Prometheus configuration and actual configuration do not match")
	}
}

func generateTestConfig(version string) ([]byte, error) {
	cg := &ConfigGenerator{}
	replicas := int32(1)
	return cg.GenerateConfig(
		&monitoringv1.Prometheus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: "default",
			},
			Spec: monitoringv1.PrometheusSpec{
				Alerting: &monitoringv1.AlertingSpec{
					Alertmanagers: []monitoringv1.AlertmanagerEndpoints{
						{
							Name:      "alertmanager-main",
							Namespace: "default",
							Port:      intstr.FromString("web"),
						},
					},
				},
				ExternalLabels: map[string]string{
					"label1": "value1",
					"label2": "value2",
				},
				Version:  version,
				Replicas: &replicas,
				ServiceMonitorSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"group": "group1",
					},
				},
				PodMonitorSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"group": "group1",
					},
				},
				RuleSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"role": "rulefile",
					},
				},
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceMemory: resource.MustParse("400Mi"),
					},
				},
				RemoteRead: []monitoringv1.RemoteReadSpec{{
					URL: "https://example.com/remote_read",
				}},
				RemoteWrite: []monitoringv1.RemoteWriteSpec{{
					URL: "https://example.com/remote_write",
				}},
			},
		},
		makeServiceMonitors(),
		makePodMonitors(),
		nil,
		&assets.Store{},
		nil,
		nil,
		nil,
		nil,
	)
}

func makeServiceMonitors() map[string]*monitoringv1.ServiceMonitor {
	res := map[string]*monitoringv1.ServiceMonitor{}

	res["servicemonitor1"] = &monitoringv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "testservicemonitor1",
			Namespace: "default",
			Labels: map[string]string{
				"group": "group1",
			},
		},
		Spec: monitoringv1.ServiceMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"group": "group1",
				},
			},
			Endpoints: []monitoringv1.Endpoint{
				{
					Port:     "web",
					Interval: "30s",
				},
			},
		},
	}

	res["servicemonitor2"] = &monitoringv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "testservicemonitor2",
			Namespace: "default",
			Labels: map[string]string{
				"group": "group2",
			},
		},
		Spec: monitoringv1.ServiceMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"group":  "group2",
					"group3": "group3",
				},
			},
			Endpoints: []monitoringv1.Endpoint{
				{
					Port:     "web",
					Interval: "30s",
				},
			},
		},
	}

	res["servicemonitor3"] = &monitoringv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "testservicemonitor3",
			Namespace: "default",
			Labels: map[string]string{
				"group": "group4",
			},
		},
		Spec: monitoringv1.ServiceMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"group":  "group4",
					"group3": "group5",
				},
			},
			Endpoints: []monitoringv1.Endpoint{
				{
					Port:     "web",
					Interval: "30s",
					Path:     "/federate",
					Params:   map[string][]string{"metrics[]": {"{__name__=~\"job:.*\"}"}},
				},
			},
		},
	}

	res["servicemonitor4"] = &monitoringv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "testservicemonitor4",
			Namespace: "default",
			Labels: map[string]string{
				"group": "group6",
			},
		},
		Spec: monitoringv1.ServiceMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"group":  "group6",
					"group3": "group7",
				},
			},
			Endpoints: []monitoringv1.Endpoint{
				{
					Port:     "web",
					Interval: "30s",
					MetricRelabelConfigs: []*monitoringv1.RelabelConfig{
						{
							Action:       "drop",
							Regex:        "my-job-pod-.+",
							SourceLabels: []string{"pod_name"},
						},
						{
							Action:       "drop",
							Regex:        "test",
							SourceLabels: []string{"namespace"},
						},
					},
				},
			},
		},
	}

	res["servicemonitor5"] = &monitoringv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "testservicemonitor4",
			Namespace: "default",
			Labels: map[string]string{
				"group": "group8",
			},
		},
		Spec: monitoringv1.ServiceMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"group":  "group8",
					"group3": "group9",
				},
			},
			Endpoints: []monitoringv1.Endpoint{
				{
					Port:     "web",
					Interval: "30s",
					RelabelConfigs: []*monitoringv1.RelabelConfig{
						{
							Action:       "replace",
							Regex:        "(.*)",
							Replacement:  "$1",
							SourceLabels: []string{"__meta_kubernetes_pod_ready"},
							TargetLabel:  "pod_ready",
						},
						{
							Action:       "replace",
							Regex:        "(.*)",
							Replacement:  "$1",
							SourceLabels: []string{"__meta_kubernetes_pod_node_name"},
							TargetLabel:  "nodename",
						},
					},
				},
			},
		},
	}

	return res
}

func makePodMonitors() map[string]*monitoringv1.PodMonitor {
	res := map[string]*monitoringv1.PodMonitor{}

	res["podmonitor1"] = &monitoringv1.PodMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "testpodmonitor1",
			Namespace: "default",
			Labels: map[string]string{
				"group": "group1",
			},
		},
		Spec: monitoringv1.PodMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"group": "group1",
				},
			},
			PodMetricsEndpoints: []monitoringv1.PodMetricsEndpoint{
				{
					Port:     "web",
					Interval: "30s",
				},
			},
		},
	}

	res["podmonitor2"] = &monitoringv1.PodMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "testpodmonitor2",
			Namespace: "default",
			Labels: map[string]string{
				"group": "group2",
			},
		},
		Spec: monitoringv1.PodMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"group":  "group2",
					"group3": "group3",
				},
			},
			PodMetricsEndpoints: []monitoringv1.PodMetricsEndpoint{
				{
					Port:     "web",
					Interval: "30s",
				},
			},
		},
	}

	res["podmonitor3"] = &monitoringv1.PodMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "testpodmonitor3",
			Namespace: "default",
			Labels: map[string]string{
				"group": "group4",
			},
		},
		Spec: monitoringv1.PodMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"group":  "group4",
					"group3": "group5",
				},
			},
			PodMetricsEndpoints: []monitoringv1.PodMetricsEndpoint{
				{
					Port:     "web",
					Interval: "30s",
					Path:     "/federate",
					Params:   map[string][]string{"metrics[]": {"{__name__=~\"job:.*\"}"}},
				},
			},
		},
	}

	res["podmonitor4"] = &monitoringv1.PodMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "testpodmonitor4",
			Namespace: "default",
			Labels: map[string]string{
				"group": "group6",
			},
		},
		Spec: monitoringv1.PodMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"group":  "group6",
					"group3": "group7",
				},
			},
			PodMetricsEndpoints: []monitoringv1.PodMetricsEndpoint{
				{
					Port:     "web",
					Interval: "30s",
					MetricRelabelConfigs: []*monitoringv1.RelabelConfig{
						{
							Action:       "drop",
							Regex:        "my-job-pod-.+",
							SourceLabels: []string{"pod_name"},
						},
						{
							Action:       "drop",
							Regex:        "test",
							SourceLabels: []string{"namespace"},
						},
					},
				},
			},
		},
	}

	res["podmonitor5"] = &monitoringv1.PodMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "testpodmonitor4",
			Namespace: "default",
			Labels: map[string]string{
				"group": "group8",
			},
		},
		Spec: monitoringv1.PodMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"group":  "group8",
					"group3": "group9",
				},
			},
			PodMetricsEndpoints: []monitoringv1.PodMetricsEndpoint{
				{
					Port:     "web",
					Interval: "30s",
					RelabelConfigs: []*monitoringv1.RelabelConfig{
						{
							Action:       "replace",
							Regex:        "(.*)",
							Replacement:  "$1",
							SourceLabels: []string{"__meta_kubernetes_pod_ready"},
							TargetLabel:  "pod_ready",
						},
						{
							Action:       "replace",
							Regex:        "(.*)",
							Replacement:  "$1",
							SourceLabels: []string{"__meta_kubernetes_pod_node_name"},
							TargetLabel:  "nodename",
						},
					},
				},
			},
		},
	}

	return res
}

func TestHonorLabels(t *testing.T) {
	type testCase struct {
		UserHonorLabels     bool
		OverrideHonorLabels bool
		Expected            bool
	}

	testCases := []testCase{
		{
			UserHonorLabels:     false,
			OverrideHonorLabels: true,
			Expected:            false,
		},
		{
			UserHonorLabels:     true,
			OverrideHonorLabels: false,
			Expected:            true,
		},
		{
			UserHonorLabels:     true,
			OverrideHonorLabels: true,
			Expected:            false,
		},
		{
			UserHonorLabels:     false,
			OverrideHonorLabels: false,
			Expected:            false,
		},
	}

	for _, tc := range testCases {
		hl := honorLabels(tc.UserHonorLabels, tc.OverrideHonorLabels)
		if tc.Expected != hl {
			t.Fatalf("\nGot: %t, \nExpected: %t\nFor values UserHonorLabels %t, OverrideHonorLabels %t\n", hl, tc.Expected, tc.UserHonorLabels, tc.OverrideHonorLabels)
		}
	}
}

func TestHonorTimestamps(t *testing.T) {
	type testCase struct {
		UserHonorTimestamps     *bool
		OverrideHonorTimestamps bool
		Expected                string
	}

	testCases := []testCase{
		{
			UserHonorTimestamps:     nil,
			OverrideHonorTimestamps: true,
			Expected:                "honor_timestamps: false\n",
		},
		{
			UserHonorTimestamps:     nil,
			OverrideHonorTimestamps: false,
			Expected:                "{}\n",
		},
		{
			UserHonorTimestamps:     swag.Bool(false),
			OverrideHonorTimestamps: true,
			Expected:                "honor_timestamps: false\n",
		},
		{
			UserHonorTimestamps:     swag.Bool(false),
			OverrideHonorTimestamps: false,
			Expected:                "honor_timestamps: false\n",
		},
		{
			UserHonorTimestamps:     swag.Bool(true),
			OverrideHonorTimestamps: true,
			Expected:                "honor_timestamps: false\n",
		},
		{
			UserHonorTimestamps:     swag.Bool(true),
			OverrideHonorTimestamps: false,
			Expected:                "honor_timestamps: true\n",
		},
	}

	for _, tc := range testCases {
		hl, _ := yaml.Marshal(honorTimestamps(yaml.MapSlice{}, tc.UserHonorTimestamps, tc.OverrideHonorTimestamps))
		cfg := string(hl)
		if tc.Expected != cfg {
			t.Fatalf("\nGot: %s, \nExpected: %s\nFor values UserHonorTimestamps %+v, OverrideHonorTimestamps %t\n", cfg, tc.Expected, tc.UserHonorTimestamps, tc.OverrideHonorTimestamps)
		}
	}
}

func TestSampleLimits(t *testing.T) {
	expectNoLimit := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: serviceMonitor/default/testservicemonitor1/0
  honor_labels: false
  kubernetes_sd_configs:
  - role: endpoints
    namespaces:
      names:
      - default
  scrape_interval: 30s
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_endpoint_port_name
    regex: web
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Node;(.*)
    replacement: ${1}
    target_label: node
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Pod;(.*)
    replacement: ${1}
    target_label: pod
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: service
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: job
    replacement: ${1}
  - target_label: endpoint
    replacement: web
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	expectLimit := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: serviceMonitor/default/testservicemonitor1/0
  honor_labels: false
  kubernetes_sd_configs:
  - role: endpoints
    namespaces:
      names:
      - default
  scrape_interval: 30s
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_endpoint_port_name
    regex: web
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Node;(.*)
    replacement: ${1}
    target_label: node
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Pod;(.*)
    replacement: ${1}
    target_label: pod
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: service
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: job
    replacement: ${1}
  - target_label: endpoint
    replacement: web
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  sample_limit: %d
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	for _, tc := range []struct {
		enforcedLimit int
		limit         int
		expected      string
	}{
		{
			enforcedLimit: -1,
			limit:         -1,
			expected:      expectNoLimit,
		},
		{
			enforcedLimit: 1000,
			limit:         -1,
			expected:      fmt.Sprintf(expectLimit, 1000),
		},
		{
			enforcedLimit: 1000,
			limit:         2000,
			expected:      fmt.Sprintf(expectLimit, 1000),
		},
		{
			enforcedLimit: 1000,
			limit:         500,
			expected:      fmt.Sprintf(expectLimit, 500),
		},
	} {
		t.Run(fmt.Sprintf("enforcedlimit(%d) limit(%d)", tc.enforcedLimit, tc.limit), func(t *testing.T) {
			cg := &ConfigGenerator{}

			prometheus := monitoringv1.Prometheus{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Spec: monitoringv1.PrometheusSpec{
					Version: "v2.20.0",
					ServiceMonitorSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"group": "group1",
						},
					},
				},
			}
			if tc.enforcedLimit >= 0 {
				i := uint64(tc.enforcedLimit)
				prometheus.Spec.EnforcedSampleLimit = &i
			}

			serviceMonitor := monitoringv1.ServiceMonitor{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testservicemonitor1",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.ServiceMonitorSpec{
					Endpoints: []monitoringv1.Endpoint{
						{
							Port:     "web",
							Interval: "30s",
						},
					},
				},
			}
			if tc.limit >= 0 {
				serviceMonitor.Spec.SampleLimit = uint64(tc.limit)
			}

			cfg, err := cg.GenerateConfig(
				&prometheus,
				map[string]*monitoringv1.ServiceMonitor{
					"testservicemonitor1": &serviceMonitor,
				},
				nil,
				nil,
				&assets.Store{},
				nil,
				nil,
				nil,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}

			result := string(cfg)
			if tc.expected != result {
				t.Logf("\n%s", pretty.Compare(tc.expected, result))
				t.Fatal("expected Prometheus configuration and actual configuration do not match")
			}
		})
	}
}

func TestTargetLimits(t *testing.T) {
	expectNoLimit := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: serviceMonitor/default/testservicemonitor1/0
  honor_labels: false
  kubernetes_sd_configs:
  - role: endpoints
    namespaces:
      names:
      - default
  scrape_interval: 30s
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_endpoint_port_name
    regex: web
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Node;(.*)
    replacement: ${1}
    target_label: node
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Pod;(.*)
    replacement: ${1}
    target_label: pod
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: service
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: job
    replacement: ${1}
  - target_label: endpoint
    replacement: web
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	expectLimit := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: serviceMonitor/default/testservicemonitor1/0
  honor_labels: false
  kubernetes_sd_configs:
  - role: endpoints
    namespaces:
      names:
      - default
  scrape_interval: 30s
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_endpoint_port_name
    regex: web
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Node;(.*)
    replacement: ${1}
    target_label: node
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Pod;(.*)
    replacement: ${1}
    target_label: pod
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: service
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: job
    replacement: ${1}
  - target_label: endpoint
    replacement: web
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  target_limit: %d
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	for _, tc := range []struct {
		version       string
		enforcedLimit int
		limit         int
		expected      string
	}{
		{
			version:       "v2.15.0",
			enforcedLimit: -1,
			limit:         -1,
			expected:      expectNoLimit,
		},
		{
			version:       "v2.21.0",
			enforcedLimit: -1,
			limit:         -1,
			expected:      expectNoLimit,
		},
		{
			version:       "v2.15.0",
			enforcedLimit: 1000,
			limit:         -1,
			expected:      expectNoLimit,
		},
		{
			version:       "v2.21.0",
			enforcedLimit: 1000,
			limit:         -1,
			expected:      fmt.Sprintf(expectLimit, 1000),
		},
		{
			version:       "v2.15.0",
			enforcedLimit: 1000,
			limit:         2000,
			expected:      expectNoLimit,
		},
		{
			version:       "v2.21.0",
			enforcedLimit: 1000,
			limit:         2000,
			expected:      fmt.Sprintf(expectLimit, 1000),
		},
		{
			version:       "v2.15.0",
			enforcedLimit: 1000,
			limit:         500,
			expected:      expectNoLimit,
		},
		{
			version:       "v2.21.0",
			enforcedLimit: 1000,
			limit:         500,
			expected:      fmt.Sprintf(expectLimit, 500),
		},
	} {
		t.Run(fmt.Sprintf("%s enforcedlimit(%d) limit(%d)", tc.version, tc.enforcedLimit, tc.limit), func(t *testing.T) {
			cg := NewConfigGenerator(log.NewLogfmtLogger(os.Stdout))

			prometheus := monitoringv1.Prometheus{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Spec: monitoringv1.PrometheusSpec{
					Version: tc.version,
					ServiceMonitorSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"group": "group1",
						},
					},
				},
			}
			if tc.enforcedLimit >= 0 {
				i := uint64(tc.enforcedLimit)
				prometheus.Spec.EnforcedTargetLimit = &i
			}

			serviceMonitor := monitoringv1.ServiceMonitor{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testservicemonitor1",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.ServiceMonitorSpec{
					Endpoints: []monitoringv1.Endpoint{
						{
							Port:     "web",
							Interval: "30s",
						},
					},
				},
			}
			if tc.limit >= 0 {
				serviceMonitor.Spec.TargetLimit = uint64(tc.limit)
			}

			cfg, err := cg.GenerateConfig(
				&prometheus,
				map[string]*monitoringv1.ServiceMonitor{
					"testservicemonitor1": &serviceMonitor,
				},
				nil,
				nil,
				&assets.Store{},
				nil,
				nil,
				nil,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}

			result := string(cfg)
			if tc.expected != result {
				t.Logf("\n%s", pretty.Compare(tc.expected, result))
				t.Fatal("expected Prometheus configuration and actual configuration do not match")
			}
		})
	}
}

func TestRemoteReadConfig(t *testing.T) {
	for _, tc := range []struct {
		version    string
		remoteRead monitoringv1.RemoteReadSpec

		expected string
	}{
		{
			version: "v2.27.1",
			remoteRead: monitoringv1.RemoteReadSpec{
				URL: "http://example.com",
				OAuth2: &monitoringv1.OAuth2{
					TokenURL:       "http://token-url",
					Scopes:         []string{"scope1"},
					EndpointParams: map[string]string{"param": "value"},
				},
			},
			expected: `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
remote_read:
- url: http://example.com
  remote_timeout: 30s
  oauth2:
    client_id: client-id
    client_secret: client-secret
    token_url: http://token-url
    scopes:
    - scope1
    endpoint_params:
      param: value
`,
		},
		{
			version: "v2.26.0",
			remoteRead: monitoringv1.RemoteReadSpec{
				URL: "http://example.com",
				OAuth2: &monitoringv1.OAuth2{
					TokenURL:       "http://token-url",
					Scopes:         []string{"scope1"},
					EndpointParams: map[string]string{"param": "value"},
				},
			},
			expected: `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
remote_read:
- url: http://example.com
  remote_timeout: 30s
`,
		},
		{
			version: "v2.26.0",
			remoteRead: monitoringv1.RemoteReadSpec{
				URL: "http://example.com",
				Authorization: &monitoringv1.Authorization{
					SafeAuthorization: monitoringv1.SafeAuthorization{
						Credentials: &v1.SecretKeySelector{
							LocalObjectReference: v1.LocalObjectReference{
								Name: "key"},
						},
					},
				},
			},
			expected: `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
remote_read:
- url: http://example.com
  remote_timeout: 30s
  authorization:
    type: Bearer
    credentials: secret
`,
		},
	} {
		t.Run(fmt.Sprintf("version=%s", tc.version), func(t *testing.T) {
			cg := &ConfigGenerator{}

			prometheus := monitoringv1.Prometheus{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Spec: monitoringv1.PrometheusSpec{
					Version: tc.version,
					ServiceMonitorSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"group": "group1",
						},
					},
					RemoteRead: []monitoringv1.RemoteReadSpec{tc.remoteRead},
				},
			}

			cfg, err := cg.GenerateConfig(
				&prometheus,
				nil,
				nil,
				nil,
				&assets.Store{
					BasicAuthAssets: map[string]assets.BasicAuthCredentials{},
					OAuth2Assets: map[string]assets.OAuth2Credentials{
						"remoteRead/0": {
							ClientID:     "client-id",
							ClientSecret: "client-secret",
						},
					},
					TokenAssets: map[string]assets.Token{
						"remoteRead/auth/0": assets.Token("secret"),
					}},
				nil,
				nil,
				nil,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}

			result := string(cfg)
			if tc.expected != result {
				t.Logf("\n%s", pretty.Compare(tc.expected, result))
				t.Fatal("expected Prometheus configuration and actual configuration do not match")
			}
		})
	}
}

func TestRemoteWriteConfig(t *testing.T) {
	for _, tc := range []struct {
		version     string
		remoteWrite monitoringv1.RemoteWriteSpec

		expected string
	}{
		{
			version: "v2.22.0",
			remoteWrite: monitoringv1.RemoteWriteSpec{
				URL: "http://example.com",
				QueueConfig: &monitoringv1.QueueConfig{
					Capacity:          1000,
					MinShards:         1,
					MaxShards:         10,
					MaxSamplesPerSend: 100,
					BatchSendDeadline: "20s",
					MaxRetries:        3,
					MinBackoff:        "1s",
					MaxBackoff:        "10s",
				},
				MetadataConfig: &monitoringv1.MetadataConfig{
					Send:         false,
					SendInterval: "1m",
				},
			},
			expected: `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
remote_write:
- url: http://example.com
  remote_timeout: 30s
  queue_config:
    capacity: 1000
    min_shards: 1
    max_shards: 10
    max_samples_per_send: 100
    batch_send_deadline: 20s
    min_backoff: 1s
    max_backoff: 10s
`,
		},
		{
			version: "v2.23.0",
			remoteWrite: monitoringv1.RemoteWriteSpec{
				URL: "http://example.com",
				QueueConfig: &monitoringv1.QueueConfig{
					Capacity:          1000,
					MinShards:         1,
					MaxShards:         10,
					MaxSamplesPerSend: 100,
					BatchSendDeadline: "20s",
					MaxRetries:        3,
					MinBackoff:        "1s",
					MaxBackoff:        "10s",
				},
				MetadataConfig: &monitoringv1.MetadataConfig{
					Send:         false,
					SendInterval: "1m",
				},
			},
			expected: `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
remote_write:
- url: http://example.com
  remote_timeout: 30s
  queue_config:
    capacity: 1000
    min_shards: 1
    max_shards: 10
    max_samples_per_send: 100
    batch_send_deadline: 20s
    min_backoff: 1s
    max_backoff: 10s
  metadata_config:
    send: false
    send_interval: 1m
`,
		},
		{
			version: "v2.23.0",
			remoteWrite: monitoringv1.RemoteWriteSpec{
				URL: "http://example.com",
				QueueConfig: &monitoringv1.QueueConfig{
					Capacity:          1000,
					MinShards:         1,
					MaxShards:         10,
					MaxSamplesPerSend: 100,
					BatchSendDeadline: "20s",
					MinBackoff:        "1s",
					MaxBackoff:        "10s",
				},
				MetadataConfig: &monitoringv1.MetadataConfig{
					Send:         false,
					SendInterval: "1m",
				},
			},
			expected: `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
remote_write:
- url: http://example.com
  remote_timeout: 30s
  queue_config:
    capacity: 1000
    min_shards: 1
    max_shards: 10
    max_samples_per_send: 100
    batch_send_deadline: 20s
    min_backoff: 1s
    max_backoff: 10s
  metadata_config:
    send: false
    send_interval: 1m
`,
		},
		{
			version: "v2.10.0",
			remoteWrite: monitoringv1.RemoteWriteSpec{
				URL: "http://example.com",
				QueueConfig: &monitoringv1.QueueConfig{
					Capacity:          1000,
					MinShards:         1,
					MaxShards:         10,
					MaxSamplesPerSend: 100,
					BatchSendDeadline: "20s",
					MaxRetries:        3,
					MinBackoff:        "1s",
					MaxBackoff:        "10s",
				},
				MetadataConfig: &monitoringv1.MetadataConfig{
					Send:         false,
					SendInterval: "1m",
				},
			},
			expected: `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
remote_write:
- url: http://example.com
  remote_timeout: 30s
  queue_config:
    capacity: 1000
    min_shards: 1
    max_shards: 10
    max_samples_per_send: 100
    batch_send_deadline: 20s
    max_retries: 3
    min_backoff: 1s
    max_backoff: 10s
`,
		},
		{
			version: "v2.27.1",
			remoteWrite: monitoringv1.RemoteWriteSpec{
				URL: "http://example.com",
				OAuth2: &monitoringv1.OAuth2{
					TokenURL:       "http://token-url",
					Scopes:         []string{"scope1"},
					EndpointParams: map[string]string{"param": "value"},
				},
			},
			expected: `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
remote_write:
- url: http://example.com
  remote_timeout: 30s
  oauth2:
    client_id: client-id
    client_secret: client-secret
    token_url: http://token-url
    scopes:
    - scope1
    endpoint_params:
      param: value
`,
		},
		{
			version: "v2.26.0",
			remoteWrite: monitoringv1.RemoteWriteSpec{
				URL: "http://example.com",
				Authorization: &monitoringv1.Authorization{
					SafeAuthorization: monitoringv1.SafeAuthorization{
						Credentials: &v1.SecretKeySelector{
							LocalObjectReference: v1.LocalObjectReference{
								Name: "key"},
						},
					},
				},
			},
			expected: `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
remote_write:
- url: http://example.com
  remote_timeout: 30s
  authorization:
    type: Bearer
    credentials: secret
`,
		},
	} {
		t.Run(fmt.Sprintf("version=%s", tc.version), func(t *testing.T) {
			cg := &ConfigGenerator{}

			prometheus := monitoringv1.Prometheus{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Spec: monitoringv1.PrometheusSpec{
					Version: tc.version,
					ServiceMonitorSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"group": "group1",
						},
					},
					RemoteWrite: []monitoringv1.RemoteWriteSpec{tc.remoteWrite},
				},
			}

			cfg, err := cg.GenerateConfig(
				&prometheus,
				nil,
				nil,
				nil,
				&assets.Store{
					BasicAuthAssets: map[string]assets.BasicAuthCredentials{},
					OAuth2Assets: map[string]assets.OAuth2Credentials{
						"remoteWrite/0": {
							ClientID:     "client-id",
							ClientSecret: "client-secret",
						},
					},
					TokenAssets: map[string]assets.Token{
						"remoteWrite/auth/0": assets.Token("secret"),
					}},
				nil,
				nil,
				nil,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}

			result := string(cfg)
			if tc.expected != result {
				t.Logf("\n%s", pretty.Compare(tc.expected, result))
				t.Fatal("expected Prometheus configuration and actual configuration do not match")
			}
		})
	}
}

func TestLabelLimits(t *testing.T) {
	expectNoLimit := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: serviceMonitor/default/testservicemonitor1/0
  honor_labels: false
  kubernetes_sd_configs:
  - role: endpoints
    namespaces:
      names:
      - default
  scrape_interval: 30s
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_endpoint_port_name
    regex: web
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Node;(.*)
    replacement: ${1}
    target_label: node
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Pod;(.*)
    replacement: ${1}
    target_label: pod
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: service
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: job
    replacement: ${1}
  - target_label: endpoint
    replacement: web
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	expectLimit := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: serviceMonitor/default/testservicemonitor1/0
  honor_labels: false
  kubernetes_sd_configs:
  - role: endpoints
    namespaces:
      names:
      - default
  scrape_interval: 30s
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_endpoint_port_name
    regex: web
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Node;(.*)
    replacement: ${1}
    target_label: node
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Pod;(.*)
    replacement: ${1}
    target_label: pod
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: service
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: job
    replacement: ${1}
  - target_label: endpoint
    replacement: web
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  label_limit: %d
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	for _, tc := range []struct {
		version            string
		enforcedLabelLimit int
		labelLimit         int
		expected           string
	}{
		{
			version:            "v2.26.0",
			enforcedLabelLimit: -1,
			labelLimit:         -1,
			expected:           expectNoLimit,
		},
		{
			version:            "v2.27.0",
			enforcedLabelLimit: -1,
			labelLimit:         -1,
			expected:           expectNoLimit,
		},
		{
			version:            "v2.26.0",
			enforcedLabelLimit: 1000,
			labelLimit:         -1,
			expected:           expectNoLimit,
		},
		{
			version:            "v2.27.0",
			enforcedLabelLimit: 1000,
			labelLimit:         -1,
			expected:           fmt.Sprintf(expectLimit, 1000),
		},
		{
			version:            "v2.26.0",
			enforcedLabelLimit: 1000,
			labelLimit:         2000,
			expected:           expectNoLimit,
		},
		{
			version:            "v2.27.0",
			enforcedLabelLimit: 1000,
			labelLimit:         2000,
			expected:           fmt.Sprintf(expectLimit, 1000),
		},
		{
			version:            "v2.26.0",
			enforcedLabelLimit: 1000,
			labelLimit:         500,
			expected:           expectNoLimit,
		},
		{
			version:            "v2.27.0",
			enforcedLabelLimit: 1000,
			labelLimit:         500,
			expected:           fmt.Sprintf(expectLimit, 500),
		},
	} {
		t.Run(fmt.Sprintf("%s enforcedLabelLimit(%d) labelLimit(%d)", tc.version, tc.enforcedLabelLimit, tc.labelLimit), func(t *testing.T) {
			cg := NewConfigGenerator(log.NewLogfmtLogger(os.Stdout))

			prometheus := monitoringv1.Prometheus{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Spec: monitoringv1.PrometheusSpec{
					Version: tc.version,
					ServiceMonitorSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"group": "group1",
						},
					},
				},
			}
			if tc.enforcedLabelLimit >= 0 {
				i := uint64(tc.enforcedLabelLimit)
				prometheus.Spec.EnforcedLabelLimit = &i
			}

			serviceMonitor := monitoringv1.ServiceMonitor{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testservicemonitor1",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.ServiceMonitorSpec{
					Endpoints: []monitoringv1.Endpoint{
						{
							Port:     "web",
							Interval: "30s",
						},
					},
				},
			}
			if tc.labelLimit >= 0 {
				serviceMonitor.Spec.LabelLimit = uint64(tc.labelLimit)
			}

			cfg, err := cg.GenerateConfig(
				&prometheus,
				map[string]*monitoringv1.ServiceMonitor{
					"testservicemonitor1": &serviceMonitor,
				},
				nil,
				nil,
				&assets.Store{},
				nil,
				nil,
				nil,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}

			result := string(cfg)
			if tc.expected != result {
				t.Logf("\n%s", pretty.Compare(tc.expected, result))
				t.Fatal("expected Prometheus configuration and actual configuration do not match")
			}
		})
	}
}

func TestLabelNameLengthLimits(t *testing.T) {
	expectNoLimit := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: podMonitor/default/testpodmonitor1/0
  honor_labels: false
  kubernetes_sd_configs:
  - role: pod
    namespaces:
      names:
      - default
  scrape_interval: 30s
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_pod_container_port_name
    regex: web
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - target_label: job
    replacement: default/testpodmonitor1
  - target_label: endpoint
    replacement: web
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	expectLimit := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: podMonitor/default/testpodmonitor1/0
  honor_labels: false
  kubernetes_sd_configs:
  - role: pod
    namespaces:
      names:
      - default
  scrape_interval: 30s
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_pod_container_port_name
    regex: web
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - target_label: job
    replacement: default/testpodmonitor1
  - target_label: endpoint
    replacement: web
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  label_name_length_limit: %d
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	for _, tc := range []struct {
		version                      string
		enforcedLabelNameLengthLimit int
		labelNameLengthLimit         int
		expected                     string
	}{
		{
			version:                      "v2.26.0",
			enforcedLabelNameLengthLimit: -1,
			labelNameLengthLimit:         -1,
			expected:                     expectNoLimit,
		},
		{
			version:                      "v2.27.0",
			enforcedLabelNameLengthLimit: -1,
			labelNameLengthLimit:         -1,
			expected:                     expectNoLimit,
		},
		{
			version:                      "v2.26.0",
			enforcedLabelNameLengthLimit: 1000,
			labelNameLengthLimit:         -1,
			expected:                     expectNoLimit,
		},
		{
			version:                      "v2.27.0",
			enforcedLabelNameLengthLimit: 1000,
			labelNameLengthLimit:         -1,
			expected:                     fmt.Sprintf(expectLimit, 1000),
		},
		{
			version:                      "v2.26.0",
			enforcedLabelNameLengthLimit: 1000,
			labelNameLengthLimit:         2000,
			expected:                     expectNoLimit,
		},
		{
			version:                      "v2.27.0",
			enforcedLabelNameLengthLimit: 1000,
			labelNameLengthLimit:         2000,
			expected:                     fmt.Sprintf(expectLimit, 1000),
		},
		{
			version:                      "v2.26.0",
			enforcedLabelNameLengthLimit: 1000,
			labelNameLengthLimit:         500,
			expected:                     expectNoLimit,
		},
		{
			version:                      "v2.27.0",
			enforcedLabelNameLengthLimit: 1000,
			labelNameLengthLimit:         500,
			expected:                     fmt.Sprintf(expectLimit, 500),
		},
	} {
		t.Run(fmt.Sprintf("%s enforcedLabelNameLengthLimit(%d) labelNameLengthLimit(%d)", tc.version, tc.enforcedLabelNameLengthLimit, tc.labelNameLengthLimit), func(t *testing.T) {
			cg := NewConfigGenerator(log.NewLogfmtLogger(os.Stdout))

			prometheus := monitoringv1.Prometheus{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Spec: monitoringv1.PrometheusSpec{
					Version: tc.version,
					ServiceMonitorSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"group": "group1",
						},
					},
				},
			}
			if tc.enforcedLabelNameLengthLimit >= 0 {
				i := uint64(tc.enforcedLabelNameLengthLimit)
				prometheus.Spec.EnforcedLabelNameLengthLimit = &i
			}

			podMonitor := monitoringv1.PodMonitor{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testpodmonitor1",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.PodMonitorSpec{
					PodMetricsEndpoints: []monitoringv1.PodMetricsEndpoint{
						{
							Port:     "web",
							Interval: "30s",
						},
					},
				},
			}
			if tc.labelNameLengthLimit >= 0 {
				podMonitor.Spec.LabelNameLengthLimit = uint64(tc.labelNameLengthLimit)
			}

			cfg, err := cg.GenerateConfig(
				&prometheus,
				nil,
				map[string]*monitoringv1.PodMonitor{
					"testpodmonitor1": &podMonitor,
				},
				nil,
				&assets.Store{},
				nil,
				nil,
				nil,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}

			result := string(cfg)
			if tc.expected != result {
				t.Logf("\n%s", pretty.Compare(tc.expected, result))
				t.Fatal("expected Prometheus configuration and actual configuration do not match")
			}
		})
	}
}

func TestLabelValueLengthLimits(t *testing.T) {
	expectNoLimit := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: probe/default/testprobe1
  honor_timestamps: true
  metrics_path: /probe
  scheme: http
  proxy_url: socks://myproxy:9095
  params:
    module:
    - http_2xx
  static_configs:
  - targets:
    - prometheus.io
    - promcon.io
    labels:
      namespace: default
      static: label
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - source_labels:
    - __address__
    target_label: __param_target
  - source_labels:
    - __param_target
    target_label: instance
  - target_label: __address__
    replacement: blackbox.exporter.io
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	expectLimit := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: probe/default/testprobe1
  honor_timestamps: true
  metrics_path: /probe
  scheme: http
  proxy_url: socks://myproxy:9095
  params:
    module:
    - http_2xx
  label_value_length_limit: %d
  static_configs:
  - targets:
    - prometheus.io
    - promcon.io
    labels:
      namespace: default
      static: label
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - source_labels:
    - __address__
    target_label: __param_target
  - source_labels:
    - __param_target
    target_label: instance
  - target_label: __address__
    replacement: blackbox.exporter.io
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	for _, tc := range []struct {
		version                       string
		enforcedLabelValueLengthLimit int
		labelValueLengthLimit         int
		expected                      string
	}{
		{
			version:                       "v2.26.0",
			enforcedLabelValueLengthLimit: -1,
			labelValueLengthLimit:         -1,
			expected:                      expectNoLimit,
		},
		{
			version:                       "v2.27.0",
			enforcedLabelValueLengthLimit: -1,
			labelValueLengthLimit:         -1,
			expected:                      expectNoLimit,
		},
		{
			version:                       "v2.26.0",
			enforcedLabelValueLengthLimit: 1000,
			labelValueLengthLimit:         -1,
			expected:                      expectNoLimit,
		},
		{
			version:                       "v2.27.0",
			enforcedLabelValueLengthLimit: 1000,
			labelValueLengthLimit:         -1,
			expected:                      fmt.Sprintf(expectLimit, 1000),
		},
		{
			version:                       "v2.26.0",
			enforcedLabelValueLengthLimit: 1000,
			labelValueLengthLimit:         2000,
			expected:                      expectNoLimit,
		},
		{
			version:                       "v2.27.0",
			enforcedLabelValueLengthLimit: 1000,
			labelValueLengthLimit:         2000,
			expected:                      fmt.Sprintf(expectLimit, 1000),
		},
		{
			version:                       "v2.26.0",
			enforcedLabelValueLengthLimit: 1000,
			labelValueLengthLimit:         500,
			expected:                      expectNoLimit,
		},
		{
			version:                       "v2.27.0",
			enforcedLabelValueLengthLimit: 1000,
			labelValueLengthLimit:         500,
			expected:                      fmt.Sprintf(expectLimit, 500),
		},
	} {
		t.Run(fmt.Sprintf("%s enforcedLabelValueLengthLimit(%d) labelValueLengthLimit(%d)", tc.version, tc.enforcedLabelValueLengthLimit, tc.labelValueLengthLimit), func(t *testing.T) {
			cg := NewConfigGenerator(log.NewLogfmtLogger(os.Stdout))

			prometheus := monitoringv1.Prometheus{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Spec: monitoringv1.PrometheusSpec{
					Version: tc.version,
					ServiceMonitorSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"group": "group1",
						},
					},
				},
			}
			if tc.enforcedLabelValueLengthLimit >= 0 {
				i := uint64(tc.enforcedLabelValueLengthLimit)
				prometheus.Spec.EnforcedLabelValueLengthLimit = &i
			}

			probe := monitoringv1.Probe{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testprobe1",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.ProbeSpec{
					ProberSpec: monitoringv1.ProberSpec{
						Scheme:   "http",
						URL:      "blackbox.exporter.io",
						Path:     "/probe",
						ProxyURL: "socks://myproxy:9095",
					},
					Module: "http_2xx",
					Targets: monitoringv1.ProbeTargets{
						StaticConfig: &monitoringv1.ProbeTargetStaticConfig{
							Targets: []string{
								"prometheus.io",
								"promcon.io",
							},
							Labels: map[string]string{
								"static": "label",
							},
						},
					},
				},
			}
			if tc.labelValueLengthLimit >= 0 {
				probe.Spec.LabelValueLengthLimit = uint64(tc.labelValueLengthLimit)
			}

			cfg, err := cg.GenerateConfig(
				&prometheus,
				nil,
				nil,
				map[string]*monitoringv1.Probe{
					"testprobe1": &probe,
				},
				&assets.Store{},
				nil,
				nil,
				nil,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}

			result := string(cfg)
			if tc.expected != result {
				t.Logf("\n%s", pretty.Compare(tc.expected, result))
				t.Fatal("expected Prometheus configuration and actual configuration do not match")
			}
		})
	}
}

func TestBodySizeLimits(t *testing.T) {
	expectNoLimit := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: serviceMonitor/default/testservicemonitor1/0
  honor_labels: false
  kubernetes_sd_configs:
  - role: endpoints
    namespaces:
      names:
      - default
  scrape_interval: 30s
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_endpoint_port_name
    regex: web
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Node;(.*)
    replacement: ${1}
    target_label: node
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Pod;(.*)
    replacement: ${1}
    target_label: pod
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: service
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: job
    replacement: ${1}
  - target_label: endpoint
    replacement: web
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	expectLimit := `global:
  evaluation_interval: 30s
  scrape_interval: 30s
  external_labels:
    prometheus: default/test
    prometheus_replica: $(POD_NAME)
rule_files: []
scrape_configs:
- job_name: serviceMonitor/default/testservicemonitor1/0
  honor_labels: false
  kubernetes_sd_configs:
  - role: endpoints
    namespaces:
      names:
      - default
  scrape_interval: 30s
  relabel_configs:
  - source_labels:
    - job
    target_label: __tmp_prometheus_job_name
  - action: keep
    source_labels:
    - __meta_kubernetes_endpoint_port_name
    regex: web
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Node;(.*)
    replacement: ${1}
    target_label: node
  - source_labels:
    - __meta_kubernetes_endpoint_address_target_kind
    - __meta_kubernetes_endpoint_address_target_name
    separator: ;
    regex: Pod;(.*)
    replacement: ${1}
    target_label: pod
  - source_labels:
    - __meta_kubernetes_namespace
    target_label: namespace
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: service
  - source_labels:
    - __meta_kubernetes_pod_name
    target_label: pod
  - source_labels:
    - __meta_kubernetes_pod_container_name
    target_label: container
  - source_labels:
    - __meta_kubernetes_service_name
    target_label: job
    replacement: ${1}
  - target_label: endpoint
    replacement: web
  - source_labels:
    - __address__
    target_label: __tmp_hash
    modulus: 1
    action: hashmod
  - source_labels:
    - __tmp_hash
    regex: $(SHARD)
    action: keep
  body_size_limit: %s
  metric_relabel_configs: []
alerting:
  alert_relabel_configs:
  - action: labeldrop
    regex: prometheus_replica
  alertmanagers: []
`

	for _, tc := range []struct {
		version               string
		enforcedBodySizeLimit string
		expected              string
		expectedErr           error
	}{
		{
			version:               "v2.27.0",
			enforcedBodySizeLimit: "1000MB",
			expected:              expectNoLimit,
		},
		{
			version:               "v2.28.0",
			enforcedBodySizeLimit: "1000MB",
			expected:              fmt.Sprintf(expectLimit, "1000MB"),
		},
		{
			version:               "v2.28.0",
			enforcedBodySizeLimit: "",
			expected:              expectNoLimit,
		},
		{
			version:               "v2.28.0",
			enforcedBodySizeLimit: "100",
			expectedErr:           errors.New("invalid enforcedBodySizeLimit value specified: units: unknown unit  in 100"),
		},
		{
			version:               "v2.28.0",
			enforcedBodySizeLimit: "200kb",
			expectedErr:           errors.New("invalid enforcedBodySizeLimit value specified: units: unknown unit kb in 200kb"),
		},
		{
			version:               "v2.28.0",
			enforcedBodySizeLimit: "300 MB",
			expectedErr:           errors.New("invalid enforcedBodySizeLimit value specified: units: unknown unit  MB in 300 MB"),
		},
		{
			version:               "v2.28.0",
			enforcedBodySizeLimit: "150M",
			expectedErr:           errors.New("invalid enforcedBodySizeLimit value specified: units: unknown unit M in 150M"),
		},
	} {
		t.Run(fmt.Sprintf("%s enforcedBodySizeLimit(%s)", tc.version, tc.enforcedBodySizeLimit), func(t *testing.T) {
			cg := NewConfigGenerator(log.NewLogfmtLogger(os.Stdout))

			prometheus := monitoringv1.Prometheus{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Spec: monitoringv1.PrometheusSpec{
					Version: tc.version,
					ServiceMonitorSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"group": "group1",
						},
					},
				},
			}
			if tc.enforcedBodySizeLimit != "" {
				i := tc.enforcedBodySizeLimit
				prometheus.Spec.EnforcedBodySizeLimit = i
			}

			serviceMonitor := monitoringv1.ServiceMonitor{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "testservicemonitor1",
					Namespace: "default",
					Labels: map[string]string{
						"group": "group1",
					},
				},
				Spec: monitoringv1.ServiceMonitorSpec{
					Endpoints: []monitoringv1.Endpoint{
						{
							Port:     "web",
							Interval: "30s",
						},
					},
				},
			}

			cfg, err := cg.GenerateConfig(
				&prometheus,
				map[string]*monitoringv1.ServiceMonitor{
					"testservicemonitor1": &serviceMonitor,
				},
				nil,
				nil,
				&assets.Store{},
				nil,
				nil,
				nil,
				nil,
			)

			if tc.expectedErr != nil {
				if tc.expectedErr.Error() != err.Error() {
					t.Logf("\n%s", pretty.Compare(tc.expectedErr.Error(), err.Error()))
					t.Fatal("expected error and actual error do not match")
				}
			} else {
				result := string(cfg)
				if tc.expected != result {
					t.Logf("\n%s", pretty.Compare(tc.expected, result))
					t.Fatal("expected Prometheus configuration and actual configuration do not match")
				}
			}
		})
	}
}
