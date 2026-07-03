package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// JSON response conventions (documented in api/openapi.yaml):
//
//	success (single):  { "data": { ... } }
//	success (list):    { "data": [ ... ], "meta": { page, per_page, total } }
//	error:             { "error": { code, message, details? } }
//
// These helpers are the ONLY way handlers should write responses, so the shape
// stays consistent across every module.

// Meta carries pagination metadata for list responses.
type Meta struct {
	Page    int   `json:"page"`
	PerPage int   `json:"per_page"`
	Total   int64 `json:"total"`
}

// OK writes a 200 with a single data object.
func OK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, gin.H{"data": data})
}

// Created writes a 201 with a single data object.
func Created(c *gin.Context, data any) {
	c.JSON(http.StatusCreated, gin.H{"data": data})
}

// List writes a 200 with a data array and pagination meta.
func List(c *gin.Context, data any, meta Meta) {
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": meta})
}

// NoContent writes a 204.
func NoContent(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

// Error writes an AppError as a JSON error envelope and aborts the chain.
func Error(c *gin.Context, err *AppError) {
	c.AbortWithStatusJSON(err.Status, gin.H{"error": gin.H{
		"code":    err.Code,
		"message": err.Message,
		"details": err.Details,
	}})
}
