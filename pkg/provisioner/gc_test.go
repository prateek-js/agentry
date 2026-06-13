package provisioner

import "testing"

// The GC safety boundary lives in gcPlanRemovals: an id the client sends
// is only ever removed if it's in the freshly-collected reclaimable set.
// These tests pin that an id NOT in the set can never reach the removal
// plan — the property that keeps "confirm a stale list" from deleting a
// running container or a tagged (rollback-worthy) image.

func TestGCPlanRemovals_OnlyRemovesReclaimable(t *testing.T) {
	cands := gcCandidates{
		Images: []gcImageCandidate{
			{ID: "sha256:img_a", SizeBytes: 100},
			{ID: "sha256:img_b", SizeBytes: 250},
		},
		Containers: []gcContainerCandidate{
			{ID: "ctr_x"},
		},
	}
	req := GCRequest{
		ImageIDs:     []string{"sha256:img_a", "sha256:NOT_A_CANDIDATE"},
		ContainerIDs: []string{"ctr_x", "ctr_RUNNING"},
	}
	plan := gcPlanRemovals(cands, req)

	// Valid image kept with its size; bogus image rejected.
	if len(plan.images) != 1 || plan.images[0].id != "sha256:img_a" || plan.images[0].size != 100 {
		t.Fatalf("expected only img_a (size 100) in plan; got %+v", plan.images)
	}
	// Valid container kept; bogus container rejected.
	if len(plan.containerIDs) != 1 || plan.containerIDs[0] != "ctr_x" {
		t.Fatalf("expected only ctr_x in plan; got %v", plan.containerIDs)
	}
	// Two rejections, each naming the offending id.
	if len(plan.errors) != 2 {
		t.Fatalf("expected 2 errors for the bogus ids; got %+v", plan.errors)
	}
	gotErrIDs := map[string]bool{}
	for _, e := range plan.errors {
		gotErrIDs[e.ID] = true
	}
	if !gotErrIDs["sha256:NOT_A_CANDIDATE"] || !gotErrIDs["ctr_RUNNING"] {
		t.Errorf("errors should name the rejected ids; got %+v", plan.errors)
	}
}

func TestGCPlanRemovals_EmptyRequestEmptyPlan(t *testing.T) {
	cands := gcCandidates{Images: []gcImageCandidate{{ID: "x", SizeBytes: 1}}}
	plan := gcPlanRemovals(cands, GCRequest{})
	if len(plan.images) != 0 || len(plan.containerIDs) != 0 || len(plan.errors) != 0 {
		t.Errorf("empty request should yield an empty plan; got %+v", plan)
	}
}

func TestGCPlanRemovals_NothingReclaimableRejectsEverything(t *testing.T) {
	// Candidate set is empty (e.g. the host was cleaned between the
	// operator's review and the confirm). Every requested id is rejected
	// — no deletes happen.
	plan := gcPlanRemovals(gcCandidates{}, GCRequest{
		ImageIDs:     []string{"sha256:a"},
		ContainerIDs: []string{"c1"},
	})
	if len(plan.images) != 0 || len(plan.containerIDs) != 0 {
		t.Fatalf("an empty candidate set must remove nothing; got %+v", plan)
	}
	if len(plan.errors) != 2 {
		t.Errorf("both ids should be rejected; got %+v", plan.errors)
	}
}

func TestGCContainerReclaimable(t *testing.T) {
	for state, want := range map[string]bool{
		"exited":     true,
		"created":    true,
		"dead":       true,
		"running":    false,
		"restarting": false,
		"paused":     false,
		"":           false,
	} {
		if got := gcContainerReclaimable(state); got != want {
			t.Errorf("gcContainerReclaimable(%q) = %v; want %v", state, got, want)
		}
	}
}

func TestShortDockerID(t *testing.T) {
	if got := shortDockerID("sha256:abcdef0123456789"); got != "abcdef012345" {
		t.Errorf("shortDockerID = %q; want abcdef012345", got)
	}
	if got := shortDockerID("short"); got != "short" {
		t.Errorf("shortDockerID(short) = %q; want short", got)
	}
}
