// Package v1alpha1 defines the smoke-test API.
// +kubebuilder:object:generate=true
// +groupName=smoketest.turnkey.engineering
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var GroupVersion = schema.GroupVersion{Group: "smoketest.turnkey.engineering", Version: "v1alpha1"}
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
var AddToScheme = SchemeBuilder.AddToScheme

func init() {
	SchemeBuilder.Register(&SmokeTest{}, &SmokeTestList{}, &SmokeTestRun{}, &SmokeTestRunList{})
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type SmokeTest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SmokeTestSpec `json:"spec"`
}

type SmokeTestSpec struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	Stages []Stage `json:"stages"`
}

type Stage struct {
	Name      string   `json:"name"`
	DependsOn []string `json:"dependsOn,omitempty"`
	// +kubebuilder:default="5m"
	Timeout        string       `json:"timeout,omitempty"`
	CreatePod      *PodAction   `json:"createPod,omitempty"`
	CreatePVC      *PVCAction   `json:"createPVC,omitempty"`
	DeleteResource *ResourceRef `json:"deleteResource,omitempty"`
	Assert         []Assertion  `json:"assert,omitempty"`
}
type PodAction struct {
	Template corev1.PodTemplateSpec `json:"template"`
}
type PVCAction struct {
	Template corev1.PersistentVolumeClaimTemplate `json:"template"`
}
type ResourceRef struct {
	FromStage string `json:"fromStage"`
	// +kubebuilder:validation:Enum="";scheduledNode;boundPV;volumeAttachment
	Relation string `json:"relation,omitempty"`
}
type Assertion struct {
	// +kubebuilder:validation:Enum=ResourceStatus;NodePoolEmpty;InitiallyUnschedulable;AutoscalerScaleUpObserved;ScheduledOnNewNode;VolumeBound;VolumeAttached;WebIdentity;ProbeResult
	Type         string            `json:"type"`
	Resource     *ResourceRef      `json:"resource,omitempty"`
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	Status       *ExpectedStatus   `json:"status,omitempty"`
}
type ExpectedStatus struct {
	Phase      string              `json:"phase,omitempty"`
	Conditions []ExpectedCondition `json:"conditions,omitempty"`
}
type ExpectedCondition struct {
	Type string `json:"type"`
	// +kubebuilder:validation:Enum=True;False;Unknown
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// +kubebuilder:object:root=true
type SmokeTestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SmokeTest `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.reason"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type SmokeTestRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Run spec is immutable; create a new Run to retry"
	Spec   SmokeTestRunSpec   `json:"spec"`
	Status SmokeTestRunStatus `json:"status,omitempty"`
}
type SmokeTestRunSpec struct {
	TestRef TestReference     `json:"testRef"`
	Context map[string]string `json:"context,omitempty"`
}
type TestReference struct {
	Name       string `json:"name"`
	UID        string `json:"uid,omitempty"`
	Generation int64  `json:"generation,omitempty"`
}
type SmokeTestRunStatus struct {
	Phase                string         `json:"phase,omitempty"`
	Reason               string         `json:"reason,omitempty"`
	Message              string         `json:"message,omitempty"`
	Definition           *SmokeTestSpec `json:"definition,omitempty"`
	DefinitionUID        string         `json:"definitionUID,omitempty"`
	DefinitionGeneration int64          `json:"definitionGeneration,omitempty"`
	DefinitionHash       string         `json:"definitionHash,omitempty"`
	ProbeImage           string         `json:"probeImage,omitempty"`
	ExpectedRoleARN      string         `json:"expectedRoleARN,omitempty"`
	Region               string         `json:"region,omitempty"`
	StartedAt            *metav1.Time   `json:"startedAt,omitempty"`
	Deadline             *metav1.Time   `json:"deadline,omitempty"`
	CleanupStartedAt     *metav1.Time   `json:"cleanupStartedAt,omitempty"`
	CompletedAt          *metav1.Time   `json:"completedAt,omitempty"`
	// +listType=map
	// +listMapKey=name
	Stages        []StageStatus    `json:"stages,omitempty"`
	Resources     []ResourceRecord `json:"resources,omitempty"`
	BaselineNodes []string         `json:"baselineNodes,omitempty"`
	BaselineAt    *metav1.Time     `json:"baselineAt,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
type StageStatus struct {
	Name        string       `json:"name"`
	Phase       string       `json:"phase"`
	StartedAt   metav1.Time  `json:"startedAt"`
	Deadline    metav1.Time  `json:"deadline"`
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	Message     string       `json:"message,omitempty"`
}
type ResourceRecord struct {
	Stage           string       `json:"stage"`
	Kind            string       `json:"kind"`
	Name            string       `json:"name"`
	UID             string       `json:"uid,omitempty"`
	CreatedAt       *metav1.Time `json:"createdAt,omitempty"`
	Deleted         bool         `json:"deleted,omitempty"`
	UnschedulableAt *metav1.Time `json:"unschedulableAt,omitempty"`
	ScaleUpAt       *metav1.Time `json:"scaleUpAt,omitempty"`
	NodeName        string       `json:"nodeName,omitempty"`
	NodeUID         string       `json:"nodeUID,omitempty"`
	PVName          string       `json:"pvName,omitempty"`
	PVUID           string       `json:"pvUID,omitempty"`
	VolumeHandle    string       `json:"volumeHandle,omitempty"`
	AttachmentName  string       `json:"attachmentName,omitempty"`
	AttachmentUID   string       `json:"attachmentUID,omitempty"`
	Attached        bool         `json:"attached,omitempty"`
	Result          *ProbeResult `json:"result,omitempty"`
}
type ProbeResult struct {
	Version   int    `json:"version"`
	RunUID    string `json:"runUID"`
	Stage     string `json:"stage"`
	Container string `json:"container"`
	Success   bool   `json:"success"`
	Checksum  string `json:"checksum,omitempty"`
	Account   string `json:"account,omitempty"`
	ARN       string `json:"arn,omitempty"`
	Error     string `json:"error,omitempty"`
}

// +kubebuilder:object:root=true
type SmokeTestRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SmokeTestRun `json:"items"`
}
