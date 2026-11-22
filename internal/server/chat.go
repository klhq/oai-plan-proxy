package server

import (
	"net/http"
	"slices"

	"github.com/klhq/oai-plan-proxy/internal/chat"
	"github.com/klhq/oai-plan-proxy/internal/responses"
)

// chatHandler serves a Chat Completions request through the plan's
// Responses route.
func (p *proxy) chatHandler(w http.ResponseWriter, r *http.Request) {
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	tr, err := chat.Translate(raw)
	if err != nil {
		writeRequestError(w, err)
		return
	}
	if p.opts.Strict && len(tr.Dropped) > 0 {
		writeParamError(w, http.StatusBadRequest, "invalid_request_error", "unsupported_parameter", tr.Dropped[0],
			tr.Dropped[0]+" has no equivalent with ChatGPT plan usage; remove it, or run serve without --strict to drop it")
		return
	}
	req, err := responses.Sanitize(tr.Body, p.opts.Strict)
	if err != nil {
		writeRequestError(w, err)
		return
	}
	dropped := slices.Concat(tr.Dropped, req.Dropped)
	slices.Sort(dropped)
	resp, ok := p.sendResponses(w, r, req.Body, dropped)
	if !ok {
		return
	}
	defer resp.Body.Close()
	writeCompleted(w, resp.Body, chat.Completion)
}
