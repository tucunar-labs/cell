package canarynode

import "testing"

// Every independently deployable bird must have a functional executor. Swift,
// Canary and Signal Store have dedicated runtime paths; the remaining birds
// use the capability HTTP contract and are guarded here against regressing to
// the old placeholder implementation.
func TestIndependentBirdsHaveFunctionalExecutors(t *testing.T) {
	ids := []CapabilityID{
		capabilityKingfisher,
		capabilityKingbird,
		capabilityPigeon,
		capabilityWoodpecker,
		capabilityHeron,
		capabilityEagle,
		capabilityKestrel,
		capabilityHawk,
		capabilityRaven,
		capabilityPhoenix,
		capabilityLark,
		capabilityOwl,
		capabilityToucan,
		capabilityAIReasoning,
	}

	for _, id := range ids {
		t.Run(string(id), func(t *testing.T) {
			definition, ok := capabilityDefinition(id)
			if !ok {
				t.Fatalf("capability definition %q is missing", id)
			}
			executor := capabilityExecutorFor(definition)
			if _, ok := executor.(functionalCapability); !ok {
				t.Fatalf("capability %q uses %T, want functionalCapability", id, executor)
			}
		})
	}
}
