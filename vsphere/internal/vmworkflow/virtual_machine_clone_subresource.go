package vmworkflow

import (
	"fmt"
	"log"

	"github.com/hashicorp/terraform/helper/schema"
	"github.com/terraform-providers/terraform-provider-vsphere/vsphere/internal/helper/datastore"
	"github.com/terraform-providers/terraform-provider-vsphere/vsphere/internal/helper/virtualmachine"
	"github.com/terraform-providers/terraform-provider-vsphere/vsphere/internal/virtualdevice"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// VirtualMachineCloneSchema represents the schema for the VM clone sub-resource.
//
// This is a workflow for vsphere_virtual_machine that facilitates the creation
// of a virtual machine through cloning from an existing template.
// Customization is nested here, even though it exists in its own workflow.
func VirtualMachineCloneSchema() map[string]*schema.Schema {
	return map[string]*schema.Schema{
		"template_uuid": {
			Type:        schema.TypeString,
			Required:    true,
			ForceNew:    true,
			Description: "The UUID of the source virtual machine or template.",
		},
		"linked_clone": {
			Type:        schema.TypeBool,
			Optional:    true,
			ForceNew:    true,
			Description: "Whether or not to create a linked clone when cloning. When this option is used, the source VM must have a single snapshot associated with it.",
		},
		"customize": {
			Type:        schema.TypeList,
			Optional:    true,
			MaxItems:    1,
			Description: "The customization spec for this clone. This allows the user to configure the virtual machine post-clone.",
		},
	}
}

// ValidateVirtualMachineClone does pre-creation validation of a virtual
// machine's configuration to make sure it's suitable for use in cloning.
// This includes, but is not limited to checking to make sure that the disks in
// the new VM configuration line up with the configuration in the existing
// template, and checking to make sure that the VM has a single snapshot we can
// use in the even that linked clones are enabled.
func ValidateVirtualMachineClone(d *schema.ResourceDiff, c *govmomi.Client) error {
	tUUID := d.Get("clone.0.template_uuid").(string)
	log.Printf("[DEBUG] ValidateVirtualMachineClone: Validating fitness of source VM/template %s", tUUID)
	vm, err := virtualmachine.FromUUID(c, tUUID)
	if err != nil {
		return fmt.Errorf("cannot locate virtual machine or template with UUID %q: %s", tUUID, err)
	}
	vprops, err := virtualmachine.Properties(vm)
	if err != nil {
		return fmt.Errorf("error fetching virtual machine or template properties: %s", err)
	}
	// The virtual machine needs to be powered off to be suitable for cloning.
	if vprops.Runtime.PowerState != types.VirtualMachinePowerStatePoweredOff {
		return fmt.Errorf("virtual machine %s must be powered off to be used as a source for cloning", tUUID)
	}
	// If linked clone is enabled, check to see if we have a snapshot. There need
	// to be a single snapshot on the template for it to be eligible.
	if d.Get("clone.0.linked_clone").(bool) {
		log.Printf("[DEBUG] ValidateVirtualMachineClone: Checking snapshots on %s for linked clone eligibility", tUUID)
		if err := validateCloneSnapshots(vprops); err != nil {
			return err
		}
	}
	// Check to make sure the disks for this VM/template line up with the disks
	// in the configuration. This is in the virtual device package, so pass off
	// to that now.
	l := object.VirtualDeviceList(vprops.Config.Hardware.Device)
	if err := virtualdevice.DiskCloneValidateOperation(d, c, l); err != nil {
		return err
	}
	log.Printf("[DEBUG] ValidateVirtualMachineClone: Source VM/template %s is a suitable source for cloning", tUUID)
	return nil
}

// validateCloneSnapshots checks a VM to make sure it has a single snapshot
// with no children, to make sure there is no ambiguity when selecting a
// snapshot for linked clones.
func validateCloneSnapshots(props *mo.VirtualMachine) error {
	if props.Snapshot == nil {
		return fmt.Errorf("virtual machine or template %s must have a snapshot to be used as a linked clone", props.Config.Uuid)
	}
	// Root snapshot list can only have a singular element
	if len(props.Snapshot.RootSnapshotList) != 1 {
		return fmt.Errorf("virtual machine or template %s must have exactly one root snapshot (has: %d)", props.Config.Uuid, len(props.Snapshot.RootSnapshotList))
	}
	// Check to make sure the root snapshot has no children
	if len(props.Snapshot.RootSnapshotList[0].ChildSnapshotList) > 0 {
		return fmt.Errorf("virtual machine or template %s's root snapshot must not have children", props.Config.Uuid)
	}
	// Current snapshot must match root snapshot (this should be the case anyway)
	if props.Snapshot.CurrentSnapshot.Value != props.Snapshot.RootSnapshotList[0].Snapshot.Value {
		return fmt.Errorf("virtual machine or template %s's current snapshot must match root snapshot", props.Config.Uuid)
	}
	return nil
}

// ExpandVirtualMachineCloneSpec creates a clone spec for an existing virtual machine.
//
// The clone spec built by this function for the clone contains the target
// datastore, the source snapshot in the event of linked clones, and a relocate
// spec that contains the new locations and configuration details of the new
// virtual disks.
func ExpandVirtualMachineCloneSpec(d *schema.ResourceData, c *govmomi.Client) (types.VirtualMachineCloneSpec, error) {
	var spec types.VirtualMachineCloneSpec
	log.Printf("[DEBUG] ExpandVirtualMachineCloneSpec: Preparing clone spec for VM")
	ds, err := datastore.FromID(c, d.Get("datastore_id").(string))
	if err != nil {
		return spec, fmt.Errorf("error locating datastore for VM: %s", err)
	}
	dsRef := ds.Reference()
	spec.Location.Datastore = &dsRef
	tUUID := d.Get("clone.0.template_uuid").(string)
	log.Printf("[DEBUG] ExpandVirtualMachineCloneSpec: Cloning from UUID: %s", tUUID)
	vm, err := virtualmachine.FromUUID(c, tUUID)
	if err != nil {
		return spec, fmt.Errorf("cannot locate virtual machine or template with UUID %q: %s", tUUID, err)
	}
	vprops, err := virtualmachine.Properties(vm)
	if err != nil {
		return spec, fmt.Errorf("error fetching virtual machine or template properties: %s", err)
	}
	// If we are creating a linked clone, grab the current snapshot of the
	// source, and populate the appropriate field. This should have already been
	// validated, but just in case, validate it again here.
	if d.Get("clone.0.linked_clone").(bool) {
		log.Printf("[DEBUG] ExpandVirtualMachineCloneSpec: Clone type is a linked clone")
		log.Printf("[DEBUG] ExpandVirtualMachineCloneSpec: Fetching snapshot for VM/template UUID %s", tUUID)
		if err := validateCloneSnapshots(vprops); err != nil {
			return spec, err
		}
		spec.Snapshot = vprops.Snapshot.CurrentSnapshot
		spec.Location.DiskMoveType = string(types.VirtualMachineRelocateDiskMoveOptionsCreateNewChildDiskBacking)
		log.Printf("[DEBUG] ExpandVirtualMachineCloneSpec: Snapshot for clone: %s", vprops.Snapshot.CurrentSnapshot.Value)
	}
	// Grab the relocate spec for the disks.
	l := object.VirtualDeviceList(vprops.Config.Hardware.Device)
	relocators, err := virtualdevice.DiskCloneRelocateOperation(d, c, l)
	if err != nil {
		return spec, err
	}
	spec.Location.Disk = relocators
	log.Printf("[DEBUG] ExpandVirtualMachineCloneSpec: Clone spec prep complete")
	return spec, nil
}
