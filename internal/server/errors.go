package server

import "net/http"

// apiError is the body shape OpenAI clients expect for an error.
type apiError struct {
	Error apiErrorDetail `json:"error"`
}

type apiErrorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

// writeError answers with an OpenAI-shaped error. An empty code is sent as
// null.
func writeError(w http.ResponseWriter, status int, typ, code, message string) {
	d := apiErrorDetail{Message: message, Type: typ}
	if code != "" {
		d.Code = &code
	}
	writeJSON(w, status, apiError{Error: d})
}
