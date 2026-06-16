package handlers

import (
	"net/http"

	"github.com/liguoqiang/kasten-frs-web/internal/k8s"
)

// handleSessionDelete deletes a FileRecoverySession and closes any
// pooled SFTP session for it. Registered as POST /sessions/{ns}/{name}/delete.
func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	name := r.PathValue("name")
	if err := s.frsDelete(r.Context(), ns, name); err != nil {
		s.renderError(w, http.StatusBadGateway, "删除 FRS 失败", err.Error())
		return
	}
	s.pool.CloseAllForFRS(ns, name)
	// Clear the wizard's watch-map entry too. If the user just
	// cancelled a FRS that was created moments ago by the wizard,
	// the background goroutine may still be polling WaitForReady;
	// without this, the next /browse on a same-name FRS would
	// briefly inherit the stale "Pending/Timeout" state.
	s.watches.del(k8s.FRSRef{Namespace: ns, Name: name})
	http.Redirect(w, r, "/sessions", http.StatusSeeOther)
}
