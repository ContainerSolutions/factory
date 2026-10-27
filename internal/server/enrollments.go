package server

import (
	"context"
	"net/http"

	"github.com/ContainerSolutions/factory/internal/apiserver/gen"
	"github.com/ContainerSolutions/factory/internal/scheduler"
)

// CreateEnrollment implements gen.StrictServerInterface (P1 §6): an admin mints a one-time
// enrollment token for a pool, an owner and a name. The owner's login is on the dispatcher's
// sign-in provider; the token is returned only here.
func (s *Server) CreateEnrollment(ctx context.Context, req gen.CreateEnrollmentRequestObject) (gen.CreateEnrollmentResponseObject, error) {
	b := req.Body
	er := scheduler.EnrollRequest{
		Pool: b.Pool, Owner: b.Owner, Name: b.Name,
		OwnerProvider: s.d.Auth.Provider(), CreatedBy: principal(ctx).ActorID(),
	}
	if b.Ttl != nil {
		d, err := b.Ttl.Parse()
		if err != nil || d <= 0 {
			return nil, &apiError{status: http.StatusBadRequest, code: CodeInvalid, detail: "ttl must be a positive duration, like 7d",
				fields: []gen.FieldError{{Field: "ttl", Message: "a Duration more than 0 and at most 30d"}}}
		}
		er.TTL = d
	}
	e, tok, err := s.d.Scheduler.CreateEnrollment(ctx, er)
	if err != nil {
		return nil, err
	}
	return gen.CreateEnrollment201JSONResponse{Enrollment: enrollmentView(e), Token: tok}, nil
}

// ListEnrollments implements gen.StrictServerInterface: a page of enrollments, never their
// tokens or hashes.
func (s *Server) ListEnrollments(ctx context.Context, req gen.ListEnrollmentsRequestObject) (gen.ListEnrollmentsResponseObject, error) {
	p, err := parseList(req.Params.Cursor, req.Params.Limit, nil)
	if err != nil {
		return nil, err
	}
	es, err := s.d.Scheduler.ListEnrollments(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]gen.Enrollment, len(es))
	for i, e := range es {
		items[i] = enrollmentView(e)
	}
	out, next, err := page(scheduler.KindEnrollment, items, func(e gen.Enrollment) string { return e.Id }, p)
	if err != nil {
		return nil, err
	}
	return gen.ListEnrollments200JSONResponse{Items: out, NextCursor: next}, nil
}

// DeleteEnrollment implements gen.StrictServerInterface: an unused enrollment's token stops
// working; a used one's record goes, and its agent stays.
func (s *Server) DeleteEnrollment(ctx context.Context, req gen.DeleteEnrollmentRequestObject) (gen.DeleteEnrollmentResponseObject, error) {
	if err := s.d.Scheduler.DeleteEnrollment(ctx, req.Id); err != nil {
		return nil, err
	}
	return gen.DeleteEnrollment204Response{}, nil
}

// enrollmentView is the API's Enrollment: no token, no hash.
func enrollmentView(e *scheduler.Enrollment) gen.Enrollment {
	out := gen.Enrollment{
		Id: e.Metadata.Name, Pool: e.Spec.Pool, Owner: e.Spec.Owner, Name: e.Spec.Name,
		CreatedAt: e.Metadata.CreatedAt, ExpiresAt: e.Spec.ExpiresAt, CreatedBy: e.Spec.CreatedBy,
	}
	if !e.Status.UsedAt.IsZero() {
		at, agent := e.Status.UsedAt, e.Status.Agent
		out.UsedAt, out.Agent = &at, &agent
	}
	return out
}
