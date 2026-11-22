package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

// maxCatalog bounds the plan's model catalog. Each entry carries a long
// instructions template, so the catalog runs to hundreds of kilobytes.
const maxCatalog = 16 << 20

// modelList is an OpenAI model list.
type modelList struct {
	Object string  `json:"object"`
	Data   []model `json:"data"`
}

// model is one entry of an OpenAI model list.
type model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// planCatalog is the part of the plan's /models answer the proxy reads. The
// plan answers with a Codex model catalog instead of an OpenAI model list.
type planCatalog struct {
	Models []struct {
		Slug       string `json:"slug"`
		Visibility string `json:"visibility"`
	} `json:"models"`
}

// listModels answers GET /v1/models with the plan's catalog as an OpenAI
// model list.
func (p *proxy) listModels(w http.ResponseWriter, r *http.Request) {
	list, ok := p.catalog(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, modelList{Object: "list", Data: list})
}

// getModel answers GET /v1/models/{id} from the catalog. The plan token
// lacks the api.model.read scope that the upstream route requires.
func (p *proxy) getModel(w http.ResponseWriter, r *http.Request) {
	list, ok := p.catalog(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	for _, m := range list {
		if m.ID == id {
			writeJSON(w, http.StatusOK, m)
			return
		}
	}
	writeParamError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", "model",
		"the model "+strconv.Quote(id)+" is not available with ChatGPT plan usage; GET /v1/models lists the ones that are")
}

// catalog fetches the plan's listed models. When it returns false it has
// already answered w.
func (p *proxy) catalog(w http.ResponseWriter, r *http.Request) ([]model, bool) {
	resp, err := p.send(r.Context(), http.MethodGet, "/models", nil)
	if err != nil {
		writeSendError(w, err)
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		copyResponse(w, resp)
		return nil, false
	}
	var cat planCatalog
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCatalog)).Decode(&cat); err != nil {
		writeError(w, http.StatusBadGateway, "server_error", "upstream_invalid",
			"the upstream model catalog could not be read: "+err.Error())
		return nil, false
	}
	list := []model{}
	for _, m := range cat.Models {
		if m.Visibility == "hide" {
			continue
		}
		list = append(list, model{ID: m.Slug, Object: "model", OwnedBy: "openai"})
	}
	return list, true
}
