package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/ContainerSolutions/factory/internal/apiserver/gen"
	"github.com/ContainerSolutions/factory/internal/catalog"
)

// The catalog directories whose files the API serves.
const (
	promptsDir = "prompts/"
	schemasDir = "schemas/"
)

// ListCatalogFiles implements gen.StrictServerInterface: the live catalog's prompt templates
// and JSON Schemas, by path. They come from the loaded snapshot, never from disk.
func (s *Server) ListCatalogFiles(context.Context, gen.ListCatalogFilesRequestObject) (gen.ListCatalogFilesResponseObject, error) {
	cat, err := s.catalog()
	if err != nil {
		return nil, err
	}
	items := []gen.CatalogFileRef{}
	for p := range cat.Assets.Prompts {
		if catalogFilePath(p) {
			items = append(items, gen.CatalogFileRef{Path: p, Kind: gen.CatalogFileKindPrompt})
		}
	}
	for p := range cat.Assets.SchemaSources {
		if catalogFilePath(p) {
			items = append(items, gen.CatalogFileRef{Path: p, Kind: gen.CatalogFileKindSchema})
		}
	}
	slices.SortFunc(items, func(a, b gen.CatalogFileRef) int { return strings.Compare(a.Path, b.Path) })
	return gen.ListCatalogFiles200JSONResponse{Items: items}, nil
}

// GetCatalogFile implements gen.StrictServerInterface: one prompt template or JSON Schema of
// the live catalog, unrendered. A path outside prompts/ and schemas/, or not in its canonical
// form (.., //, a trailing /), is a 400; the lookup is in the snapshot's maps only.
func (s *Server) GetCatalogFile(_ context.Context, req gen.GetCatalogFileRequestObject) (gen.GetCatalogFileResponseObject, error) {
	p := req.Params.Path
	if !catalogFilePath(p) {
		return nil, &apiError{status: http.StatusBadRequest, code: CodeInvalid, detail: "path must be a file under prompts/ or schemas/",
			fields: []gen.FieldError{{Field: "path", Message: "not a file under prompts/ or schemas/"}}}
	}
	cat, err := s.catalog()
	if err != nil {
		return nil, err
	}
	out, ok := catalogFile(cat, p)
	if !ok {
		return nil, problemf(http.StatusNotFound, CodeNotFound, "no file %q in the catalog", p)
	}
	return gen.GetCatalogFile200JSONResponse(out), nil
}

// catalogFilePath reports whether p is a clean, relative path to a file under prompts/ or
// schemas/ (at most 512 bytes, as the contract allows).
func catalogFilePath(p string) bool {
	if len(p) > 512 || path.Clean(p) != p || strings.Contains(p, `\`) {
		return false
	}
	return (strings.HasPrefix(p, promptsDir) && len(p) > len(promptsDir)) ||
		(strings.HasPrefix(p, schemasDir) && len(p) > len(schemasDir))
}

// catalogFile is the catalog's file at p, which catalogFilePath accepted.
func catalogFile(cat *catalog.Catalog, p string) (gen.CatalogFile, bool) {
	if strings.HasPrefix(p, promptsDir) {
		pr := cat.Assets.Prompts[p]
		if pr == nil {
			return gen.CatalogFile{}, false
		}
		return gen.CatalogFile{Path: p, Kind: gen.CatalogFileKindPrompt, Digest: pr.Digest, Content: pr.Text}, true
	}
	b, ok := cat.Assets.SchemaSources[p]
	if !ok {
		return gen.CatalogFile{}, false
	}
	sum := sha256.Sum256(b)
	return gen.CatalogFile{Path: p, Kind: gen.CatalogFileKindSchema, Digest: "sha256:" + hex.EncodeToString(sum[:]), Content: string(b)}, true
}
