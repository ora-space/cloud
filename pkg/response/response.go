// Package response provides standard HTTP response helpers.
package response

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Response represents standard API JSON response body
type Response struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Common business response status codes.
const (
	// CodeSuccess indicates operation succeeded.
	CodeSuccess = 0
	// CodeBadRequest indicates client request error.
	CodeBadRequest = 400
	// CodeNotFound indicates resource not found.
	CodeNotFound = 404
	// CodeServerError indicates internal server error.
	CodeServerError = 500
)

// Success sends a success response with code 0 and HTTP status 200
func Success(c *gin.Context, data any) {
	c.JSON(http.StatusOK, Response{
		Code:    CodeSuccess,
		Message: "success",
		Data:    data,
	})
}

// SuccessWithMessage sends a success response with a custom message
func SuccessWithMessage(c *gin.Context, message string, data any) {
	c.JSON(http.StatusOK, Response{
		Code:    CodeSuccess,
		Message: message,
		Data:    data,
	})
}

// Fail sends an error response with custom business code and message
func Fail(c *gin.Context, httpStatus, code int, message string) {
	c.JSON(httpStatus, Response{
		Code:    code,
		Message: message,
	})
}

// BadRequest sends HTTP 400 response
func BadRequest(c *gin.Context, message string) {
	Fail(c, http.StatusBadRequest, CodeBadRequest, message)
}

// ServerError sends HTTP 500 response
func ServerError(c *gin.Context, message string) {
	Fail(c, http.StatusInternalServerError, CodeServerError, message)
}

// NotFound sends HTTP 404 response
func NotFound(c *gin.Context, message string) {
	Fail(c, http.StatusNotFound, CodeNotFound, message)
}
