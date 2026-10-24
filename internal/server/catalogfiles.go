package server

import (
	"context"
	"net/http"

	"github.com/compliance-framework/factory/internal/apiserver/gen"
)

// ListCatalogFiles implements gen.StrictServerInterface. A stub until UI-1b serves the loaded
// catalog's prompt and schema files.
func (s *Server) ListCatalogFiles(context.Context, gen.ListCatalogFilesRequestObject) (gen.ListCatalogFilesResponseObject, error) {
	return nil, problemf(http.StatusNotImplemented, CodeNotImplemented, "listing the catalog's files is not implemented yet")
}

// GetCatalogFile implements gen.StrictServerInterface. A stub until UI-1b.
func (s *Server) GetCatalogFile(context.Context, gen.GetCatalogFileRequestObject) (gen.GetCatalogFileResponseObject, error) {
	return nil, problemf(http.StatusNotImplemented, CodeNotImplemented, "serving a catalog file is not implemented yet")
}
