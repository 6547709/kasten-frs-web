package handlers

import (
	"log/slog"
	"net/http"

	"github.com/liguoqiang/kasten-frs-web/internal/k8s"
)

// handleSessionDelete deletes a FileRecoverySession and closes any
// pooled SFTP session for it. Registered as POST /sessions/{ns}/{name}/delete.
func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	name := r.PathValue("name")
	if err := s.frsDelete(r.Context(), ns, name); err != nil {
		slog.Error("frs.delete.failed", "user", s.auth.Username, "frs", ns+"/"+name, "err", err)
		s.renderError(w, http.StatusBadGateway, "删除 FRS 失败", err.Error())
		return
	}
	s.pool.CloseAllForFRS(ns, name)
	s.watches.del(k8s.FRSRef{Namespace: ns, Name: name})
	slog.Info("frs.deleted", "user", s.auth.Username, "frs", ns+"/"+name)
	http.Redirect(w, r, "/sessions", http.StatusSeeOther)
}
