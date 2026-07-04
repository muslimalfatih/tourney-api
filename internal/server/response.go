package server

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// Pagination parses ?page & ?per_page with sane defaults and caps, returning
// the 1-based page, per-page size, and computed SQL offset. This is the single
// place list endpoints read pagination from, keeping the convention uniform.
func Pagination(c *gin.Context) (page, perPage, offset int) {
	page = atoiDefault(c.Query("page"), 1)
	if page < 1 {
		page = 1
	}
	perPage = atoiDefault(c.Query("per_page"), 20)
	if perPage < 1 {
		perPage = 20
	}
	if perPage > 100 {
		perPage = 100
	}
	offset = (page - 1) * perPage
	return page, perPage, offset
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

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
