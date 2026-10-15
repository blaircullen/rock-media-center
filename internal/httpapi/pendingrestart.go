package httpapi

import (
	"net/http"
)

// handlePendingRestart — GET /api/v1/system/pending-restart. Folders saved but not in use
// until a restart, and whether the app can restart itself. A separate endpoint, so
// /system/library's flat folder map (which the Settings form diffs field by field) stays as
// it is.
func (a *api) handlePendingRestart(w http.ResponseWriter, r *http.Request) {
	st := a.folderRestartState(r.Context())
	a.writeJSON(w, http.StatusOK, map[string]any{
		"restart_needed": st.Needed,
		"can_restart":    st.CanRestart,
		"changed":        st.Changed,
	})
}
