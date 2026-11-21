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
	writeParamError(w, status, typ, code, "", message)
}

// writeParamError is writeError with the offending request field. An empty
// param is sent as null.
func writeParamError(w http.ResponseWriter, status int, typ, code, param, message string) {
	d := apiErrorDetail{Message: message, Type: typ}
	if code != "" {
		d.Code = &code
	}
	if param != "" {
		d.Param = &param
	}
	writeJSON(w, status, apiError{Error: d})
}
