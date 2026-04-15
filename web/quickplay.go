// Web routes for the Quick Play page: a stripped match-play UI for home/practice use
// that lets an operator load a test match with arbitrary team numbers in one click.

package web

import (
	"net/http"

	"github.com/Team254/cheesy-arena/model"
)

// Shows the Quick Play operator interface.
func (web *Web) quickPlayHandler(w http.ResponseWriter, r *http.Request) {
	if !web.userIsAdmin(w, r) {
		return
	}

	template, err := web.parseFiles("templates/quickplay.html", "templates/base.html")
	if err != nil {
		handleWebErr(w, err)
		return
	}
	data := struct {
		*model.EventSettings
		PlcIsEnabled          bool
		PlcArmorBlockStatuses map[string]bool
	}{
		web.arena.EventSettings,
		web.arena.Plc.IsEnabled(),
		web.arena.Plc.GetArmorBlockStatuses(),
	}
	err = template.ExecuteTemplate(w, "base", data)
	if err != nil {
		handleWebErr(w, err)
		return
	}
}
