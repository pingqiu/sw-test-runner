package smoke

import "strings"

// absentShow is az's exact ResourceNotFound text for id, which reread treats
// as absent (the create was never completed in Azure).
func absentShow(id string) []byte {
	p := strings.Split(id, "/")
	line := "The Resource '" + p[6] + "/" + p[7] + "/" + p[8] + "' under resource group '" + p[4] + "' was not found. For more details please go to https://aka.ms/ARMResourceNotFoundFix"
	return []byte("ERROR: (ResourceNotFound) " + line + "\nCode: ResourceNotFound\nMessage: " + line)
}

// inScope moves a captured create form into the controller fixture's RG.
func inScope(message string) string {
	return strings.ReplaceAll(message, "fixture-rg", "dedicated-test")
}
