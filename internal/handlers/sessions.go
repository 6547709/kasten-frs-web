package handlers

import (
	"net/http"

	"github.com/liguoqiang/kasten-frs-web/internal/k8s"
	"github.com/liguoqiang/kasten-frs-web/internal/metrics"
)

// handleSessionDelete deletes a FileRecoverySession and closes any
// pooled SFTP session for it. Registered as POST /sessions/{ns}/{name}/delete.
func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	name := r.PathValue("name")
	log := s.log(r.Context())
	if err := s.frsDelete(r.Context(), ns, name); err != nil {
		log.Error("frs.delete.failed", "user", s.auth.Username, "frs", ns+"/"+name, "err", err)
		s.renderError(w, http.StatusBadGateway, "Failed to delete FRS", err.Error())
		return
	}
	s.pool.CloseAllForFRS(ns, name)
	metrics.SFTPConnectionsActive.Set(float64(s.pool.Len()))
	s.watches.del(k8s.FRSRef{Namespace: ns, Name: name})
	log.Info("frs.deleted", "user", s.auth.Username, "frs", ns+"/"+name)
	http.Redirect(w, r, "/sessions", http.StatusSeeOther)
}
