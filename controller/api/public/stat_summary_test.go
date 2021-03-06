package public

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/golang/protobuf/proto"
	"github.com/prometheus/common/model"
	tap "github.com/runconduit/conduit/controller/gen/controller/tap"
	pb "github.com/runconduit/conduit/controller/gen/public"
	"github.com/runconduit/conduit/controller/k8s"
	pkgK8s "github.com/runconduit/conduit/pkg/k8s"
)

type statSumExpected struct {
	err                       error
	k8sConfigs                []string               // k8s objects to seed the API
	mockPromResponse          model.Value            // mock out a prometheus query response
	expectedPrometheusQueries []string               // queries we expect public-api to issue to prometheus
	req                       pb.StatSummaryRequest  // the request we would like to test
	expectedResponse          pb.StatSummaryResponse // the stat response we expect
}

func prometheusMetric(resName string, resType string, resNs string, classification string, isDst bool) model.Vector {
	return model.Vector{
		genPromSample(resName, resType, resNs, classification, isDst),
	}
}

func genPromSample(resName string, resType string, resNs string, classification string, isDst bool) *model.Sample {
	labelName := model.LabelName(resType)
	namespaceLabel := model.LabelName("namespace")

	if isDst {
		labelName = "dst_" + labelName
		namespaceLabel = "dst_" + namespaceLabel
	}

	return &model.Sample{
		Metric: model.Metric{
			labelName:        model.LabelValue(resName),
			namespaceLabel:   model.LabelValue(resNs),
			"classification": model.LabelValue(classification),
			"tls":            model.LabelValue("true"),
		},
		Value:     123,
		Timestamp: 456,
	}
}

func genEmptyResponse() pb.StatSummaryResponse {
	return pb.StatSummaryResponse{
		Response: &pb.StatSummaryResponse_Ok_{ // https://github.com/golang/protobuf/issues/205
			Ok: &pb.StatSummaryResponse_Ok{
				StatTables: []*pb.StatTable{
					&pb.StatTable{
						Table: &pb.StatTable_PodGroup_{
							PodGroup: &pb.StatTable_PodGroup{},
						},
					},
				},
			},
		},
	}
}

func testStatSummary(t *testing.T, expectations []statSumExpected) {
	for _, exp := range expectations {
		k8sAPI, err := k8s.NewFakeAPI(exp.k8sConfigs...)
		if err != nil {
			t.Fatalf("NewFakeAPI returned an error: %s", err)
		}

		mockProm := &MockProm{Res: exp.mockPromResponse}
		fakeGrpcServer := newGrpcServer(
			mockProm,
			tap.NewTapClient(nil),
			k8sAPI,
			"conduit",
			[]string{},
		)

		k8sAPI.Sync(nil)

		rsp, err := fakeGrpcServer.StatSummary(context.TODO(), &exp.req)
		if err != exp.err {
			t.Fatalf("Expected error: %s, Got: %s", exp.err, err)
		}

		if len(exp.expectedPrometheusQueries) > 0 {
			sort.Strings(exp.expectedPrometheusQueries)
			sort.Strings(mockProm.QueriesExecuted)

			if !reflect.DeepEqual(exp.expectedPrometheusQueries, mockProm.QueriesExecuted) {
				t.Fatalf("Prometheus queries incorrect. \nExpected:\n%+v \nGot:\n%+v",
					exp.expectedPrometheusQueries, mockProm.QueriesExecuted)
			}
		}

		rspStatTables := rsp.GetOk().StatTables

		if len(rspStatTables) != len(exp.expectedResponse.GetOk().StatTables) {
			t.Fatalf(
				"Expected [%d] stat tables, got [%d].\nExpected:\n%s\nGot:\n%s",
				len(exp.expectedResponse.GetOk().StatTables),
				len(rspStatTables),
				exp.expectedResponse.GetOk().StatTables,
				rspStatTables,
			)
		}

		sort.Sort(byStatResult(rspStatTables))
		statOkRsp := &pb.StatSummaryResponse_Ok{
			StatTables: rspStatTables,
		}

		for i, st := range rspStatTables {
			expected := exp.expectedResponse.GetOk().StatTables[i]
			if !proto.Equal(st, expected) {
				t.Fatalf("Expected: %+v\n Got: %+v", expected, st)
			}
		}

		if !proto.Equal(exp.expectedResponse.GetOk(), statOkRsp) {
			t.Fatalf("Expected: %+v\n Got: %+v", &exp.expectedResponse, rsp)
		}

	}
}

type byStatResult []*pb.StatTable

func (s byStatResult) Len() int {
	return len(s)
}

func (s byStatResult) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s byStatResult) Less(i, j int) bool {
	if len(s[i].GetPodGroup().Rows) == 0 {
		return true
	}
	if len(s[j].GetPodGroup().Rows) == 0 {
		return false
	}

	return s[i].GetPodGroup().Rows[0].Resource.Type < s[j].GetPodGroup().Rows[0].Resource.Type
}

func TestStatSummary(t *testing.T) {
	t.Run("Successfully performs a query based on resource type", func(t *testing.T) {
		expectations := []statSumExpected{
			statSumExpected{
				err: nil,
				k8sConfigs: []string{`
apiVersion: apps/v1beta2
kind: Deployment
metadata:
  name: emoji
  namespace: emojivoto
spec:
  selector:
    matchLabels:
      app: emoji-svc
  strategy: {}
  template:
    spec:
      containers:
      - image: buoyantio/emojivoto-emoji-svc:v3
`, `
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-meshed
  namespace: emojivoto
  labels:
    app: emoji-svc
  annotations:
    conduit.io/proxy-version: testinjectversion
status:
  phase: Running
`, `
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-not-meshed
  namespace: emojivoto
  labels:
    app: emoji-svc
status:
  phase: Running
`, `
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-meshed-not-running
  namespace: emojivoto
  labels:
    app: emoji-svc
  annotations:
    conduit.io/proxy-version: testinjectversion
status:
  phase: Completed
`,
				},
				mockPromResponse: prometheusMetric("emoji", "deployment", "emojivoto", "success", false),
				req: pb.StatSummaryRequest{
					Selector: &pb.ResourceSelection{
						Resource: &pb.Resource{
							Namespace: "emojivoto",
							Type:      pkgK8s.Deployments,
						},
					},
					TimeWindow: "1m",
				},
				expectedResponse: GenStatSummaryResponse("emoji", "deployments", "emojivoto", &PodCounts{
					MeshedPods:  1,
					RunningPods: 2,
					FailedPods:  0,
				}),
			},
		}

		testStatSummary(t, expectations)
	})

	t.Run("Queries prometheus for a specific resource if name is specified", func(t *testing.T) {
		expectations := []statSumExpected{
			statSumExpected{
				err: nil,
				k8sConfigs: []string{`
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-1
  namespace: emojivoto
  labels:
    app: emoji-svc
  annotations:
    conduit.io/proxy-version: testinjectversion
status:
  phase: Running
`,
				},
				mockPromResponse: prometheusMetric("emojivoto-1", "pod", "emojivoto", "success", false),
				req: pb.StatSummaryRequest{
					Selector: &pb.ResourceSelection{
						Resource: &pb.Resource{
							Name:      "emojivoto-1",
							Namespace: "emojivoto",
							Type:      pkgK8s.Pods,
						},
					},
					TimeWindow: "1m",
				},
				expectedPrometheusQueries: []string{
					`histogram_quantile(0.5, sum(irate(response_latency_ms_bucket{direction="inbound", namespace="emojivoto", pod="emojivoto-1"}[1m])) by (le, namespace, pod))`,
					`histogram_quantile(0.95, sum(irate(response_latency_ms_bucket{direction="inbound", namespace="emojivoto", pod="emojivoto-1"}[1m])) by (le, namespace, pod))`,
					`histogram_quantile(0.99, sum(irate(response_latency_ms_bucket{direction="inbound", namespace="emojivoto", pod="emojivoto-1"}[1m])) by (le, namespace, pod))`,
					`sum(increase(response_total{direction="inbound", namespace="emojivoto", pod="emojivoto-1"}[1m])) by (namespace, pod, classification, tls)`,
				},
				expectedResponse: GenStatSummaryResponse("emojivoto-1", "pods", "emojivoto", &PodCounts{
					MeshedPods:  1,
					RunningPods: 1,
					FailedPods:  0,
				}),
			},
		}

		testStatSummary(t, expectations)
	})

	t.Run("Queries prometheus for outbound metrics if from resource is specified, ignores resource name", func(t *testing.T) {
		expectations := []statSumExpected{
			statSumExpected{
				err: nil,
				k8sConfigs: []string{`
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-1
  namespace: emojivoto
  labels:
    app: emoji-svc
  annotations:
    conduit.io/proxy-version: testinjectversion
status:
  phase: Running
`,
				},
				mockPromResponse: prometheusMetric("emojivoto-2", "pod", "emojivoto", "success", false),
				req: pb.StatSummaryRequest{
					Selector: &pb.ResourceSelection{
						Resource: &pb.Resource{
							Name:      "emojivoto-1",
							Namespace: "emojivoto",
							Type:      pkgK8s.Pods,
						},
					},
					TimeWindow: "1m",
					Outbound: &pb.StatSummaryRequest_FromResource{
						FromResource: &pb.Resource{
							Name:      "emojivoto-2",
							Namespace: "emojivoto",
							Type:      pkgK8s.Pods,
						},
					},
				},
				expectedPrometheusQueries: []string{
					`histogram_quantile(0.5, sum(irate(response_latency_ms_bucket{direction="outbound", namespace="emojivoto", pod="emojivoto-2"}[1m])) by (le, dst_namespace, dst_pod))`,
					`histogram_quantile(0.95, sum(irate(response_latency_ms_bucket{direction="outbound", namespace="emojivoto", pod="emojivoto-2"}[1m])) by (le, dst_namespace, dst_pod))`,
					`histogram_quantile(0.99, sum(irate(response_latency_ms_bucket{direction="outbound", namespace="emojivoto", pod="emojivoto-2"}[1m])) by (le, dst_namespace, dst_pod))`,
					`sum(increase(response_total{direction="outbound", namespace="emojivoto", pod="emojivoto-2"}[1m])) by (dst_namespace, dst_pod, classification, tls)`,
				},
				expectedResponse: genEmptyResponse(),
			},
		}

		testStatSummary(t, expectations)
	})

	t.Run("Queries prometheus for outbound metrics if --to resource is specified", func(t *testing.T) {
		expectations := []statSumExpected{
			statSumExpected{
				err: nil,
				k8sConfigs: []string{`
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-1
  namespace: emojivoto
  labels:
    app: emoji-svc
  annotations:
    conduit.io/proxy-version: testinjectversion
status:
  phase: Running
`,
				},
				mockPromResponse: model.Vector{
					genPromSample("emojivoto-1", "pod", "emojivoto", "success", false),
				},
				req: pb.StatSummaryRequest{
					Selector: &pb.ResourceSelection{
						Resource: &pb.Resource{
							Name:      "emojivoto-1",
							Namespace: "emojivoto",
							Type:      pkgK8s.Pods,
						},
					},
					TimeWindow: "1m",
					Outbound: &pb.StatSummaryRequest_ToResource{
						ToResource: &pb.Resource{
							Name:      "emojivoto-2",
							Namespace: "emojivoto",
							Type:      pkgK8s.Pods,
						},
					},
				},
				expectedPrometheusQueries: []string{
					`histogram_quantile(0.5, sum(irate(response_latency_ms_bucket{direction="outbound", dst_namespace="emojivoto", dst_pod="emojivoto-2", namespace="emojivoto", pod="emojivoto-1"}[1m])) by (le, namespace, pod))`,
					`histogram_quantile(0.95, sum(irate(response_latency_ms_bucket{direction="outbound", dst_namespace="emojivoto", dst_pod="emojivoto-2", namespace="emojivoto", pod="emojivoto-1"}[1m])) by (le, namespace, pod))`,
					`histogram_quantile(0.99, sum(irate(response_latency_ms_bucket{direction="outbound", dst_namespace="emojivoto", dst_pod="emojivoto-2", namespace="emojivoto", pod="emojivoto-1"}[1m])) by (le, namespace, pod))`,
					`sum(increase(response_total{direction="outbound", dst_namespace="emojivoto", dst_pod="emojivoto-2", namespace="emojivoto", pod="emojivoto-1"}[1m])) by (namespace, pod, classification, tls)`,
				},
				expectedResponse: GenStatSummaryResponse("emojivoto-1", "pods", "emojivoto", &PodCounts{
					MeshedPods:  1,
					RunningPods: 1,
					FailedPods:  0,
				}),
			},
		}

		testStatSummary(t, expectations)
	})

	t.Run("Queries prometheus for outbound metrics if --to resource is specified and --to-namespace is different from the resource namespace", func(t *testing.T) {
		expectations := []statSumExpected{
			statSumExpected{
				err: nil,
				k8sConfigs: []string{`
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-1
  namespace: emojivoto
  labels:
    app: emoji-svc
  annotations:
    conduit.io/proxy-version: testinjectversion
status:
  phase: Running
`,
				},
				mockPromResponse: model.Vector{
					genPromSample("emojivoto-1", "pod", "emojivoto", "success", false),
				},
				req: pb.StatSummaryRequest{
					Selector: &pb.ResourceSelection{
						Resource: &pb.Resource{
							Name:      "emojivoto-1",
							Namespace: "emojivoto",
							Type:      pkgK8s.Pods,
						},
					},
					TimeWindow: "1m",
					Outbound: &pb.StatSummaryRequest_ToResource{
						ToResource: &pb.Resource{
							Name:      "emojivoto-2",
							Namespace: "totallydifferent",
							Type:      pkgK8s.Pods,
						},
					},
				},
				expectedPrometheusQueries: []string{
					`histogram_quantile(0.5, sum(irate(response_latency_ms_bucket{direction="outbound", dst_namespace="totallydifferent", dst_pod="emojivoto-2", namespace="emojivoto", pod="emojivoto-1"}[1m])) by (le, namespace, pod))`,
					`histogram_quantile(0.95, sum(irate(response_latency_ms_bucket{direction="outbound", dst_namespace="totallydifferent", dst_pod="emojivoto-2", namespace="emojivoto", pod="emojivoto-1"}[1m])) by (le, namespace, pod))`,
					`histogram_quantile(0.99, sum(irate(response_latency_ms_bucket{direction="outbound", dst_namespace="totallydifferent", dst_pod="emojivoto-2", namespace="emojivoto", pod="emojivoto-1"}[1m])) by (le, namespace, pod))`,
					`sum(increase(response_total{direction="outbound", dst_namespace="totallydifferent", dst_pod="emojivoto-2", namespace="emojivoto", pod="emojivoto-1"}[1m])) by (namespace, pod, classification, tls)`,
				},
				expectedResponse: GenStatSummaryResponse("emojivoto-1", "pods", "emojivoto", &PodCounts{
					MeshedPods:  1,
					RunningPods: 1,
					FailedPods:  0,
				}),
			},
		}

		testStatSummary(t, expectations)
	})

	t.Run("Queries prometheus for outbound metrics if --from resource is specified", func(t *testing.T) {
		expectations := []statSumExpected{
			statSumExpected{
				err: nil,
				k8sConfigs: []string{`
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-1
  namespace: emojivoto
  labels:
    app: emoji-svc
  annotations:
    conduit.io/proxy-version: testinjectversion
status:
  phase: Running
`, `
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-2
  namespace: totallydifferent
  labels:
    app: emoji-svc
  annotations:
    conduit.io/proxy-version: testinjectversion
status:
  phase: Running
`,
				},
				mockPromResponse: model.Vector{
					genPromSample("emojivoto-1", "pod", "emojivoto", "success", true),
				},
				req: pb.StatSummaryRequest{
					Selector: &pb.ResourceSelection{
						Resource: &pb.Resource{
							Name:      "",
							Namespace: "emojivoto",
							Type:      pkgK8s.Pods,
						},
					},
					TimeWindow: "1m",
					Outbound: &pb.StatSummaryRequest_FromResource{
						FromResource: &pb.Resource{
							Name:      "emojivoto-2",
							Namespace: "",
							Type:      pkgK8s.Pods,
						},
					},
				},
				expectedPrometheusQueries: []string{
					`histogram_quantile(0.5, sum(irate(response_latency_ms_bucket{direction="outbound", pod="emojivoto-2"}[1m])) by (le, dst_namespace, dst_pod))`,
					`histogram_quantile(0.95, sum(irate(response_latency_ms_bucket{direction="outbound", pod="emojivoto-2"}[1m])) by (le, dst_namespace, dst_pod))`,
					`histogram_quantile(0.99, sum(irate(response_latency_ms_bucket{direction="outbound", pod="emojivoto-2"}[1m])) by (le, dst_namespace, dst_pod))`,
					`sum(increase(response_total{direction="outbound", pod="emojivoto-2"}[1m])) by (dst_namespace, dst_pod, classification, tls)`,
				},
				expectedResponse: GenStatSummaryResponse("emojivoto-1", "pods", "emojivoto", &PodCounts{
					MeshedPods:  1,
					RunningPods: 1,
					FailedPods:  0,
				}),
			},
		}

		testStatSummary(t, expectations)
	})

	t.Run("Queries prometheus for outbound metrics if --from resource is specified and --from-namespace is different from the resource namespace", func(t *testing.T) {
		expectations := []statSumExpected{
			statSumExpected{
				err: nil,
				k8sConfigs: []string{`
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-1
  namespace: emojivoto
  labels:
    app: emoji-svc
  annotations:
    conduit.io/proxy-version: testinjectversion
status:
  phase: Running
`, `
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-2
  namespace: totallydifferent
  labels:
    app: emoji-svc
  annotations:
    conduit.io/proxy-version: testinjectversion
status:
  phase: Running
`,
				},
				mockPromResponse: model.Vector{
					genPromSample("emojivoto-1", "pod", "emojivoto", "success", true),
				},
				req: pb.StatSummaryRequest{
					Selector: &pb.ResourceSelection{
						Resource: &pb.Resource{
							Name:      "emojivoto-1",
							Namespace: "emojivoto",
							Type:      pkgK8s.Pods,
						},
					},
					TimeWindow: "1m",
					Outbound: &pb.StatSummaryRequest_FromResource{
						FromResource: &pb.Resource{
							Name:      "emojivoto-2",
							Namespace: "totallydifferent",
							Type:      pkgK8s.Pods,
						},
					},
				},
				expectedPrometheusQueries: []string{
					`histogram_quantile(0.5, sum(irate(response_latency_ms_bucket{direction="outbound", namespace="totallydifferent", pod="emojivoto-2"}[1m])) by (le, dst_namespace, dst_pod))`,
					`histogram_quantile(0.95, sum(irate(response_latency_ms_bucket{direction="outbound", namespace="totallydifferent", pod="emojivoto-2"}[1m])) by (le, dst_namespace, dst_pod))`,
					`histogram_quantile(0.99, sum(irate(response_latency_ms_bucket{direction="outbound", namespace="totallydifferent", pod="emojivoto-2"}[1m])) by (le, dst_namespace, dst_pod))`,
					`sum(increase(response_total{direction="outbound", namespace="totallydifferent", pod="emojivoto-2"}[1m])) by (dst_namespace, dst_pod, classification, tls)`,
				},
				expectedResponse: GenStatSummaryResponse("emojivoto-1", "pods", "emojivoto", &PodCounts{
					MeshedPods:  1,
					RunningPods: 1,
					FailedPods:  0,
				}),
			},
		}

		testStatSummary(t, expectations)
	})

	t.Run("Successfully queries for resource type 'all'", func(t *testing.T) {
		expectations := []statSumExpected{
			statSumExpected{
				err: nil,
				k8sConfigs: []string{`
apiVersion: apps/v1beta2
kind: Deployment
metadata:
  name: emoji-deploy
  namespace: emojivoto
spec:
  selector:
    matchLabels:
      app: emoji-svc
  strategy: {}
  template:
    spec:
      containers:
      - image: buoyantio/emojivoto-emoji-svc:v3
`, `
apiVersion: v1
kind: Service
metadata:
  name: emoji-svc
  namespace: emojivoto
spec:
  clusterIP: None
  selector:
    app: emoji-svc
`, `
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-pod-1
  namespace: not-right-emojivoto-namespace
  labels:
    app: emoji-svc
  annotations:
    conduit.io/proxy-version: testinjectversion
status:
  phase: Running
`, `
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-pod-2
  namespace: emojivoto
  labels:
    app: emoji-svc
  annotations:
    conduit.io/proxy-version: testinjectversion
status:
  phase: Running
`,
				},
				mockPromResponse: prometheusMetric("emoji-deploy", "deployment", "emojivoto", "success", false),
				req: pb.StatSummaryRequest{
					Selector: &pb.ResourceSelection{
						Resource: &pb.Resource{
							Namespace: "emojivoto",
							Type:      pkgK8s.All,
						},
					},
					TimeWindow: "1m",
				},
				expectedResponse: pb.StatSummaryResponse{
					Response: &pb.StatSummaryResponse_Ok_{ // https://github.com/golang/protobuf/issues/205
						Ok: &pb.StatSummaryResponse_Ok{
							StatTables: []*pb.StatTable{
								&pb.StatTable{
									Table: &pb.StatTable_PodGroup_{
										PodGroup: &pb.StatTable_PodGroup{
											Rows: []*pb.StatTable_PodGroup_Row{},
										},
									},
								},
								&pb.StatTable{
									Table: &pb.StatTable_PodGroup_{
										PodGroup: &pb.StatTable_PodGroup{
											Rows: []*pb.StatTable_PodGroup_Row{
												&pb.StatTable_PodGroup_Row{
													Resource: &pb.Resource{
														Namespace: "emojivoto",
														Type:      "authorities",
													},
													TimeWindow: "1m",
													Stats: &pb.BasicStats{
														SuccessCount:    123,
														FailureCount:    0,
														LatencyMsP50:    123,
														LatencyMsP95:    123,
														LatencyMsP99:    123,
														TlsRequestCount: 123,
													},
												},
											},
										},
									},
								},
								&pb.StatTable{
									Table: &pb.StatTable_PodGroup_{
										PodGroup: &pb.StatTable_PodGroup{
											Rows: []*pb.StatTable_PodGroup_Row{
												&pb.StatTable_PodGroup_Row{
													Resource: &pb.Resource{
														Namespace: "emojivoto",
														Type:      "deployments",
														Name:      "emoji-deploy",
													},
													Stats: &pb.BasicStats{
														SuccessCount:    123,
														FailureCount:    0,
														LatencyMsP50:    123,
														LatencyMsP95:    123,
														LatencyMsP99:    123,
														TlsRequestCount: 123,
													},
													TimeWindow:      "1m",
													MeshedPodCount:  1,
													RunningPodCount: 1,
												},
											},
										},
									},
								},
								&pb.StatTable{
									Table: &pb.StatTable_PodGroup_{
										PodGroup: &pb.StatTable_PodGroup{
											Rows: []*pb.StatTable_PodGroup_Row{
												&pb.StatTable_PodGroup_Row{
													Resource: &pb.Resource{
														Namespace: "emojivoto",
														Type:      "pods",
														Name:      "emojivoto-pod-2",
													},
													TimeWindow:      "1m",
													MeshedPodCount:  1,
													RunningPodCount: 1,
												},
											},
										},
									},
								},
								&pb.StatTable{
									Table: &pb.StatTable_PodGroup_{
										PodGroup: &pb.StatTable_PodGroup{
											Rows: []*pb.StatTable_PodGroup_Row{
												&pb.StatTable_PodGroup_Row{
													Resource: &pb.Resource{
														Namespace: "emojivoto",
														Type:      "services",
														Name:      "emoji-svc",
													},
													TimeWindow:      "1m",
													MeshedPodCount:  1,
													RunningPodCount: 1,
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		}

		testStatSummary(t, expectations)
	})

	t.Run("Given an invalid resource type, returns error", func(t *testing.T) {
		k8sAPI, err := k8s.NewFakeAPI()
		if err != nil {
			t.Fatalf("NewFakeAPI returned an error: %s", err)
		}

		expectations := []statSumExpected{
			statSumExpected{
				err: errors.New("rpc error: code = Unimplemented desc = unimplemented resource type: badtype"),
				req: pb.StatSummaryRequest{
					Selector: &pb.ResourceSelection{
						Resource: &pb.Resource{
							Type: "badtype",
						},
					},
				},
			},
			statSumExpected{
				err: errors.New("rpc error: code = Unimplemented desc = unimplemented resource type: deployment"),
				req: pb.StatSummaryRequest{
					Selector: &pb.ResourceSelection{
						Resource: &pb.Resource{
							Type: "deployment",
						},
					},
				},
			},
			statSumExpected{
				err: errors.New("rpc error: code = Unimplemented desc = unimplemented resource type: pod"),
				req: pb.StatSummaryRequest{
					Selector: &pb.ResourceSelection{
						Resource: &pb.Resource{
							Type: "pod",
						},
					},
				},
			},
		}

		for _, exp := range expectations {
			fakeGrpcServer := newGrpcServer(
				&MockProm{Res: exp.mockPromResponse},
				tap.NewTapClient(nil),
				k8sAPI,
				"conduit",
				[]string{},
			)

			_, err := fakeGrpcServer.StatSummary(context.TODO(), &exp.req)
			if err != nil || exp.err != nil {
				if (err == nil && exp.err != nil) ||
					(err != nil && exp.err == nil) ||
					(err.Error() != exp.err.Error()) {
					t.Fatalf("Unexpected error (Expected: %s, Got: %s)", exp.err, err)
				}
			}
		}
	})

	t.Run("Validates service stat requests", func(t *testing.T) {
		k8sAPI, err := k8s.NewFakeAPI()
		if err != nil {
			t.Fatalf("NewFakeAPI returned an error: %s", err)
		}
		fakeGrpcServer := newGrpcServer(
			&MockProm{Res: model.Vector{}},
			tap.NewTapClient(nil),
			k8sAPI,
			"conduit",
			[]string{},
		)

		invalidRequests := []statSumExpected{
			statSumExpected{
				req: pb.StatSummaryRequest{},
			},
			statSumExpected{
				req: pb.StatSummaryRequest{
					Selector: &pb.ResourceSelection{
						Resource: &pb.Resource{
							Type: "services",
						},
					},
				},
			},
			statSumExpected{
				req: pb.StatSummaryRequest{
					Selector: &pb.ResourceSelection{
						Resource: &pb.Resource{
							Type: "services",
						},
					},
					Outbound: &pb.StatSummaryRequest_ToResource{
						ToResource: &pb.Resource{
							Type: "pods",
						},
					},
				},
			},
			statSumExpected{
				req: pb.StatSummaryRequest{
					Selector: &pb.ResourceSelection{
						Resource: &pb.Resource{
							Type: "pods",
						},
					},
					Outbound: &pb.StatSummaryRequest_FromResource{
						FromResource: &pb.Resource{
							Type: "services",
						},
					},
				},
			},
		}

		for _, invalid := range invalidRequests {
			rsp, err := fakeGrpcServer.StatSummary(context.TODO(), &invalid.req)

			if err != nil || rsp.GetError() == nil {
				t.Fatalf("Expected validation error on StatSummaryResponse, got %v, %v", rsp, err)
			}
		}

		validRequests := []statSumExpected{
			statSumExpected{
				req: pb.StatSummaryRequest{
					Selector: &pb.ResourceSelection{
						Resource: &pb.Resource{
							Type: "pods",
						},
					},
					Outbound: &pb.StatSummaryRequest_ToResource{
						ToResource: &pb.Resource{
							Type: "services",
						},
					},
				},
			},
			statSumExpected{
				req: pb.StatSummaryRequest{
					Selector: &pb.ResourceSelection{
						Resource: &pb.Resource{
							Type: "services",
						},
					},
					Outbound: &pb.StatSummaryRequest_FromResource{
						FromResource: &pb.Resource{
							Type: "pods",
						},
					},
				},
			},
		}

		for _, valid := range validRequests {
			rsp, err := fakeGrpcServer.StatSummary(context.TODO(), &valid.req)

			if err != nil || rsp.GetError() != nil {
				t.Fatalf("Did not expect validation error on StatSummaryResponse, got %v, %v", rsp, err)
			}
		}
	})

	t.Run("Return empty stats summary response", func(t *testing.T) {
		t.Run("when pod phase is succeeded or failed", func(t *testing.T) {
			expectations := []statSumExpected{
				statSumExpected{
					err: nil,
					k8sConfigs: []string{`
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-00
  namespace: emojivoto
  labels:
    app: emoji-svc
  annotations:
    conduit.io/proxy-version: testinjectversion
status:
  phase: Succeeded
`, `
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-01
  namespace: emojivoto
  labels:
    app: emoji-svc
  annotations:
    conduit.io/proxy-version: testinjectversion
status:
  phase: Failed
`},
					mockPromResponse: model.Vector{},
					req: pb.StatSummaryRequest{
						Selector: &pb.ResourceSelection{
							Resource: &pb.Resource{
								Namespace: "emojivoto",
								Type:      pkgK8s.Pods,
							},
						},
					},
					expectedPrometheusQueries: []string{},
					expectedResponse:          genEmptyResponse(),
				},
			}

			testStatSummary(t, expectations)
		})

		t.Run("for succeeded or failed replicas of a deployment", func(t *testing.T) {
			expectations := []statSumExpected{
				statSumExpected{
					err: nil,
					k8sConfigs: []string{`
apiVersion: apps/v1beta2
kind: Deployment
metadata:
  name: emoji
  namespace: emojivoto
spec:
  selector:
    matchLabels:
      app: emoji-svc
  strategy: {}
  template:
    spec:
      containers:
      - image: buoyantio/emojivoto-emoji-svc:v3
`, `
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-00
  namespace: emojivoto
  labels:
    app: emoji-svc
  annotations:
    conduit.io/proxy-version: testinjectversion
status:
  phase: Running
`, `
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-01
  namespace: emojivoto
  labels:
    app: emoji-svc
status:
  phase: Running
`, `
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-02
  namespace: emojivoto
  labels:
    app: emoji-svc
  annotations:
    conduit.io/proxy-version: testinjectversion
status:
  phase: Failed
`, `
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-03
  namespace: emojivoto
  labels:
    app: emoji-svc
  annotations:
    conduit.io/proxy-version: testinjectversion
status:
  phase: Succeeded
`},
					mockPromResponse: prometheusMetric("emoji", "deployment", "emojivoto", "success", false),
					req: pb.StatSummaryRequest{
						Selector: &pb.ResourceSelection{
							Resource: &pb.Resource{
								Namespace: "emojivoto",
								Type:      pkgK8s.Deployments,
							},
						},
						TimeWindow: "1m",
					},
					expectedResponse: GenStatSummaryResponse("emoji", "deployments", "emojivoto", &PodCounts{
						MeshedPods:  1,
						RunningPods: 2,
						FailedPods:  1,
					}),
				},
			}

			testStatSummary(t, expectations)
		})
	})

	t.Run("Queries prometheus for authority stats", func(t *testing.T) {
		expectations := []statSumExpected{
			statSumExpected{
				err: nil,
				k8sConfigs: []string{`
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-1
  namespace: emojivoto
  labels:
    app: emoji-svc
  annotations:
    conduit.io/proxy-version: testinjectversion
status:
  phase: Running
`,
				},
				mockPromResponse: model.Vector{
					genPromSample("10.1.1.239:9995", "authority", "conduit", "success", false),
				},
				req: pb.StatSummaryRequest{
					Selector: &pb.ResourceSelection{
						Resource: &pb.Resource{
							Namespace: "conduit",
							Type:      pkgK8s.Authorities,
						},
					},
					TimeWindow: "1m",
				},
				expectedPrometheusQueries: []string{
					`histogram_quantile(0.5, sum(irate(response_latency_ms_bucket{direction="inbound", namespace="conduit"}[1m])) by (le, namespace, authority))`,
					`histogram_quantile(0.95, sum(irate(response_latency_ms_bucket{direction="inbound", namespace="conduit"}[1m])) by (le, namespace, authority))`,
					`histogram_quantile(0.99, sum(irate(response_latency_ms_bucket{direction="inbound", namespace="conduit"}[1m])) by (le, namespace, authority))`,
					`sum(increase(response_total{direction="inbound", namespace="conduit"}[1m])) by (namespace, authority, classification, tls)`,
				},
				expectedResponse: GenStatSummaryResponse("10.1.1.239:9995", "authorities", "conduit", nil),
			},
		}

		testStatSummary(t, expectations)
	})

	t.Run("Queries prometheus for authority stats when --from authority is used", func(t *testing.T) {
		expectations := []statSumExpected{
			statSumExpected{
				err: nil,
				k8sConfigs: []string{`
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-1
  namespace: emojivoto
  labels:
    app: emoji-svc
  annotations:
    conduit.io/proxy-version: testinjectversion
status:
  phase: Running
`,
				},
				mockPromResponse: model.Vector{
					genPromSample("10.1.1.239:9995", "authority", "conduit", "success", false),
				},
				req: pb.StatSummaryRequest{
					Selector: &pb.ResourceSelection{
						Resource: &pb.Resource{
							Namespace: "conduit",
							Type:      pkgK8s.Authorities,
						},
					},
					TimeWindow: "1m",
					Outbound: &pb.StatSummaryRequest_FromResource{
						FromResource: &pb.Resource{
							Name:      "emojivoto",
							Namespace: "",
							Type:      pkgK8s.Deployments,
						},
					},
				},
				expectedPrometheusQueries: []string{
					`histogram_quantile(0.5, sum(irate(response_latency_ms_bucket{deployment="emojivoto", direction="outbound"}[1m])) by (le, dst_namespace, authority))`,
					`histogram_quantile(0.95, sum(irate(response_latency_ms_bucket{deployment="emojivoto", direction="outbound"}[1m])) by (le, dst_namespace, authority))`,
					`histogram_quantile(0.99, sum(irate(response_latency_ms_bucket{deployment="emojivoto", direction="outbound"}[1m])) by (le, dst_namespace, authority))`,
					`sum(increase(response_total{deployment="emojivoto", direction="outbound"}[1m])) by (dst_namespace, authority, classification, tls)`,
				},
				expectedResponse: GenStatSummaryResponse("10.1.1.239:9995", "authorities", "", nil),
			},
		}

		testStatSummary(t, expectations)
	})

	t.Run("Queries prometheus for a named authority", func(t *testing.T) {
		expectations := []statSumExpected{
			statSumExpected{
				err: nil,
				k8sConfigs: []string{`
apiVersion: v1
kind: Pod
metadata:
  name: emojivoto-1
  namespace: emojivoto
  labels:
    app: emoji-svc
  annotations:
    conduit.io/proxy-version: testinjectversion
status:
  phase: Running
`,
				},
				mockPromResponse: model.Vector{
					genPromSample("10.1.1.239:9995", "authority", "conduit", "success", false),
				},
				req: pb.StatSummaryRequest{
					Selector: &pb.ResourceSelection{
						Resource: &pb.Resource{
							Namespace: "conduit",
							Type:      pkgK8s.Authorities,
							Name:      "10.1.1.239:9995",
						},
					},
					TimeWindow: "1m",
				},
				expectedPrometheusQueries: []string{
					`histogram_quantile(0.5, sum(irate(response_latency_ms_bucket{authority="10.1.1.239:9995", direction="inbound", namespace="conduit"}[1m])) by (le, namespace, authority))`,
					`histogram_quantile(0.95, sum(irate(response_latency_ms_bucket{authority="10.1.1.239:9995", direction="inbound", namespace="conduit"}[1m])) by (le, namespace, authority))`,
					`histogram_quantile(0.99, sum(irate(response_latency_ms_bucket{authority="10.1.1.239:9995", direction="inbound", namespace="conduit"}[1m])) by (le, namespace, authority))`,
					`sum(increase(response_total{authority="10.1.1.239:9995", direction="inbound", namespace="conduit"}[1m])) by (namespace, authority, classification, tls)`,
				},
				expectedResponse: GenStatSummaryResponse("10.1.1.239:9995", "authorities", "conduit", nil),
			},
		}

		testStatSummary(t, expectations)
	})
}
