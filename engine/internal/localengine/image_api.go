//go:build linux || darwin

package localengine

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
)

func (e *Engine) pullHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	source, tag := q.Get("fromImage"), q.Get("tag")
	if source == "" || q.Get("fromSrc") != "" {
		writeError(w, fmt.Errorf("%w: expected a registry image reference", errInvalid))
		return
	}
	platforms, err := imagePlatforms(q["platform"])
	if err != nil {
		writeError(w, err)
		return
	}
	if len(platforms) > 1 {
		writeError(w, fmt.Errorf("%w: only one platform may be pulled at a time", errInvalid))
		return
	}
	if tag != "" && !strings.Contains(source, "@") {
		if strings.HasPrefix(tag, "sha256:") {
			source += "@" + tag
		} else {
			source += ":" + tag
		}
	}
	ref, err := name.ParseReference(source)
	if err != nil {
		writeError(w, fmt.Errorf("%w: invalid image reference", errInvalid))
		return
	}
	auth, err := registryAuth(r)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	out := json.NewEncoder(w)
	if err := out.Encode(map[string]string{"status": "Pulling " + ref.Name()}); err != nil {
		return
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	image, err := e.pull(r.Context(), ref, auth, platforms...)
	if err != nil {
		writeRegistryStreamError(out, err, "pull access denied")
		return
	}
	_ = out.Encode(map[string]string{"status": "Pulled " + ref.Name(), "id": image.ID})
}

// imagesHTTP serves GET /images/json: the filtered images, newest first as
// Docker lists them, then by ID so equal times keep one order.
func (e *Engine) imagesHTTP(w http.ResponseWriter, r *http.Request) {
	filters, err := parseFilters(r.URL.Query().Get("filters"), "dangling", "label", "reference")
	if err != nil {
		writeError(w, err)
		return
	}
	dangling, err := pruneBool(filters, "dangling")
	if err != nil {
		writeError(w, err)
		return
	}
	e.mu.Lock()
	all := e.allImages()
	rows := make([]map[string]any, 0, len(all))
	ids := slices.SortedFunc(maps.Keys(all), func(a, b string) int {
		return cmp.Or(cmp.Compare(all[b].Created, all[a].Created), strings.Compare(a, b))
	})
	for _, id := range ids {
		image := all[id]
		tags, digests := e.imageNames(id)
		refs := e.imageRefs(id)
		isDangling := len(refs) == 0
		if dangling != nil {
			if isDangling != *dangling {
				continue
			}
		} else if isDangling && !flagSet(r.URL.Query(), "all") {
			continue
		}
		if !matchesLabels(image.Labels, filters["label"]) || !imageReferencesMatch(refs, filters["reference"]) {
			continue
		}
		rows = append(rows, map[string]any{
			"Id": id, "RepoTags": tags, "RepoDigests": digests, "Labels": image.Labels,
			"Created": image.Created, "Size": image.Size,
		})
	}
	e.mu.Unlock()
	writeJSON(w, http.StatusOK, rows)
}

func (e *Engine) imageNames(id string) (tags, digests []string) {
	tags, digests = []string{}, []string{}
	for _, ref := range e.imageRefs(id) {
		image := e.images[ref]
		if strings.Contains(ref, "@") {
			digests = append(digests, ref)
		} else {
			tags = append(tags, ref)
		}
		if image.RepoDigest != "" {
			digests = append(digests, image.RepoDigest)
		}
	}
	slices.Sort(digests)
	return tags, slices.Compact(digests)
}

func imageReferencesMatch(refs, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, ref := range refs {
		for _, pattern := range patterns {
			if match, _ := path.Match(pattern, ref); match {
				return true
			}
			if parsed, err := name.ParseReference(ref); err == nil && parsed.Context().RegistryStr() == name.DefaultRegistry {
				if match, _ := path.Match(pattern, shortImageReference(parsed.Name())); match {
					return true
				}
			}
		}
	}
	return false
}

// shortImageReference omits the implied Docker Hub registry and library
// namespace from a canonical reference, as Fabo's archive format does.
func shortImageReference(ref string) string {
	if rest, ok := strings.CutPrefix(ref, name.DefaultRegistry+"/"); ok {
		return strings.TrimPrefix(rest, "library/")
	}
	return ref
}

func (e *Engine) inspectImageHTTP(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	if history, ok := strings.CutSuffix(ref, "/history"); ok {
		e.imageHistoryHTTP(w, history)
		return
	}
	ref, ok := strings.CutSuffix(ref, "/json")
	if !ok {
		http.NotFound(w, r)
		return
	}
	e.mu.Lock()
	image, err := e.findImage(ref)
	if err != nil {
		e.mu.Unlock()
		writeError(w, err)
		return
	}
	tags, digests := e.imageNames(image.ID)
	// Fabo's named inspection exposes the manifest in the named repository.
	// An alias for an image from another registry does not imply a push.
	if named, err := name.ParseReference(image.ref); err == nil {
		prefix := named.Context().Name() + "@"
		digests = slices.DeleteFunc(digests, func(d string) bool { return !strings.HasPrefix(d, prefix) })
		if i := slices.Index(digests, image.RepoDigest); i > 0 {
			digests[0], digests[i] = digests[i], digests[0]
		}
	}
	e.mu.Unlock()
	check, shell, err := image.healthMetadata()
	if err != nil {
		writeError(w, err)
		return
	}
	platform := image.platform()
	writeJSON(w, http.StatusOK, map[string]any{
		"Id": image.ID, "RepoTags": tags, "RepoDigests": digests, "Size": image.Size,
		"Created":      time.Unix(image.Created, 0).UTC().Format(time.RFC3339Nano),
		"Architecture": platform.Architecture, "Os": platform.OS, "Variant": platform.Variant,
		"RootFS": map[string]any{"Type": "layers", "Layers": image.DiffIDs},
		"Config": map[string]any{
			"Cmd": image.Cmd, "Entrypoint": image.Entrypoint, "Env": image.Env, "WorkingDir": image.WorkingDir,
			"User": image.User, "ExposedPorts": image.ExposedPorts, "StopSignal": image.StopSignal, "Labels": image.Labels,
			"Healthcheck": check, "Shell": shell,
		},
	})
}

func (e *Engine) imageActionHTTP(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	switch {
	case strings.HasSuffix(ref, "/push"):
		e.pushImageHTTP(w, r, strings.TrimSuffix(ref, "/push"))
	case strings.HasSuffix(ref, "/tag"):
		e.tagImageHTTP(w, r, strings.TrimSuffix(ref, "/tag"))
	default:
		http.NotFound(w, r)
	}
}

func (e *Engine) tagImageHTTP(w http.ResponseWriter, r *http.Request, source string) {
	repo, tag := r.URL.Query().Get("repo"), r.URL.Query().Get("tag")
	if tag == "" {
		tag = "latest"
	}
	target, err := name.NewTag(repo + ":" + tag)
	if repo == "" || err != nil {
		writeError(w, fmt.Errorf("%w: invalid target image tag", errInvalid))
		return
	}
	e.imageOps.Lock()
	defer e.imageOps.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		writeError(w, fmt.Errorf("%w: daemon is shutting down", errConflict))
		return
	}
	image, err := e.findImage(source)
	if err == nil {
		if !strings.HasPrefix(image.RepoDigest, target.Context().Name()+"@") {
			image.RepoDigest = ""
		}
		err = e.putImage(target.Name(), image.Image)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	e.emitImage(image.Image, "tag", target.Name())
	w.WriteHeader(http.StatusCreated)
}
