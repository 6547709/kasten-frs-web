package handlers

import (
	"net/http"
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
	http.Redirect(w, r, "/sessions", http.StatusSeeOther)
}
