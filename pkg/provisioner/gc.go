package provisioner

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"
)

// Image / container garbage collection.
//
// GC is an INTENTIONAL operator action, never automatic: the dashboard
// lists what's reclaimable (GET /api/gc/candidates), the operator
// reviews it, then POSTs back the exact ids to remove (POST /api/gc).
// The execute path RE-VALIDATES every id against the candidate set
// before touching it, so a stale or hand-crafted id pointing at a
// running container or the runtime image can't slip through even if the
// client sends it. Confirm-list-before-trigger, defended on both ends.
//
// The reclaimable set is deliberately conservative:
//
//   - Dangling images — untagged `<none>` layers left behind when a
//     rebuild moved a tag to a new image. The dominant disk cost of
//     repeated deploys, and unreferenced by definition.
//   - Exited agentry DEPLOYMENT containers — stopped/failed deploy
//     containers (they carry the agentry.deployment.id label). Sandbox
//     containers, buildkit, and anything running are never listed.
//
// We never list tagged images (a tagged image could be a deploy the
// operator wants to roll back to) or running containers.

// gcImageCandidate is one reclaimable image in the candidate list.
type gcImageCandidate struct {
	ID        string   `json:"id"`       // full sha256:... id (what POST /api/gc expects)
	ShortID   string   `json:"short_id"` // trimmed for display
	Tags      []string `json:"tags"`     // usually ["<none>:<none>"] for dangling
	SizeBytes int64    `json:"size_bytes"`
	Created   int64    `json:"created"` // unix seconds
}

// gcContainerCandidate is one reclaimable container in the candidate list.
type gcContainerCandidate struct {
	ID      string `json:"id"`
	ShortID string `json:"short_id"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	State   string `json:"state"`  // "exited" | "created" | "dead"
	Status  string `json:"status"` // human string, e.g. "Exited (1) 2 days ago"
	Created int64  `json:"created"`
}

type gcCandidates struct {
	Images           []gcImageCandidate     `json:"images"`
	Containers       []gcContainerCandidate `json:"containers"`
	ReclaimableBytes int64                  `json:"reclaimable_bytes"`
}

// handleGCCandidates lists everything GC could reclaim, for operator
// review. Read-only; deletes nothing.
//
// GET /api/gc/candidates
func (p *Provisioner) handleGCCandidates(w http.ResponseWriter, r *http.Request) {
	dockerCli, err := p.docker()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "docker client unavailable: "+err.Error())
		return
	}
	cands, err := gcCollectCandidates(r.Context(), dockerCli)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "collect gc candidates: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cands)
}

// gcCollectCandidates is the shared lister used by both the candidates
// endpoint and the execute path's re-validation. Pulling it out means
// "what's reclaimable" is defined in exactly one place.
func gcCollectCandidates(ctx context.Context, cli *dockerclient.Client) (gcCandidates, error) {
	out := gcCandidates{Images: []gcImageCandidate{}, Containers: []gcContainerCandidate{}}

	// Dangling images only — untagged leftovers from rebuilds.
	imgFilter := filters.NewArgs()
	imgFilter.Add("dangling", "true")
	imgs, err := cli.ImageList(ctx, image.ListOptions{Filters: imgFilter})
	if err != nil {
		return out, err
	}
	for _, im := range imgs {
		tags := im.RepoTags
		if len(tags) == 0 {
			tags = []string{"<none>:<none>"}
		}
		out.Images = append(out.Images, gcImageCandidate{
			ID:        im.ID,
			ShortID:   shortDockerID(im.ID),
			Tags:      tags,
			SizeBytes: im.Size,
			Created:   im.Created,
		})
		out.ReclaimableBytes += im.Size
	}

	// Exited deployment containers — agentry.deployment.id label + a
	// non-running state. The label scopes us to agentry's own deploy
	// containers; we never list sandboxes, buildkit, or system
	// containers.
	ctrFilter := filters.NewArgs()
	ctrFilter.Add("label", deploymentIDLabel)
	ctrs, err := cli.ContainerList(ctx, container.ListOptions{All: true, Filters: ctrFilter})
	if err != nil {
		return out, err
	}
	for _, c := range ctrs {
		if !gcContainerReclaimable(c.State) {
			continue
		}
		out.Containers = append(out.Containers, gcContainerCandidate{
			ID:      c.ID,
			ShortID: shortDockerID(c.ID),
			Name:    strings.TrimPrefix(firstName(c.Names), "/"),
			Image:   c.Image,
			State:   c.State,
			Status:  c.Status,
			Created: c.Created,
		})
	}

	// Stable display order: biggest images first, newest containers first.
	sort.SliceStable(out.Images, func(i, j int) bool {
		return out.Images[i].SizeBytes > out.Images[j].SizeBytes
	})
	sort.SliceStable(out.Containers, func(i, j int) bool {
		return out.Containers[i].Created > out.Containers[j].Created
	})
	return out, nil
}

// gcContainerReclaimable reports whether a container state is safe to GC.
// Only stopped states — never "running", "restarting", "paused".
func gcContainerReclaimable(state string) bool {
	switch state {
	case "exited", "created", "dead":
		return true
	default:
		return false
	}
}

// GCRequest is the confirmed delete list the dashboard POSTs back after
// the operator reviews the candidates. Only ids that re-validate as
// reclaimable are touched.
type GCRequest struct {
	ImageIDs     []string `json:"image_ids"`
	ContainerIDs []string `json:"container_ids"`
}

// GCResult reports what was actually removed plus per-id errors, so the
// dashboard can show "freed 1.2 GB; 1 image skipped (in use)".
type GCResult struct {
	RemovedImages     []string  `json:"removed_images"`
	RemovedContainers []string  `json:"removed_containers"`
	ReclaimedBytes    int64     `json:"reclaimed_bytes"`
	Errors            []gcError `json:"errors,omitempty"`
}

type gcError struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// handleGC executes a reviewed GC. It re-derives the candidate set and
// removes ONLY ids that appear in it — a defense-in-depth check so a
// client that sends an id for a running container or a tagged image
// (whether stale or malicious) gets a skipped+error, never a deletion.
//
// POST /api/gc
func (p *Provisioner) handleGC(w http.ResponseWriter, r *http.Request) {
	dockerCli, err := p.docker()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "docker client unavailable: "+err.Error())
		return
	}
	var req GCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	if len(req.ImageIDs) == 0 && len(req.ContainerIDs) == 0 {
		writeError(w, http.StatusBadRequest, "nothing to collect: pass image_ids and/or container_ids")
		return
	}

	// Re-derive the reclaimable set NOW and only act on ids within it.
	cands, err := gcCollectCandidates(r.Context(), dockerCli)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "revalidate gc candidates: "+err.Error())
		return
	}

	// Partition the request against the reclaimable set BEFORE touching
	// docker. Anything not in the set becomes an error, never a delete —
	// this is the safety boundary, so it lives in a pure, tested function.
	plan := gcPlanRemovals(cands, req)
	res := GCResult{RemovedImages: []string{}, RemovedContainers: []string{}, Errors: plan.errors}

	for _, id := range plan.containerIDs {
		if err := dockerCli.ContainerRemove(r.Context(), id, container.RemoveOptions{Force: false}); err != nil {
			res.Errors = append(res.Errors, gcError{ID: id, Error: err.Error()})
			continue
		}
		res.RemovedContainers = append(res.RemovedContainers, id)
	}
	for _, im := range plan.images {
		if _, err := dockerCli.ImageRemove(r.Context(), im.id, image.RemoveOptions{Force: false, PruneChildren: true}); err != nil {
			res.Errors = append(res.Errors, gcError{ID: im.id, Error: err.Error()})
			continue
		}
		res.RemovedImages = append(res.RemovedImages, im.id)
		res.ReclaimedBytes += im.size
	}

	writeJSON(w, http.StatusOK, res)
}

// gcPlanImage pairs a validated image id with the size we'll credit when
// it's removed.
type gcPlanImage struct {
	id   string
	size int64
}

// gcPlan is the validated removal set: only ids that appeared in the
// freshly-collected candidate list, plus errors for everything else.
type gcPlan struct {
	images       []gcPlanImage
	containerIDs []string
	errors       []gcError
}

// gcPlanRemovals partitions a GCRequest against the reclaimable set. It
// is the safety boundary: an id not present in cands NEVER ends up in
// the removal set, regardless of what the client sent. Pure + tested so
// "a stale/forged id can't delete a running container or tagged image"
// is a property we can assert, not just hope for.
func gcPlanRemovals(cands gcCandidates, req GCRequest) gcPlan {
	imgSize := make(map[string]int64, len(cands.Images))
	for _, im := range cands.Images {
		imgSize[im.ID] = im.SizeBytes
	}
	validContainer := make(map[string]struct{}, len(cands.Containers))
	for _, c := range cands.Containers {
		validContainer[c.ID] = struct{}{}
	}

	var plan gcPlan
	for _, id := range req.ContainerIDs {
		if _, ok := validContainer[id]; !ok {
			plan.errors = append(plan.errors, gcError{ID: id, Error: "not a reclaimable container (running, unknown, or already gone)"})
			continue
		}
		plan.containerIDs = append(plan.containerIDs, id)
	}
	for _, id := range req.ImageIDs {
		size, ok := imgSize[id]
		if !ok {
			plan.errors = append(plan.errors, gcError{ID: id, Error: "not a reclaimable image (tagged, in use, or already gone)"})
			continue
		}
		plan.images = append(plan.images, gcPlanImage{id: id, size: size})
	}
	return plan
}

// shortDockerID trims a "sha256:abc…" id to 12 hex chars for display.
func shortDockerID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func firstName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}
