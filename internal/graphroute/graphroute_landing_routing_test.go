package graphroute

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
)

// The agent-forge-eskw regression: a graph.v2 workflow whose late controller
// step (the agentforge-minimal "merge" handoff) declares the per-step
// compile-time routing intent gc.run_target=core.control-dispatcher must get
// its ATTEMPT bead stamped gc.routed_to=<rig>/core.control-dispatcher at
// decoration time, so the control-dispatcher serve query
// (workflowServeControlReadyQueryForBeads's routed_ready leg) can select it.
// Unrouted, the attempt carries no assignee and an empty gc.routed_to and the
// lane sits ready-and-unrouted in finalization forever (624+ lanes stuck on
// poe-pricer / poe-craft-engine, 2026-09-11).
//
// The recipe below mirrors the compiled shape ApplyRetries produces for one
// retry-managed step: control (gc.kind=retry, inherits the authored metadata
// incl. gc.run_target), the frozen step spec (topology), and the first
// attempt (the actual work bead), decorated with an EMPTY default binding —
// exactly the live shape (the workflow root's delivery stamp is applied later
// by InstantiateSlingFormula's SlingResult path, so decoration sees no default
// route).
func landingRecipe() *formula.Recipe {
	return &formula.Recipe{
		Name: "agentforge-minimal",
		Steps: []formula.RecipeStep{
			{
				ID:     "agentforge-minimal.workflow",
				IsRoot: true,
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
					beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
				},
			},
			{
				// Verify attempt — pool-claimed executor work, NO authored
				// route target (the live pre-fix shape that must stay unrouted
				// at the dispatcher).
				ID: "agentforge-minimal.verify.attempt.1",
				Metadata: map[string]string{
					beadmeta.StepIDMetadataKey: "verify",
					"af.owner":                 "executor",
				},
			},
			{
				// Merge retry control — expandRetry clones the authored merge
				// step, inheriting its gc.run_target routing intent.
				ID: "agentforge-minimal.merge",
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:      beadmeta.KindRetry,
					beadmeta.StepIDMetadataKey:    "merge",
					beadmeta.RunTargetMetadataKey: "core.control-dispatcher",
					"af.owner":                    "controller",
				},
			},
			{
				ID: "agentforge-minimal.merge.spec",
				Metadata: map[string]string{
					beadmeta.KindMetadataKey: beadmeta.KindSpec,
				},
			},
			{
				// Merge attempt — the bead the dispatcher must see.
				ID: "agentforge-minimal.merge.attempt.1",
				Metadata: map[string]string{
					beadmeta.AttemptMetadataKey:    "1",
					beadmeta.StepIDMetadataKey:     "merge",
					beadmeta.ControlForMetadataKey: "merge",
					beadmeta.RunTargetMetadataKey:  "core.control-dispatcher",
					"af.owner":                     "controller",
				},
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "agentforge-minimal.verify.attempt.1", DependsOnID: "agentforge-minimal.merge", Type: "blocks"},
			{StepID: "agentforge-minimal.merge", DependsOnID: "agentforge-minimal.merge.attempt.1", Type: "blocks"},
		},
	}
}

// rigDispatcherConfig ships the rig-scoped dispatcher exactly the way the
// core pack expands it: a deterministic control-dispatcher bound to the rig
// (Dir), qualified identity <rig>/core.control-dispatcher.
func rigDispatcherConfig(rig string) *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "agentforge"},
		Agents: []config.Agent{
			{
				Name:        config.ControlDispatcherAgentName,
				BindingName: "core",
				Dir:         rig,
				Provider:    "",
				StartCommand: `sh -c 'exec "${GC_BIN:-gc}" convoy control --serve --follow ` +
					config.ControlDispatcherAgentName + `'`,
			},
		},
	}
}

func TestDecorateGraphWorkflowRecipe_MergeRunTargetStampsAttemptWithDispatcherRoute(t *testing.T) {
	const rig = "poe-pricer"
	cfg := rigDispatcherConfig(rig)
	r := landingRecipe()
	deps := Deps{Resolver: rigAwareDispatcherResolver{}}

	err := DecorateGraphWorkflowRecipeWithDefaultBinding(
		r, nil, "src-1", "city", "src-1", "rig:"+rig,
		GraphRouteBinding{MetadataOnly: true}, // empty default route — the live shape
		nil, "agentforge", cfg, deps,
	)
	if err != nil {
		t.Fatalf("DecorateGraphWorkflowRecipeWithDefaultBinding: %v", err)
	}

	byID := make(map[string]*formula.RecipeStep, len(r.Steps))
	for i := range r.Steps {
		byID[r.Steps[i].ID] = &r.Steps[i]
	}

	// THE fix: the merge attempt is dispatcher-routed, rig-qualified.
	attempt := byID["agentforge-minimal.merge.attempt.1"]
	if got := attempt.Metadata[beadmeta.RoutedToMetadataKey]; got != rig+"/core.control-dispatcher" {
		t.Fatalf("merge attempt gc.routed_to = %q, want %q (agent-forge-eskw: the landing step must be visible to exactly one controller)", got, rig+"/core.control-dispatcher")
	}
	if got := strings.TrimSpace(attempt.Assignee); got != "" {
		t.Fatalf("merge attempt assignee = %q, want empty (pool-routed, metadata-only)", got)
	}

	// The retry control keeps its control route — same dispatcher identity.
	control := byID["agentforge-minimal.merge"]
	if got := control.Metadata[beadmeta.RoutedToMetadataKey]; got != rig+"/core.control-dispatcher" {
		t.Fatalf("merge retry control gc.routed_to = %q, want %q", got, rig+"/core.control-dispatcher")
	}

	// The pool-claimed executor attempt stays unrouted for the dispatcher:
	// the empty default route must not grow a dispatcher stamp.
	executorAttempt := byID["agentforge-minimal.verify.attempt.1"]
	if got := executorAttempt.Metadata[beadmeta.RoutedToMetadataKey]; got != "" {
		t.Fatalf("verify attempt gc.routed_to = %q, want empty (pool-claimed work must not be double-dispatched)", got)
	}
}

// The regression guard has a loud counterpart: a rig whose config does not
// carry the core.control-dispatcher identity must FAIL the workflow AT
// CREATION (decoration error) instead of silently stamping an unrouted
// landing step that no one will ever serve.
func TestDecorateGraphWorkflowRecipe_MissingDispatcherTargetFailsLoud(t *testing.T) {
	const rig = "poe-pricer"
	// A config with NO control-dispatcher at all.
	cfg := &config.City{Workspace: config.Workspace{Name: "agentforge"}}
	r := landingRecipe()
	deps := Deps{Resolver: rigAwareDispatcherResolver{}}

	err := DecorateGraphWorkflowRecipeWithDefaultBinding(
		r, nil, "src-1", "city", "src-1", "rig:"+rig,
		GraphRouteBinding{MetadataOnly: true},
		nil, "agentforge", cfg, deps,
	)
	if err == nil {
		t.Fatal("decoration error = nil, want loud failure for an unresolvable gc.run_target (no silent unrouted landing)")
	}
	if !strings.Contains(err.Error(), "unknown formulas v2 target") && !strings.Contains(err.Error(), "control-dispatcher") {
		t.Fatalf("decoration error = %v, want it to name the unresolvable target", err)
	}
}
