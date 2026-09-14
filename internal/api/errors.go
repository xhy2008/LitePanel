package api

import (
	"encoding/json"
	"net/http"
)

// 统一错误体（设计 14 节）。
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeErrorDetail(w, status, code, message, "")
}

func writeErrorDetail(w http.ResponseWriter, status int, code, message, detail string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Code: code, Message: message, Detail: detail})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
