package gateway

import (
	"fmt"
	"strings"

	"github.com/yetone/magpie/internal/provider"
)

// unknownModel is what a request for a model magpie doesn't know is told
// (MOMO on Discord: "Zen-Space-Bunny" got a 404 that said neither how a
// model is named nor which one was meant): the ids nearest to it, the
// ways a model can be named, then every id there is. what is "" for a
// chat, or what the model was for (" to draw with").
func unknownModel(asked, what string) string {
	msg := fmt.Sprintf("magpie knows no model %q%s", asked, what)
	if near := provider.Closest(asked, 3); len(near) > 0 {
		msg += "; did you mean " + strings.Join(near, ", ") + "?"
	}
	msg += " Name a model as provider/model (as /v1/models lists it), a routing group's id or name, or a bare model id" +
		" (one served by several providers goes to the group magpie found for it, else to the first of them in the Providers order)"
	if ids := provider.IDs(); len(ids) > 0 {
		msg += "; it has " + strings.Join(ids, ", ")
	} else {
		msg += "; add a provider in magpie first"
	}
	return msg
}

// notPicked is what a model the Providers page doesn't tick is told: magpie
// knows it, it just isn't picked, and saying "knows no model" (as this was
// first written) sent the reader looking for a catalog problem that wasn't
// there — 4 个模型里有 3 个报 404 时看到的就是这句。
func notPicked(asked string) string {
	pid, model, ok := strings.Cut(asked, "/")
	if !ok {
		return fmt.Sprintf("%q is not picked on the Providers page: pick it there, then save, to serve it", asked)
	}
	return fmt.Sprintf("magpie knows %s but %q is not picked on the Providers page ( Providers → %s → Models): pick it there, then save, to serve it", asked, model, pid)
}
