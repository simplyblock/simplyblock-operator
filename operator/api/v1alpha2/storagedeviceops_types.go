// StorageDeviceOps: one operation performed against one StorageDevice.
//
// It is the narrowest blast radius in the ownership spine. A wedged device is
// recycled today by restarting its storage node, which takes every other device
// on that node with it and costs the cluster a node's worth of redundancy for
// the duration; one device is the narrowest thing that can be recycled, and
// choosing the narrowest resource that achieves an outcome is the rule
// design-crd-model.md §8.2 states.
//
// design-storagedevice.md §6 specifies five actions. One is served by the
// control plane's v2 API and is built; the other four are blocked on verbs that
// API does not offer, and the TODO beside their constants is the ask.
//
// **The enum admits only what the operator can perform.** Declaring the other
// four now would accept an object whose first reconcile can only fail, and an
// API that takes a request it will never carry out is worse than one that
// refuses it at admission — the refusal names the missing capability, and the
// failure names nothing a user can act on. Widening an enum is additive, so
// each action arrives with the endpoint it needs.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/statemachine"
)

// StorageDeviceOpsAction is the operation a StorageDeviceOps performs.
//
// There is no Add: a device that appears is discovered (§5.1). There is no bare
// Remove either: a removal is a step of Replace and of Migrate, and taking a
// device out of the data path without replacing it is Fail (§6.4).
// +kubebuilder:validation:Enum=Restart
type StorageDeviceOpsAction string

const (
	// StorageDeviceOpsActionRestart is the action the kind exists for:
	// recycling one device rather than its node.
	StorageDeviceOpsActionRestart StorageDeviceOpsAction = "Restart"

	// TODO(storagedeviceops): EXTERNAL DEPENDENCY — the four actions below wait
	// on control-plane verbs the v2 API does not offer. It serves restart,
	// remove, and reset where design-storagedevice.md §7 asks for seven, and
	// remove buys no action on its own: it is a step of Replace and of Migrate,
	// and both also need the adopt call that names the device arriving.
	//
	//	SelfTest  POST /api/v2/clusters/{c}/storage-nodes/{n}/devices/{d}/self-test
	//	          runs the device's own self-test and reports the verdict, with
	//	          the short or extended mode in the body.
	//	Fail      POST .../devices/{d}/fail
	//	          takes a device out of the data path and leaves it in the slot,
	//	          so the cluster rebuilds its redundancy elsewhere and stops
	//	          reading from a device somebody has judged untrustworthy.
	//	Replace   POST .../devices/adopt
	//	          names the device that arrived; the removal verb exists and the
	//	          pairing is what makes the arrival identifiable.
	//	Migrate   POST .../devices/{d}/detach, and the adopt above accepting a
	//	          device WITH ITS CONTENTS. An adopt that can only take an empty
	//	          device turns Migrate into two Replaces and a full rebuild,
	//	          which is what §6.2 gives as the action's reason to exist. This
	//	          is the row whose absence removes an action rather than
	//	          degrading it.
	//
	// The constants are declared and absent from the Enum marker above, so the
	// names exist where the reason does while an object naming one is refused at
	// admission rather than accepted and failed.
	StorageDeviceOpsActionSelfTest StorageDeviceOpsAction = "SelfTest"
	StorageDeviceOpsActionFail     StorageDeviceOpsAction = "Fail"
	StorageDeviceOpsActionReplace  StorageDeviceOpsAction = "Replace"
	StorageDeviceOpsActionMigrate  StorageDeviceOpsAction = "Migrate"
)

// StorageDeviceOpsPhase is the operation's own progress.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Aborted
type StorageDeviceOpsPhase string

const (
	StorageDeviceOpsPhasePending   StorageDeviceOpsPhase = "Pending"
	StorageDeviceOpsPhaseRunning   StorageDeviceOpsPhase = "Running"
	StorageDeviceOpsPhaseSucceeded StorageDeviceOpsPhase = "Succeeded"
	StorageDeviceOpsPhaseFailed    StorageDeviceOpsPhase = "Failed"
	StorageDeviceOpsPhaseAborted   StorageDeviceOpsPhase = "Aborted"
)

// StorageDeviceOpsStep is one step of a running device operation. Which steps
// belong to which action is declared by that action's graph rather than by this
// type, which is why the enum stays flat as actions are added.
//
// It carries the two steps Restart has. The eight the other four actions need
// arrive with those actions, because a step no graph declares is a status value
// nothing can resume from and a CEL rule that admits one is a rule that admits
// nonsense.
// +kubebuilder:validation:Enum=Requesting;Awaiting
type StorageDeviceOpsStep string

const (
	// StorageDeviceOpsStepRequesting issues the call.
	StorageDeviceOpsStepRequesting StorageDeviceOpsStep = "Requesting"

	// StorageDeviceOpsStepAwaiting waits for the control plane to report the
	// device in the state the call asked for.
	StorageDeviceOpsStepAwaiting StorageDeviceOpsStep = "Awaiting"
)

// StorageDeviceOpsSpec is one operation to perform against one StorageDevice.
type StorageDeviceOpsSpec struct {
	// DeviceRef names the StorageDevice this operation acts on, in this
	// operation's own namespace. The operation never owns its target, because
	// deleting the record of an operation must not delete the device record it
	// operated on.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Required
	// +k8s:immutable
	DeviceRef string `json:"deviceRef"`

	// Action is the operation to perform.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Action StorageDeviceOpsAction `json:"action"`

	// Abort asks a running operation to stop at its next step and unwind.
	//
	// Restart can be aborted before its call is issued and not after: a restart
	// the control plane has accepted is one nothing can recall, so the graph
	// declares where the edge exists rather than this field promising one.
	// +optional
	Abort bool `json:"abort,omitempty"`
}

// StorageDeviceOpsStatus is the observed state of one device operation.
type StorageDeviceOpsStatus struct {
	// Phase is the operation's own progress.
	// +optional
	Phase StorageDeviceOpsPhase `json:"phase,omitempty"`

	// Step is the position of the running action's state machine. It is
	// persisted before the side effect that step performs.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Requesting','Awaiting']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// DeviceStatusBefore is what the control plane reported the device's status
	// to be when the operation took its lock, so a wait can tell the device
	// coming back from its never having gone.
	// +optional
	DeviceStatusBefore string `json:"deviceStatusBefore,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the operation moves, and never a log.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation the rest of this status was computed
	// from, so a stale status can be told from a current one.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// StartedAt is when the operation acquired its target's lock.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when it reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// v1alpha2 is the only version this kind has ever had, so it is the storage
// version and converts nothing.
// +kubebuilder:storageversion
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sdops
// +kubebuilder:printcolumn:name="Device",type=string,JSONPath=".spec.deviceRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageDeviceOps is a single operation performed against one StorageDevice.
type StorageDeviceOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageDeviceOpsSpec   `json:"spec,omitempty"`
	Status StorageDeviceOpsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StorageDeviceOpsList contains a list of StorageDeviceOps.
type StorageDeviceOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageDeviceOps `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageDeviceOps{}, &StorageDeviceOpsList{})
}
