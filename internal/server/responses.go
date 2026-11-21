package server

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/klhq/oai-plan-proxy/internal/responses"
)

// maxRequestBody bounds a /v1/responses body. Inline images make bodies
// large, so the bound is generous.
const maxRequestBody = 32 << 20

// droppedHeader lists the parameters removed from a request.
const droppedHeader = "x-oai-plan-proxy-dropped-params"

// responsesHandler sanitizes a Responses request and forwards it.
func (p *proxy) responsesHandler(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", err.Error())
		return
	}
	req, err := responses.Sanitize(raw, p.opts.Strict)
	var reqErr *responses.Error
	if errors.As(err, &reqErr) {
		writeParamError(w, http.StatusBadRequest, "invalid_request_error", reqErr.Code, reqErr.Param, reqErr.Message)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "", err.Error())
		return
	}
	if len(req.Dropped) > 0 {
		w.Header().Set(droppedHeader, strings.Join(req.Dropped, ","))
	}
	resp, err := p.send(r.Context(), http.MethodPost, "/responses", req.Body)
	if err != nil {
		writeSendError(w, err)
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
