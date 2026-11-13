// Attaching the drafted storage nodes through Multus NetworkAttachmentDefinitions
// instead of host networking, when the run asks for it.
//
// A run names the definitions, and the draft names them in place of interfaces
// for every group: the two modes do not mix within a document. What is checked
// here is what the probe reports can answer. The definition has to exist in the
// namespace, and the host interface it builds on has to exist on the worker,
// since a macvlan, ipvlan, or host-device on a NIC the machine does not have
// leaves its pod stuck in ContainerCreating. A bridge plugin creates its bridge,
// and a definition that names no host interface is placed by the scheduler
// through a device plugin resource, so neither is held against a worker.
//
// A cluster that does not serve the kind at all has no Multus, and the run then
// drafts host networking and says so: the administrator asked for something the
// cluster cannot do, and a draft of what it can do is more use to them than none.

package discovery

import (
	"fmt"
	"slices"
	"strings"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

const (
	// NetworkAttachmentRule is the rule a worker is refused under when a
	// definition the run names cannot attach it.
	NetworkAttachmentRule = "network attachment"

	// NetworkAttachmentsUnavailable names a run that asked for attachments on a
	// cluster that does not serve the NetworkAttachmentDefinition kind.
	NetworkAttachmentsUnavailable = "NetworkAttachmentsUnavailable"
)

// The states a probe reports the definitions in, in nodeprobe's spelling.
const (
	attachmentsServed    = "Served"
	attachmentsNotServed = "NotServed"
)

// NetworkingOf is the attachment the draft names for every group, or nil for
// host networking, with the finding a fallback owes the run.
//
// The kind is the cluster's, so the fleet's reports are read together: only a
// fleet where no probe found the kind served and at least one found it not
// served falls back. A probe that could not read the definitions says nothing
// about the cluster, and its worker is refused by AdmitAttachment instead.
func NetworkingOf(run *simplyblockv1alpha2.DiscoverSpec, reports []nodeprobe.Report) (*simplyblockv1alpha2.NetworkAttachmentSelection, []Finding) {
	if run == nil || run.NetworkAttachment == nil {
		return nil, nil
	}
	served, notServed := 0, 0
	for _, report := range reports {
		if report.NetworkAttachments == nil {
			continue
		}
		switch report.NetworkAttachments.State {
		case attachmentsServed:
			served++
		case attachmentsNotServed:
			notServed++
		}
	}
	if served == 0 && notServed > 0 {
		return nil, []Finding{{
			Reason: NetworkAttachmentsUnavailable,
			Message: fmt.Sprintf("spec.discover.networkAttachment names %s, but the cluster does not serve "+
				"NetworkAttachmentDefinitions (Multus is not installed), so the draft keeps host networking",
				strings.Join(attachmentNames(run.NetworkAttachment), " and ")),
		}}
	}
	selection := *run.NetworkAttachment
	return &selection, nil
}

// attachmentNames is the definitions a selection names, management first.
func attachmentNames(selection *simplyblockv1alpha2.NetworkAttachmentSelection) []string {
	names := []string{selection.Management}
	if selection.Data != "" && selection.Data != selection.Management {
		names = append(names, selection.Data)
	}
	return names
}

// AdmitAttachment reports whether every definition the selection names can
// attach a pod on the worker, and why not.
func AdmitAttachment(report nodeprobe.Report, selection *simplyblockv1alpha2.NetworkAttachmentSelection) (bool, string) {
	attachments := report.NetworkAttachments
	switch {
	case attachments == nil:
		return false, "the probe did not read the NetworkAttachmentDefinitions, so the attachment cannot be checked"
	case attachments.State != attachmentsServed:
		return false, fmt.Sprintf("the probe could not read the NetworkAttachmentDefinitions (%s): %s",
			attachments.State, attachments.Detail)
	}

	for _, name := range attachmentNames(selection) {
		definition, found := report.NetworkAttachment(name)
		if !found {
			namespace := orNone(attachments.Namespace, "of the run")
			if len(attachments.Definitions) == 0 {
				return false, fmt.Sprintf("the namespace %s holds no NetworkAttachmentDefinition %s, and holds none at all",
					namespace, name)
			}
			return false, fmt.Sprintf("the namespace %s holds no NetworkAttachmentDefinition %s, only %s",
				namespace, name, heldDefinitions(attachments))
		}
		if ok, why := hostCarries(report, definition); !ok {
			return false, why
		}
	}
	return true, ""
}

// hostCarries reports whether the worker has the host interface a definition
// builds on.
func hostCarries(report nodeprobe.Report, definition nodeprobe.NetworkAttachment) (bool, string) {
	switch {
	case definition.Type == "bridge":
		return true, ""
	case definition.HostInterface != "":
		if slices.ContainsFunc(report.Interfaces, func(i nodeprobe.Interface) bool { return i.Name == definition.HostInterface }) {
			return true, ""
		}
		return false, fmt.Sprintf("the NetworkAttachmentDefinition %s (%s) builds on %s, which this worker does not have",
			definition.Name, definition.Type, definition.HostInterface)
	case definition.PCIAddress != "":
		if slices.ContainsFunc(report.Interfaces, func(i nodeprobe.Interface) bool { return i.PCIAddress == definition.PCIAddress }) {
			return true, ""
		}
		return false, fmt.Sprintf("the NetworkAttachmentDefinition %s moves the NIC at %s into the pod, and this worker has none there",
			definition.Name, definition.PCIAddress)
	}
	return true, ""
}

// heldDefinitions lists the namespace's definitions for a refusal, which is
// what the run is corrected with.
func heldDefinitions(attachments *nodeprobe.NetworkAttachments) string {
	names := make([]string, 0, len(attachments.Definitions))
	for _, definition := range attachments.Definitions {
		names = append(names, definition.Name)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

func orNone(value, unknown string) string {
	if value == "" {
		return unknown
	}
	return value
}
