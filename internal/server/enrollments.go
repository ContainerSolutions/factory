package server

import (
	"context"
	"net/http"

	"github.com/compliance-framework/factory/internal/apiserver/gen"
)

// errEnrollmentNotImplemented answers the enrollment routes and an admin's deregisterAgent
// until the dispatcher mints, stores and checks enrollment and refresh tokens (P1 phase 6,
// design/proposals/P1-agent-workspaces.md §6).
func errEnrollmentNotImplemented(what string) error {
	return problemf(http.StatusNotImplemented, CodeNotImplemented, "%s is not implemented yet (P1 phase 6)", what)
}

// CreateEnrollment implements gen.StrictServerInterface. A stub until P1 phase 6, which mints
// the enrollment and its token.
func (s *Server) CreateEnrollment(context.Context, gen.CreateEnrollmentRequestObject) (gen.CreateEnrollmentResponseObject, error) {
	return nil, errEnrollmentNotImplemented("minting an enrollment")
}

// ListEnrollments implements gen.StrictServerInterface. A stub until P1 phase 6, which stores
// the enrollments.
func (s *Server) ListEnrollments(context.Context, gen.ListEnrollmentsRequestObject) (gen.ListEnrollmentsResponseObject, error) {
	return nil, errEnrollmentNotImplemented("listing enrollments")
}

// DeleteEnrollment implements gen.StrictServerInterface. A stub until P1 phase 6, which
// revokes an unused enrollment.
func (s *Server) DeleteEnrollment(context.Context, gen.DeleteEnrollmentRequestObject) (gen.DeleteEnrollmentResponseObject, error) {
	return nil, errEnrollmentNotImplemented("revoking an enrollment")
}
