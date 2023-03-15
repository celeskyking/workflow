/*
Copyright 2022 The KubeVela Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/kubevela/pkg/multicluster"
	"github.com/oam-dev/cluster-gateway/pkg/apis/cluster/v1alpha1"
	"io"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"net/http"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"

	wfContext "github.com/kubevela/workflow/pkg/context"
	"github.com/kubevela/workflow/pkg/cue/model/value"
	workflowtypes "github.com/kubevela/workflow/pkg/types"
)

// GetDataFromContext get data from workflow context
func GetDataFromContext(ctx context.Context, cli client.Client, ctxName, name, ns string, paths ...string) (*value.Value, error) {
	wfCtx, err := wfContext.LoadContext(cli, ns, name, ctxName)
	if err != nil {
		return nil, err
	}
	v, err := wfCtx.GetVar(paths...)
	if err != nil {
		return nil, err
	}
	if v.Error() != nil {
		return nil, v.Error()
	}
	return v, nil
}

// GetLogConfigFromStep get log config from step
func GetLogConfigFromStep(ctx context.Context, cli client.Client, ctxName, name, ns, step string) (*workflowtypes.LogConfig, error) {
	wfCtx, err := wfContext.LoadContext(cli, ns, name, ctxName)
	if err != nil {
		return nil, err
	}
	config := make(map[string]workflowtypes.LogConfig)
	c := wfCtx.GetMutableValue(workflowtypes.ContextKeyLogConfig)
	if c == "" {
		return nil, fmt.Errorf("no log config found")
	}

	if err := json.Unmarshal([]byte(c), &config); err != nil {
		return nil, err
	}

	stepConfig, ok := config[step]
	if !ok {
		return nil, fmt.Errorf("no log config found for step %s", step)
	}
	return &stepConfig, nil
}

// GetPodListFromResources get pod list from resources
func GetPodListFromResources(ctx context.Context, cli client.Client, resources []workflowtypes.Resource) ([]workflowtypes.Pod, error) {
	pods := make([]workflowtypes.Pod, 0)
	for _, resource := range resources {
		var clusterClient client.Client
		if resource.Cluster == "" {
			clusterClient = cli
		} else {
			clusterGateway := &v1alpha1.ClusterGateway{}
			cli.Get(ctx, types.NamespacedName{Name: resource.Cluster}, clusterGateway)
			restCfg, err := v1alpha1.NewConfigFromCluster(ctx, clusterGateway)
			if err != nil {
				return nil, err
			}
			cc, err := client.New(restCfg, client.Options{})
			if err != nil {
				return nil, err
			}
			clusterClient = cc
		}
		cliCtx := multicluster.WithCluster(ctx, resource.Cluster)
		if resource.LabelSelector != nil {
			labels := &metav1.LabelSelector{
				MatchLabels: resource.LabelSelector,
			}
			selector, err := metav1.LabelSelectorAsSelector(labels)
			if err != nil {
				return nil, err
			}
			var podList corev1.PodList
			err = clusterClient.List(cliCtx, &podList, &client.ListOptions{
				LabelSelector: selector,
			})
			if err != nil {
				return nil, err
			}
			for _, p := range podList.Items {
				pods = append(pods, workflowtypes.Pod{
					Pod:     p,
					Cluster: resource.Cluster,
				})
			}
			continue
		}
		if resource.Namespace == "" {
			resource.Namespace = metav1.NamespaceDefault
		}
		var pod corev1.Pod
		err := clusterClient.Get(cliCtx, client.ObjectKey{Namespace: resource.Namespace, Name: resource.Name}, &pod)
		if err != nil {
			return nil, err
		}
		pods = append(pods, workflowtypes.Pod{
			Pod:     pod,
			Cluster: resource.Cluster,
		})
	}
	if len(pods) == 0 {
		return nil, fmt.Errorf("no pod found")
	}
	return pods, nil
}

// GetLogsFromURL get logs from url
func GetLogsFromURL(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// GetLogsFromLoki get logs from url
func GetLogsFromLoki(ctx context.Context, lokiCfg workflowtypes.Loki) ([]string, error) {
	url := fmt.Sprintf("http://%s/loki/api/v1/query")
	var kvs []string
	for k, v := range lokiCfg.Labels {
		kvs = append(kvs, fmt.Sprintf("%s=%s", k, v))
	}
	queryText := fmt.Sprintf(`{%s} | json log | line_format "{{.log}}" `, strings.Join(kvs, " "))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.URL.Query().Add("query", queryText)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var response QueryLokiResponse
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(data, &response)
	if err != nil {
		return nil, err
	}
	var logs []string
	if response.Data.ResultType == "streams" {
		for _, line := range response.Data.Result {
			logs = append(logs, line.values[1])
		}
	}
	return nil, nil
}

// GetLogsFromPod get logs from pod
func GetLogsFromPod(ctx context.Context, clientSet kubernetes.Interface, cli client.Client, podName, ns, cluster string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
	cliCtx := multicluster.WithCluster(ctx, cluster)
	var clustergateway v1alpha1.ClusterGateway
	err := cli.Get(cliCtx, types.NamespacedName{Name: cluster}, &clustergateway)
	if err != nil {
		return nil, err
	}
	restCfg, err := v1alpha1.NewConfigFromCluster(cliCtx, &clustergateway)
	if err != nil {
		return nil, err
	}
	restCfg.Insecure = true
	restCfg.CAData = nil
	restCfg.CAFile = ""
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, err
	}
	req := cs.CoreV1().Pods(ns).GetLogs(podName, opts)
	readCloser, err := req.Stream(cliCtx)
	if err != nil {
		return nil, err
	}
	return readCloser, nil
}

type QueryLokiResponse struct {
	Status string              `json:"status"`
	Data   QueryLokiResultData `json:"data"`
}

type QueryLokiResultData struct {
	ResultType string            `json:"resultType"`
	Result     []QueryLokiResult `json:"result"`
}

type QueryLokiResult struct {
	stream map[string]string `json:"stream"`
	values []string          `json:"values"`
}
