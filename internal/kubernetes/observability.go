package kubernetes

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.opencensus.io/stats"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	client "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/workqueue"
)

const (
	ConfigurationAnnotationKey = "node-lifecycle.datadoghq.com/draino-configuration"
	nodeOptionsMetricName      = "node_options_nodes_total"
)

type DrainoConfigurationObserver interface {
	Runner
	IsInScope(node *v1.Node) (bool, string, error)
	Reset()
}

// metricsObjectsForObserver groups all the object required to serve the metrics
type metricsObjectsForObserver struct {
	previousMeasureNodesWithNodeOptions *view.View
	MeasureNodesWithNodeOptions         *stats.Int64Measure
}

// reset: replace existing gauges to eliminate obsolete series
func (g *metricsObjectsForObserver) reset() error {
	if g.previousMeasureNodesWithNodeOptions != nil {
		view.Unregister(g.previousMeasureNodesWithNodeOptions)
	}

	if err := wait.Poll(100*time.Millisecond, 5*time.Second, func() (done bool, err error) { return view.Find(nodeOptionsMetricName) == nil, nil }); err != nil {
		return fmt.Errorf("failed to purge previous series")
	} // wait for metrics engine to purge previous series

	g.MeasureNodesWithNodeOptions = stats.Int64(nodeOptionsMetricName, "Number of nodes for each options", stats.UnitDimensionless)
	g.previousMeasureNodesWithNodeOptions = &view.View{
		Name:        nodeOptionsMetricName,
		Measure:     g.MeasureNodesWithNodeOptions,
		Description: "Number of nodes for each options",
		Aggregation: view.LastValue(),
		TagKeys:     []tag.Key{TagNodegroupName, TagNodegroupNamespace, TagTeam, TagConditions, TagUserOptOutViaPodAnnotation, TagDrainRetry, TagPVCManagement, TagPreprovisioning, TagInScope},
	}

	view.Register(g.previousMeasureNodesWithNodeOptions)
	return nil
}

// DrainoConfigurationObserverImpl is responsible for annotating the nodes with the draino configuration that cover the node (if any)
// It also expose a metrics 'draino_in_scope_nodes_total' that count the nodes of a nodegroup that are in scope of a given configuration. The metric is available per condition. A condition named 'any' is virtually defined to groups all the nodes (with or without conditions).
type DrainoConfigurationObserverImpl struct {
	kclient        client.Interface
	nodeStore      NodeStore
	podStore       PodStore
	analysisPeriod time.Duration
	// queueNodeToBeUpdated: To avoid burst in node updates the work to be done is queued. This way we can pace the node updates.
	// The consequence is that the metric is not 100% accurate when the controller starts. It converges after couple ou cycles.
	queueNodeToBeUpdated workqueue.RateLimitingInterface

	configName          string
	conditions          []SuppliedCondition
	nodeFilterFunc      func(obj interface{}) bool
	podFilterFunc       PodFilterFunc
	userOptOutPodFilter PodFilterFunc
	logger              *zap.Logger

	metricsObjects metricsObjectsForObserver
}

var _ DrainoConfigurationObserver = &DrainoConfigurationObserverImpl{}

func NewScopeObserver(client client.Interface, configName string, conditions []SuppliedCondition, nodeStore NodeStore, podStore PodStore, analysisPeriod time.Duration, podFilterFunc PodFilterFunc, userOptOutPodFilter PodFilterFunc, nodeFilterFunc func(obj interface{}) bool, log *zap.Logger) DrainoConfigurationObserver {
	scopeObserver := &DrainoConfigurationObserverImpl{
		kclient:             client,
		nodeStore:           nodeStore,
		podStore:            podStore,
		nodeFilterFunc:      nodeFilterFunc,
		podFilterFunc:       podFilterFunc,
		userOptOutPodFilter: userOptOutPodFilter,
		analysisPeriod:      analysisPeriod,
		logger:              log,
		configName:          configName,
		conditions:          conditions,
		queueNodeToBeUpdated: workqueue.NewNamedRateLimitingQueue(workqueue.NewMaxOfRateLimiter(
			workqueue.NewItemExponentialFailureRateLimiter(200*time.Millisecond, 20*time.Second),
			&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(0.5, 1)}),
			"nodeUpdater"),
	}
	return scopeObserver
}

type inScopeTags struct {
	NodeTagsValues
	InScope                    bool
	PreprovisioningEnabled     bool
	PVCManagementEnabled       bool
	DrainRetry                 bool
	UserOptOutViaPodAnnotation bool
	Condition                  string
}

type inScopeMetrics map[inScopeTags]int64

func (s *DrainoConfigurationObserverImpl) Run(stop <-chan struct{}) {
	ticker := time.NewTicker(s.analysisPeriod)
	// Wait for the informer to sync before starting
	wait.PollImmediateInfinite(10*time.Second, func() (done bool, err error) {
		return s.podStore.HasSynced(), nil
	})

	go s.processQueueForNodeUpdates()
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			s.queueNodeToBeUpdated.ShutDown()
			return
		case <-ticker.C:
			// Let's update the nodes metadata
			for _, node := range s.nodeStore.ListNodes() {
				if s.IsAnnotationUpdateNeeded(node) {
					s.addNodeToQueue(node)
				}
			}
			newMetricsValue := inScopeMetrics{}
			// Let's update the metrics
			for _, node := range s.nodeStore.ListNodes() {
				nodeTags := GetNodeTagsValues(node)
				conditions := GetNodeOffendingConditions(node, s.conditions)
				if node.Annotations == nil {
					node.Annotations = map[string]string{}
				}
				t := inScopeTags{
					NodeTagsValues:             nodeTags,
					InScope:                    len(node.Annotations[ConfigurationAnnotationKey]) > 0,
					PreprovisioningEnabled:     node.Annotations[preprovisioningAnnotationKey] == preprovisioningAnnotationValue,
					PVCManagementEnabled:       s.HasPodWithPVCManagementEnabled(node),
					DrainRetry:                 node.Annotations[drainRetryAnnotationKey] == drainRetryAnnotationValue,
					UserOptOutViaPodAnnotation: s.HasPodWithUserOptOutAnnotation(node),
				}
				// adding a virtual condition 'any' to be able to count the nodes whatever the condition(s) or absence of condition.
				conditionsWithAll := append(GetConditionsTypes(conditions), "any")
				for _, c := range conditionsWithAll {
					t.Condition = c
					newMetricsValue[t] = newMetricsValue[t] + 1
				}
			}
			s.updateGauges(newMetricsValue)
		}
	}
}

//updateGauges is in charge of updating the gauges values and purging the series that do not exist anymore
//
// Note: with opencensus unregistering/registering the view would clean-up the old series. The problem is that a metrics has to be registered to be recorded.
//       As a consequence there is a risk of concurrency between the goroutine that populates the fresh registered metric and the one that expose the metric for the scape.
//       There is no other way around I could find for the moment to cleanup old series. The concurrency risk is clearly acceptable if we look at the frequency of metric poll versus the frequency and a speed of metric generation.
//       In worst case the server will be missing series for a given scrape (not even report a bad value, just missing series). So the impact if it happens is insignificant.
func (s *DrainoConfigurationObserverImpl) updateGauges(metrics inScopeMetrics) {
	s.logger.Info("updating gauges")
	if err := s.metricsObjects.reset(); err != nil {
		s.logger.Error("Unable to purger previous metrics series")
		return
	}
	s.logger.Info("reset gauges done")
	for tagsValues, count := range metrics {
		allTags, _ := tag.New(context.Background(),
			tag.Upsert(TagNodegroupNamespace, tagsValues.NgNamespace), tag.Upsert(TagNodegroupName, tagsValues.NgName),
			tag.Upsert(TagTeam, tagsValues.Team),
			tag.Upsert(TagConditions, tagsValues.Condition),
			tag.Upsert(TagInScope, strconv.FormatBool(tagsValues.InScope)),
			tag.Upsert(TagPreprovisioning, strconv.FormatBool(tagsValues.PreprovisioningEnabled)),
			tag.Upsert(TagPVCManagement, strconv.FormatBool(tagsValues.PVCManagementEnabled)),
			tag.Upsert(TagDrainRetry, strconv.FormatBool(tagsValues.DrainRetry)),
			tag.Upsert(TagUserOptOutViaPodAnnotation, strconv.FormatBool(tagsValues.UserOptOutViaPodAnnotation)))
		stats.Record(allTags, s.metricsObjects.MeasureNodesWithNodeOptions.M(count))
	}
	s.logger.Info("metrics gauges done")
}

func (s *DrainoConfigurationObserverImpl) addNodeToQueue(node *v1.Node) {
	s.logger.Info("Adding node to queue", zap.String("node", node.Name))
	s.queueNodeToBeUpdated.AddRateLimited(node.Name)
}

func (s *DrainoConfigurationObserverImpl) IsAnnotationUpdateNeeded(node *v1.Node) bool {
	configs := strings.Split(node.Annotations[ConfigurationAnnotationKey], ",")
	inScope, _, err := s.IsInScope(node)
	if err != nil {
		s.logger.Error("Can't check if node is in scope", zap.Error(err), zap.String("node", node.Name))
		return false
	}
	if inScope {
		for _, c := range configs {
			if c == s.configName {
				return false
			}
		}
		return true
	}
	for _, c := range configs {
		if c == s.configName {
			return true
		}
	}
	return false
}

// IsInScope return if the node is in scope of the running configuration. If not it also return the reason for not being in scope.
func (s *DrainoConfigurationObserverImpl) IsInScope(node *v1.Node) (inScope bool, reasonIfnOtInScope string, err error) {
	if !s.nodeFilterFunc(node) {
		return false, "labelSelection", nil
	}
	var pods []*v1.Pod
	if pods, err = s.podStore.ListPodsForNode(node.Name); err != nil {
		return false, "", err
	}
	for _, p := range pods {
		passes, reason, err := s.podFilterFunc(*p)
		if err != nil {
			return false, "", err
		}
		if !passes {
			return false, reason, nil
		}
	}
	return true, "", nil
}

func (s *DrainoConfigurationObserverImpl) processQueueForNodeUpdates() {
	for {
		obj, shutdown := s.queueNodeToBeUpdated.Get()
		if shutdown {
			s.logger.Info("Queue shutdown")
			break
		}

		// func encapsultation to benefit from defer s.queue.Done()
		func(obj interface{}) {
			defer s.queueNodeToBeUpdated.Done(obj)
			nodeName := obj.(string)
			requeueCount := s.queueNodeToBeUpdated.NumRequeues(nodeName)
			if requeueCount > 10 {
				s.queueNodeToBeUpdated.Forget(nodeName)
				s.logger.Error("retrying count exceeded", zap.String("node", nodeName))
				return
			}

			if err := RetryWithTimeout(func() error {
				err := s.updateNodeAnnotations(nodeName)
				if err != nil {
					s.logger.Info("Failed attempt to update annotation", zap.String("node", nodeName), zap.Error(err))
				}
				return err
			}, 500*time.Millisecond, 10); err != nil {
				s.logger.Error("Failed to update annotations", zap.String("node", nodeName), zap.Int("retry", requeueCount))
				s.queueNodeToBeUpdated.AddRateLimited(obj)
				return
			}
			// Remove the nodeName from the queue
			s.queueNodeToBeUpdated.Forget(nodeName)
		}(obj)
	}
}

func (s *DrainoConfigurationObserverImpl) updateNodeAnnotations(nodeName string) error {
	s.logger.Info("Update node annotations", zap.String("node", nodeName))
	node, err := s.nodeStore.Get(nodeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	var configs []string
	configsFromAnnotation := strings.Split(node.Annotations[ConfigurationAnnotationKey], ",")
	for _, c := range configsFromAnnotation {
		if c == "" { // remove empty annotation that reflect no scope.
			continue
		}
		configs = append(configs, c)
	}
	initialConfigCount := len(configs)

	inScope, _, err := s.IsInScope(node)
	if err != nil {
		s.logger.Error("Can't check if node is in scope", zap.Error(err), zap.String("node", node.Name))
		return err
	}
	if inScope {
		found := false
		for _, c := range configs {
			if c == s.configName {
				found = true
				break
			}
		}
		if !found {
			configs = append(configs, s.configName)
		}
	} else {
		toKeep := []string{}
		for _, c := range configs {
			if c != s.configName {
				toKeep = append(toKeep, c)
			}
		}
		configs = toKeep
	}

	if len(configs) != initialConfigCount {
		newConfig := strings.Join(configs, ",")
		if node.Annotations == nil {
			node.Annotations = map[string]string{}
		}
		return PatchNodeAnnotationKey(s.kclient, nodeName, ConfigurationAnnotationKey, newConfig)
	}
	return nil
}

// Reset: remove all previous persisted values in node annotations.
// This can be useful if ever the name of the draino configuration changes
func (s *DrainoConfigurationObserverImpl) Reset() {
	if err := wait.PollImmediateInfinite(2*time.Second, func() (done bool, err error) {
		synced := s.nodeStore.HasSynced()
		s.logger.Info("Wait for node informer to sync", zap.Bool("synced", synced))
		return synced, nil
	}); err != nil {
		s.logger.Error("Failed to sync node informer before reset annotation. Reset annotation cancelled.")
	}

	s.logger.Info("Resetting annotations for configuration names")
	// Reset the annotations that are set by the observer
	for _, node := range s.nodeStore.ListNodes() {
		s.logger.Info("Resetting annotations for node", zap.String("node", node.Name))
		if node.Annotations[ConfigurationAnnotationKey] != "" {
			if err := RetryWithTimeout(func() error {
				time.Sleep(2 * time.Second)
				err := PatchDeleteNodeAnnotationKey(s.kclient, node.Name, ConfigurationAnnotationKey)
				if err != nil {
					s.logger.Info("Failed attempt to reset annotation", zap.String("node", node.Name), zap.Error(err))
				}
				return err
			}, 500*time.Millisecond, 10); err != nil {
				s.logger.Error("Failed to reset annotations", zap.String("node", node.Name), zap.Error(err))
				continue
			}
			s.logger.Info("Annotation reset done", zap.String("node", node.Name))
		}
	}
	s.logger.Info("Nodes annotation reset completed")
}

func (s *DrainoConfigurationObserverImpl) HasPodWithPVCManagementEnabled(node *v1.Node) bool {
	if node == nil {
		return false
	}
	pods, err := s.podStore.ListPodsForNode(node.Name)
	if err != nil {
		s.logger.Error("Failed to list pod for node in DrainoConfigurationObserverImpl.HasPodWithPVCManagementEnabled", zap.String("node", node.Name), zap.Error(err))
		return false
	}
	for _, p := range pods {
		if p.Annotations[PVCStorageClassCleanupAnnotationKey] == PVCStorageClassCleanupAnnotationValue {
			return true
		}
	}
	return false
}

func (s *DrainoConfigurationObserverImpl) HasPodWithUserOptOutAnnotation(node *v1.Node) bool {
	if node == nil {
		return false
	}
	pods, err := s.podStore.ListPodsForNode(node.Name)
	if err != nil {
		s.logger.Error("Failed to list pods for node in DrainoConfigurationObserverImpl.HasPodWithUserOptOutAnnotation", zap.String("node", node.Name), zap.Error(err))
		return false
	}
	for _, p := range pods {
		optOut, _, err := s.userOptOutPodFilter(*p)
		if err != nil {
			s.logger.Error("Failed to check if pod is opt-out", zap.String("node", node.Name), zap.String("pod", p.Name), zap.Error(err))
			continue
		}
		if optOut {
			return true
		}
	}
	return false
}
