package smoke

import (
	"regexp"
	"strings"
)

// moduleTypes lists every resource type provision/terraform/azure creates, and
// is the one source of type coverage for the create re-read (settleCreates)
// and the orphan sweep (Claude01 ruling 6ae76c16). A test keeps it equal to the
// module, so a new type cannot slip past either. ARM is "" for a type that is
// not a top-level ARM resource of its own (a child or an association); it is
// neither re-read nor swept and goes with its parent. Sweep is the deletion
// order: a VM first, since it holds its NIC (run 7e: NicInUse) and OS disk.
var moduleTypes = []struct {
	TF, ARM string
	Sweep   int
}{
	{"azurerm_linux_virtual_machine", "Microsoft.Compute/virtualMachines", 0},
	{"azurerm_network_interface", "Microsoft.Network/networkInterfaces", 1},
	{"azurerm_public_ip", "Microsoft.Network/publicIPAddresses", 2},
	{"azurerm_managed_disk", "Microsoft.Compute/disks", 3}, // also each VM's OS disk
	{"azurerm_virtual_network", "Microsoft.Network/virtualNetworks", 4},
	{"azurerm_network_security_group", "Microsoft.Network/networkSecurityGroups", 5},
	{"azurerm_subnet", "", 0},
	{"azurerm_subnet_network_security_group_association", "", 0},
	{"azurerm_virtual_machine_data_disk_attachment", "", 0},
}

// armType is the ARM type of a module Terraform type, or "".
func armType(tf string) string {
	for _, m := range moduleTypes {
		if m.TF == tf {
			return m.ARM
		}
	}
	return ""
}

// sweepRank is the deletion rank of an ARM type (any case).
func sweepRank(arm string) (int, bool) {
	for _, m := range moduleTypes {
		if m.ARM != "" && strings.EqualFold(m.ARM, arm) {
			return m.Sweep, true
		}
	}
	return 0, false
}

// lostCreateForms is a create PUT that lost its response, for every top-level
// module type: the PUT names an ARM ID of that type and the block's address is
// of the matching Terraform type (run 7e lost a VM create, api 2024-03-01).
func lostCreateForms() string {
	uuid := `[a-f0-9-]{36}`
	var forms []string
	for _, m := range moduleTypes {
		if m.ARM == "" {
			continue
		}
		forms = append(forms, `(?:creating|updating) [A-Z][A-Za-z ]* \(Subscription: "`+uuid+`"(?:\n[A-Z][A-Za-z ]* Name: "[A-Za-z0-9_-]+")+\): performing CreateOrUpdate: Put "https://management\.azure\.com/subscriptions/`+uuid+
			`/resourceGroups/[A-Za-z0-9_-]+/providers/`+regexp.QuoteMeta(m.ARM)+`/[A-Za-z0-9_-]+\?api-version=[0-9]{4}-[0-9]{2}-[0-9]{2}": HTTP response was nil; connection may have been reset`+
			`\n\n  with module\.run\.`+m.TF+`\.[a-z_]+(?:\["[a-z0-9-]+"\])?,\n  on \.\./azure/main\.tf line [0-9]+, in resource "`+m.TF+`" "[a-z_]+":\n  [0-9]+: resource "`+m.TF+`" "[a-z_]+" \{`)
	}
	return strings.Join(forms, "|")
}
