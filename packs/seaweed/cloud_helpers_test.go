package seaweed

import "testing"

// Run 7 failed in prepare because chaos_load.py was uploaded but not
// embedded. Every helper any cloud phase can upload must be in the embed.
func TestEveryCloudHelperIsEmbedded(t *testing.T) {
	for _, phase := range []string{"", "prepare"} {
		for _, set := range []string{"", "A", "B"} {
			for _, name := range cloudHelpers(phase, set) {
				if _, err := workerFiles.ReadFile("worker/" + name); err != nil {
					t.Fatalf("phase %q chaos_set %q uploads %s, which is not embedded: %v", phase, set, name, err)
				}
			}
		}
	}
}
