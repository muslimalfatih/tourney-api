package server

import "net/http"

// AppError is the canonical application error. Handlers and services return it
// (or one of the constructors below) and the response layer renders it as a
// consistent JSON error envelope. Code is a stable machine-readable string the
// frontend can switch on; Message is human-readable.
type AppError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func (e *AppError) Error() string { return e.Message }

// WithDetails returns a copy of the error carrying structured detail (e.g. a
// slice of field validation errors).
func (e *AppError) WithDetails(details any) *AppError {
	cp := *e
	cp.Details = details
	return &cp
}

// Constructors for the common cases. Prefer these over building AppError inline.

func ErrBadRequest(msg string) *AppError {
	return &AppError{Status: http.StatusBadRequest, Code: "bad_request", Message: msg}
}

func ErrValidation(msg string) *AppError {
	return &AppError{Status: http.StatusUnprocessableEntity, Code: "validation_error", Message: msg}
}

func ErrUnauthorized(msg string) *AppError {
	if msg == "" {
		msg = "authentication required"
	}
	return &AppError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: msg}
}

func ErrForbidden(msg string) *AppError {
	if msg == "" {
		msg = "you do not have permission to perform this action"
	}
	return &AppError{Status: http.StatusForbidden, Code: "forbidden", Message: msg}
}

func ErrNotFound(msg string) *AppError {
	if msg == "" {
		msg = "resource not found"
	}
	return &AppError{Status: http.StatusNotFound, Code: "not_found", Message: msg}
}

func ErrConflict(msg string) *AppError {
	return &AppError{Status: http.StatusConflict, Code: "conflict", Message: msg}
}

func ErrInternal(msg string) *AppError {
	if msg == "" {
		msg = "an unexpected error occurred"
	}
	return &AppError{Status: http.StatusInternalServerError, Code: "internal_error", Message: msg}
}
