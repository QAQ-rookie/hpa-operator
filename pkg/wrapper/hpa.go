package wrapper

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"

	"k8s.io/api/autoscaling/v2beta2"
	k8serrros "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// Controls whether to turn on the HPA for this workload.
	HPAEnable = "hpa.autoscaling.navigatorcloud.io/enable"
	// minReplicas is the lower limit for the number of replicas to which the autoscaler
	// can scale down.
	HPAMinReplicas = "hpa.autoscaling.navigatorcloud.io/minReplicas"
	// maxReplicas is the upper limit for the number of replicas to which the autoscaler
	// can scale up.
	HPAMaxReplicas = "hpa.autoscaling.navigatorcloud.io/maxReplicas"
	// metrics contains the specifications for which to use to calculate the desired replica
	// count(the maximum replica count across all metrics will be used).
	HPAMetrics = "hpa.autoscaling.navigatorcloud.io/metrics"
	// The scheme of `schedule-jobs` is similar with `crontab`, create HPA resource for the
	// workload regularly.
	HPAScheduleJobs = "hpa.autoscaling.navigatorcloud.io/schedule-jobs"
)

var (
	HPADefaultLabels = map[string]string{
		"managed-by": "hpa-operator",
	}
)

type hpaOperator struct {
	client         client.Client
	log            logr.Logger
	namespacedName types.NamespacedName
	annotations    map[string]string
	kind           string
	uid            types.UID
}

func NewHPAOperator(client client.Client, log logr.Logger, namespacedName types.NamespacedName, annotations map[string]string, kind string, uid types.UID) HPAOperator {
	return &hpaOperator{
		client:         client,
		log:            log,
		namespacedName: namespacedName,
		annotations:    annotations,
		kind:           kind,
		uid:            uid,
	}
}

type HPAOperator interface {
	DoHorizontalPodAutoscaler(ctx context.Context)
}

// DoHorizontalPodAutoscaler
// 为不同的 Workload 处理 HPA 的逻辑
// 不返回任何错误，如果有错误，只记录
func (h *hpaOperator) DoHorizontalPodAutoscaler(ctx context.Context) {
	hpaLog := h.log.WithValues("doHorizontalPodAutoscaler", "doing")

	hpaLog.Info("start")
	enable := false
	if val, ok := h.annotations[HPAEnable]; ok {
		if val == "true" {
			enable = true
		}
	}
	if !enable {
		h.log.Info("The HPA is disabled in the workload")
		return
	}

	scheduleEnable := false
	if _, ok := h.annotations[HPAScheduleJobs]; ok {
		scheduleEnable = true
	}
	// (1) 处理定时 HPA 资源
	if scheduleEnable {

	} else {
		h.nonScheduleHPA(ctx)
	}
	return
}

// 创建普通 HPA 资源
func (h *hpaOperator) nonScheduleHPA(ctx context.Context) {
	minReplicas, err := extractAnnotationIntValue(h.annotations, HPAMinReplicas)
	if err != nil {
		h.log.Error(err, "extractAnnotation minReplicas failed")
	}
	// 创建普通的 HPA 资源时，maxReplicas 是必选字段
	maxReplicas, err := extractAnnotationIntValue(h.annotations, HPAMaxReplicas)
	if err != nil {
		h.log.Error(err, "extractAnnotation maxReplicas failed")
		return
	}
	blockOwnerDeletion := true
	isController := true
	ref := metav1.OwnerReference{
		APIVersion:         "apps/v1",
		Kind:               h.kind,
		Name:               h.namespacedName.Name,
		UID:                h.uid,
		BlockOwnerDeletion: &blockOwnerDeletion,
		Controller:         &isController,
	}

	hpa := &v2beta2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      h.namespacedName.Name,
			Namespace: h.namespacedName.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				ref,
			},
			Labels: HPADefaultLabels,
		},
		Spec: v2beta2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: v2beta2.CrossVersionObjectReference{
				Kind:       h.kind,
				Name:       h.namespacedName.Name,
				APIVersion: "apps/v1",
			},
			MaxReplicas: maxReplicas,
			Metrics:     make([]v2beta2.MetricSpec, 0),
		},
	}
	if minReplicas != 0 {
		hpa.Spec.MinReplicas = &minReplicas
	}
	metricsExist := false
	if metricsVal, ok := h.annotations[HPAMetrics]; ok {
		metricsExist = true
		err := json.Unmarshal([]byte(metricsVal), &hpa.Spec.Metrics)
		if err != nil {
			h.log.Error(err, "metrics value is invalid")
			return
		}
	}
	// 查询是否存在对应的 HPA 资源
	// - 存在，检查 Spec 是否一致，不一致更新
	// - 不存在，创建对应的HPA资源即可
	curHPA := &v2beta2.HorizontalPodAutoscaler{}
	err = h.client.Get(ctx, types.NamespacedName{
		Namespace: hpa.Namespace,
		Name:      hpa.Name,
	}, curHPA)
	if err != nil {
		if k8serrros.IsNotFound(err) {
			// create
			err = h.client.Create(ctx, hpa)
			if err != nil && !k8serrros.IsAlreadyExists(err) {
				h.log.Error(err, "failed to create HPA")
			}
			return
		}
		h.log.Error(err, "failed to get HPA")
		return
	}
	// update
	needUpdate := false
	if metricsExist {
		if !reflect.DeepEqual(curHPA.Spec, hpa.Spec) {
			needUpdate = true
		}
	} else {
		if !reflect.DeepEqual(curHPA.Spec.MinReplicas, hpa.Spec.MinReplicas) || !reflect.DeepEqual(curHPA.Spec.MaxReplicas, hpa.Spec.MaxReplicas) {
			needUpdate = true
		}
	}
	if needUpdate {
		err = h.client.Update(ctx, hpa)
		if err != nil {
			h.log.Error(err, "failed to update HPA")
			return
		}
	}
	return
}

func extractAnnotationIntValue(annotations map[string]string, annotationName string) (int32, error) {
	strValue, ok := annotations[annotationName]
	if !ok {
		return 0, errors.New(annotationName + " annotation is missing for workload")
	}
	int64Value, err := strconv.ParseInt(strValue, 10, 32)
	if err != nil {
		return 0, errors.New(annotationName + " value for workload is invalid: " + err.Error())
	}
	value := int32(int64Value)
	if value <= 0 {
		return 0, errors.New(annotationName + " value for workload should be positive number")
	}
	return value, nil
}
