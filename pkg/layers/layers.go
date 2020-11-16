//Package layers provides an interface for processing AddonsLayers.
//go:generate mockgen -destination=../mocks/layers/mockLayers.go -package=mocks -source=layers.go . Layer
package layers

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta1"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"golang.org/x/mod/semver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kraanv1alpha1 "github.com/fidelity/kraan/api/v1alpha1"
	"github.com/fidelity/kraan/pkg/repos"
	"github.com/fidelity/kraan/pkg/utils"
)

// MaxConditions is the maximum number of condtions to retain.
var (
	MaxConditions = 10
	RootPath      = "/data"
)

func init() {
	path, set := os.LookupEnv("DATA_PATH")
	if set {
		RootPath = path
	}
}

// Layer defines the interface for managing the layer.
type Layer interface {
	SetStatusK8sVersion()
	SetStatusApplying()
	SetStatusPruning()
	SetStatusPending()
	SetStatusDeployed()
	StatusUpdate(status, reason, message string)

	IsHold() bool
	SetHold()
	DependenciesDeployed() bool

	GetSourceKey() string
	GetStatus() string
	GetName() string
	GetLogger() logr.Logger
	GetContext() context.Context
	GetSourcePath() string
	GetTimeout() time.Duration
	IsUpdated() bool
	NeedsRequeue() bool
	IsDelayed() bool
	GetDelay() time.Duration
	SetRequeue()
	SetDelayedRequeue()
	SetUpdated()
	GetRequiredK8sVersion() string
	CheckK8sVersion() bool
	GetFullStatus() *kraanv1alpha1.AddonsLayerStatus
	GetSpec() *kraanv1alpha1.AddonsLayerSpec
	GetAddonsLayer() *kraanv1alpha1.AddonsLayer
	RevisionReady(conditions []meta.Condition, revision string) (bool, string)
}

// KraanLayer is the Schema for the addons API.
type KraanLayer struct {
	updated     bool
	requeue     bool
	delayed     bool
	delay       time.Duration
	ctx         context.Context
	client      client.Client
	k8client    kubernetes.Interface
	log         logr.Logger
	Layer       `json:"-"`
	addonsLayer *kraanv1alpha1.AddonsLayer
}

// CreateLayer creates a layer object.
func CreateLayer(ctx context.Context, client client.Client, k8client kubernetes.Interface, log logr.Logger, addonsLayer *kraanv1alpha1.AddonsLayer) Layer {
	l := &KraanLayer{
		requeue:     false,
		delayed:     false,
		updated:     false,
		ctx:         ctx,
		log:         log,
		client:      client,
		k8client:    k8client,
		addonsLayer: addonsLayer,
	}
	l.delay = l.addonsLayer.Spec.Interval.Duration
	return l
}

// SetRequeue sets the requeue flag to cause the AddonsLayer to be requeued.
func (l *KraanLayer) SetRequeue() {
	l.requeue = true
}

// GetSourcePath gets the path to an addons layer's top directory in the local filesystem.
func (l *KraanLayer) GetSourcePath() string {
	return fmt.Sprintf("%s/layers/%s/%s",
		repos.DefaultRootPath,
		l.GetName(),
		l.GetSpec().Version)
}

// SetUpdated sets the updated flag to cause the AddonsLayer to update the custom resource.
func (l *KraanLayer) SetUpdated() {
	l.updated = true
}

// SetDelayedRequeue sets the delayed flag to cause the AddonsLayer to delay the requeue.
func (l *KraanLayer) SetDelayedRequeue() {
	l.delayed = true
	l.requeue = true
}

// GetFullStatus returns the AddonsLayers Status sub resource.
func (l *KraanLayer) GetFullStatus() *kraanv1alpha1.AddonsLayerStatus {
	return &l.addonsLayer.Status
}

// GetSpec returns the AddonsLayers Spec.
func (l *KraanLayer) GetSpec() *kraanv1alpha1.AddonsLayerSpec {
	return &l.addonsLayer.Spec
}

// GetAddonsLayer returns the AddonsLayers Spec.
func (l *KraanLayer) GetAddonsLayer() *kraanv1alpha1.AddonsLayer {
	return l.addonsLayer
}

// GetRequiredK8sVersion returns the K8s Version required.
func (l *KraanLayer) GetRequiredK8sVersion() string {
	return l.addonsLayer.Spec.PreReqs.K8sVersion
}

func (l *KraanLayer) getK8sClient() kubernetes.Interface {
	return l.k8client
}

// CheckK8sVersion checks if the cluster api server version is equal to or above the required version.
func (l *KraanLayer) CheckK8sVersion() bool {
	utils.TraceCall(l.GetLogger())
	defer utils.TraceExit(l.GetLogger())
	versionInfo, err := l.getK8sClient().Discovery().ServerVersion()
	if err != nil {
		l.GetLogger().Error(err, "failed get server version", utils.GetFunctionAndSource(utils.MyCaller)...)
		l.StatusUpdate(l.GetStatus(), "failed to obtain cluster api server version",
			err.Error())
		l.SetDelayedRequeue()
		return false
	}
	return semver.Compare(versionInfo.String(), l.GetRequiredK8sVersion()) >= 0
}

func (l *KraanLayer) trimConditions() {
	utils.TraceCall(l.GetLogger())
	defer utils.TraceExit(l.GetLogger())
	length := len(l.addonsLayer.Status.Conditions)
	if length < MaxConditions {
		return
	}
	trimedCond := l.addonsLayer.Status.Conditions[length-MaxConditions:]
	l.addonsLayer.Status.Conditions = trimedCond
}

func (l *KraanLayer) setStatus(status, reason, message string) {
	utils.TraceCall(l.GetLogger())
	defer utils.TraceExit(l.GetLogger())
	length := len(l.addonsLayer.Status.Conditions)
	if length > 0 {
		last := &l.addonsLayer.Status.Conditions[length-1]
		if last.Reason == reason && last.Message == message && last.Type == status {
			return
		}
		last.Status = corev1.ConditionFalse
	}

	l.addonsLayer.Status.Conditions = append(l.addonsLayer.Status.Conditions, kraanv1alpha1.Condition{
		Type:               status,
		Version:            l.addonsLayer.Spec.Version,
		Status:             corev1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	})
	l.trimConditions()
	l.addonsLayer.Status.State = status
	l.addonsLayer.Status.Version = l.addonsLayer.Spec.Version
	l.updated = true
	l.requeue = true
}

// SetStatusK8sVersion sets the addon layer's status to waiting for required K8s Version.
func (l *KraanLayer) SetStatusK8sVersion() {
	utils.TraceCall(l.GetLogger())
	defer utils.TraceExit(l.GetLogger())
	l.setStatus(kraanv1alpha1.K8sVersionCondition,
		kraanv1alpha1.AddonsLayerK8sVersionReason, kraanv1alpha1.AddonsLayerK8sVersionMsg)
}

// SetStatusDeployed sets the addon layer's status to deployed.
func (l *KraanLayer) SetStatusDeployed() {
	utils.TraceCall(l.GetLogger())
	defer utils.TraceExit(l.GetLogger())
	l.setStatus(kraanv1alpha1.DeployedCondition,
		fmt.Sprintf("AddonsLayer version %s is Deployed", l.GetSpec().Version), "")
}

// SetStatusApplying sets the addon layer's status to apply in progress.
func (l *KraanLayer) SetStatusApplying() {
	utils.TraceCall(l.GetLogger())
	defer utils.TraceExit(l.GetLogger())
	l.setStatus(kraanv1alpha1.ApplyingCondition,
		kraanv1alpha1.AddonsLayerApplyingReason, kraanv1alpha1.AddonsLayerApplyingMsg)
}

// SetStatusPruning sets the addon layer's status to pruning.
func (l *KraanLayer) SetStatusPruning() {
	utils.TraceCall(l.GetLogger())
	defer utils.TraceExit(l.GetLogger())
	l.setStatus(kraanv1alpha1.PruningCondition,
		kraanv1alpha1.AddonsLayerPruningReason, kraanv1alpha1.AddonsLayerPruningMsg)
}

// SetStatusPending sets the addon layer's status to pending.
func (l *KraanLayer) SetStatusPending() {
	utils.TraceCall(l.GetLogger())
	defer utils.TraceExit(l.GetLogger())
	reason := fmt.Sprintf("waiting for layer data to be available.")
	message := fmt.Sprintf("Layer source: %s, not yet available.", l.GetSourceKey())
	l.setStatus(kraanv1alpha1.FailedCondition, reason, message)
}

// GetSourceKey gets the namespace and name of the source used by layer.
func (l *KraanLayer) GetSourceKey() string {
	return fmt.Sprintf("%s/%s", l.GetSpec().Source.NameSpace, l.GetSpec().Source.Name)
}

// StatusUpdate sets the addon layer's status.
func (l *KraanLayer) StatusUpdate(status, reason, message string) {
	utils.TraceCall(l.GetLogger())
	defer utils.TraceExit(l.GetLogger())
	l.setStatus(status, reason, message)
}

// IsHold returns hold status.
func (l *KraanLayer) IsHold() bool {
	return l.addonsLayer.Spec.Hold
}

func getNameVersion(nameVersion string) (string, string) {
	parts := strings.Split(nameVersion, "@")
	if len(parts) < 2 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

func (l *KraanLayer) RevisionReady(conditions []meta.Condition, revision string) (bool, string) {
	utils.TraceCall(l.GetLogger())
	defer utils.TraceExit(l.GetLogger())
	for _, cond := range conditions {
		if cond.Type == "Ready" {
			return cond.Status == corev1.ConditionTrue && strings.Contains(cond.Message, revision), cond.Message
		}
	}
	return false, "GitRepository not yet reconciled"
}

func (l *KraanLayer) isOtherDeployed(otherVersion string, otherLayer *kraanv1alpha1.AddonsLayer) bool { // nolint: funlen // ok
	utils.TraceCall(l.GetLogger())
	defer utils.TraceExit(l.GetLogger())
	l.GetLogger().V(1).Info("checking dependency", append(utils.GetFunctionAndSource(utils.MyCaller), "dependson", otherLayer.Name, "layer", l.GetName())...)
	if otherLayer.Status.ObservedGeneration != otherLayer.Generation {
		l.GetLogger().V(2).Info("waiting for observed generation", "dependson", otherLayer.Name,
			"observed", otherLayer.Status.ObservedGeneration, "generation", otherLayer.Generation, "layer", l.GetName())
		reason := fmt.Sprintf("waiting for layer: %s, version: %s to be applied.", otherLayer.ObjectMeta.Name, otherVersion)
		message := fmt.Sprintf("Layer: %s, observed generation: %d, generation: %d", otherLayer.ObjectMeta.Name, otherLayer.Status.ObservedGeneration, otherLayer.Generation)
		l.setStatus(kraanv1alpha1.ApplyPendingCondition, reason, message)
		return false
	}
	if otherLayer.Status.Version != otherVersion {
		l.GetLogger().V(2).Info("waiting for version", append(utils.GetFunctionAndSource(utils.MyCaller), "dependson", otherLayer.Name,
			"version", otherLayer.Status.Version, "required", otherVersion, "layer", l.GetName())...)
		reason := fmt.Sprintf("waiting for layer: %s, version: %s to be applied.", otherLayer.ObjectMeta.Name, otherVersion)
		message := fmt.Sprintf("Layer: %s, current version is: %s, require version: %s.",
			otherLayer.ObjectMeta.Name, otherLayer.Status.Version, otherVersion)
		l.setStatus(kraanv1alpha1.ApplyPendingCondition, reason, message)
		return false
	}
	otherSource, err := l.getSource(otherLayer.Spec.Source.NameSpace, otherLayer.Spec.Source.Name)
	if err != nil {
		reason := fmt.Sprintf("unable to obtain source revision for layer: %s", otherLayer.ObjectMeta.Name)
		message := err.Error()
		l.setStatus(kraanv1alpha1.FailedCondition, reason, message)
		return false
	}
	if otherSource.Status.ObservedGeneration != otherSource.Generation {
		l.GetLogger().V(2).Info("waiting for source generation", append(utils.GetFunctionAndSource(utils.MyCaller),
			"dependson", otherLayer.Name, "source", otherSource.ObjectMeta.Name,
			"observed", otherSource.Status.ObservedGeneration, "generation", otherSource.Generation, "layer", l.GetName())...)
		reason := fmt.Sprintf("waiting for layer: %s, source: %s, to be reconciled.",
			otherLayer.ObjectMeta.Name, otherSource.ObjectMeta.Name)
		message := fmt.Sprintf("Layer: %s, source: %s, observed generation: %d, generation: %d",
			otherLayer.ObjectMeta.Name, otherSource.ObjectMeta.Name, otherSource.Status.ObservedGeneration, otherSource.Generation)
		l.setStatus(kraanv1alpha1.ApplyPendingCondition, reason, message)
		return false
	}

	revision := "not set"
	if otherSource.Status.Artifact != nil {
		revision = otherSource.Status.Artifact.Revision
	}
	ready, srcMsg := l.RevisionReady(otherSource.Status.Conditions, revision)
	if !ready {
		l.GetLogger().V(2).Info("waiting for source to be ready", append(utils.GetFunctionAndSource(utils.MyCaller),
			"dependson", otherLayer.Name, "source", otherSource.ObjectMeta.Name,
			"deployed", otherLayer.Status.DeployedRevision, "revision", revision, "layer", l.GetName())...)
		reason := fmt.Sprintf("waiting for layer: %s, layer source: %s not ready.", otherLayer.ObjectMeta.Name, otherSource.ObjectMeta.Name)
		message := fmt.Sprintf("Layer: %s, source: %s, status: %s.", otherLayer.ObjectMeta.Name, otherSource.ObjectMeta.Name, srcMsg)
		l.setStatus(kraanv1alpha1.ApplyPendingCondition, reason, message)
		return false
	}

	if otherLayer.Status.DeployedRevision != otherSource.Status.Artifact.Revision {
		l.GetLogger().V(2).Info("waiting for source revision", append(utils.GetFunctionAndSource(utils.MyCaller),
			"dependson", otherLayer.Name, "source", otherSource.ObjectMeta.Name,
			"deployed", otherLayer.Status.DeployedRevision, "revision", otherSource.Status.Artifact.Revision, "layer", l.GetName())...)
		reason := fmt.Sprintf("waiting for layer: %s, to apply source revision: %s.", otherLayer.ObjectMeta.Name, otherSource.Status.Artifact.Revision)
		message := fmt.Sprintf("Layer: %s, current state: %s, deployed revision: %s.", otherLayer.ObjectMeta.Name, otherLayer.Status.State, otherLayer.Status.DeployedRevision)
		l.setStatus(kraanv1alpha1.ApplyPendingCondition, reason, message)
		return false
	}
	if otherLayer.Status.State != kraanv1alpha1.DeployedCondition {
		l.GetLogger().V(2).Info("waiting for deployed", append(utils.GetFunctionAndSource(utils.MyCaller),
			"dependson", otherLayer.Name, "state", otherLayer.Status.State, "layer", l.GetName())...)
		reason := fmt.Sprintf("waiting for layer: %s, version: %s to be applied.", otherLayer.ObjectMeta.Name, otherVersion)
		message := fmt.Sprintf("Layer: %s, current state: %s.", otherLayer.ObjectMeta.Name, otherLayer.Status.State)
		l.setStatus(kraanv1alpha1.ApplyPendingCondition, reason, message)
		return false
	}
	return true
}

// DependenciesDeployed checks that all the layers this layer is dependent on are deployed.
func (l *KraanLayer) DependenciesDeployed() bool {
	utils.TraceCall(l.GetLogger())
	defer utils.TraceExit(l.GetLogger())
	for _, otherNameVersion := range l.GetSpec().PreReqs.DependsOn {
		otherName, otherVersion := getNameVersion(otherNameVersion)
		otherLayer, err := l.getOtherAddonsLayer(otherName)
		if err != nil {
			l.StatusUpdate(kraanv1alpha1.FailedCondition, kraanv1alpha1.AddonsLayerFailedReason, err.Error())
			return false
		}
		if !l.isOtherDeployed(otherVersion, otherLayer) {
			return false
		}
	}
	return true
}

// IsUpdated returns true if an update to the AddonsLayer data has occurred.
func (l *KraanLayer) IsUpdated() bool {
	return l.updated
}

// IsDelayed returns true if the requeue should be delayed.
func (l *KraanLayer) IsDelayed() bool {
	return l.delayed
}

// NeedsRequeue returns true if the AddonsLayer needed to be reprocessed.
func (l *KraanLayer) NeedsRequeue() bool {
	return l.requeue
}

// GetDelay returns the delay period.
func (l *KraanLayer) GetDelay() time.Duration {
	return l.delay
}

// GetStatus returns the status.
func (l *KraanLayer) GetStatus() string {
	return l.addonsLayer.Status.State
}

// SetHold sets the hold status.
func (l *KraanLayer) SetHold() {
	utils.TraceCall(l.GetLogger())
	defer utils.TraceExit(l.GetLogger())
	if l.IsHold() && l.GetStatus() != kraanv1alpha1.HoldCondition {
		l.StatusUpdate(kraanv1alpha1.HoldCondition,
			kraanv1alpha1.AddonsLayerHoldReason, kraanv1alpha1.AddonsLayerHoldMsg)
		l.updated = true
	}
}

// GetContext gets the context.
func (l *KraanLayer) GetContext() context.Context {
	return l.ctx
}

// GetLogger gets the layer logger.
func (l *KraanLayer) GetLogger() logr.Logger {
	return l.log
}

// GetName gets the layer name.
func (l *KraanLayer) GetName() string {
	return l.addonsLayer.ObjectMeta.Name
}

// getOtherAddonsLayer returns another addonsLayer.
func (l *KraanLayer) getOtherAddonsLayer(name string) (*kraanv1alpha1.AddonsLayer, error) {
	utils.TraceCall(l.GetLogger())
	defer utils.TraceExit(l.GetLogger())
	obj := &kraanv1alpha1.AddonsLayer{}
	if err := l.client.Get(l.GetContext(), types.NamespacedName{Name: name}, obj); err != nil {
		return nil, errors.Wrapf(err, "failed to retrieve layer: %s", types.NamespacedName{Name: name})
	}
	return obj, nil
}

// getSource returns a GitRepository Source.
func (l *KraanLayer) getSource(namespace, name string) (*sourcev1.GitRepository, error) {
	utils.TraceCall(l.GetLogger())
	defer utils.TraceExit(l.GetLogger())
	gitRepo := &sourcev1.GitRepository{}
	if err := l.client.Get(l.GetContext(), types.NamespacedName{Namespace: namespace, Name: name}, gitRepo); err != nil {
		return nil, errors.Wrapf(err, "failed to retrieve source: %s", types.NamespacedName{Namespace: namespace, Name: name})
	}
	return gitRepo, nil
}
