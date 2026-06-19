package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/nexusllm/nexusllm/internal/auth"
	"github.com/nexusllm/nexusllm/internal/models"
)

const ClaimsKey = "claims"

// AuthRequired validates the Bearer token (API key or JWT) on every request.
// On success it stores *auth.TeamClaims under the "claims" key in the Gin context.
func AuthRequired(svc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if header == "" {
			abort(c, http.StatusUnauthorized, "missing_auth", "Authorization header required")
			return
		}

		parts := strings.SplitN(header, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
			abort(c, http.StatusUnauthorized, "invalid_auth", "Authorization header must be: Bearer <token>")
			return
		}

		token := strings.TrimSpace(parts[1])
		if token == "" {
			abort(c, http.StatusUnauthorized, "missing_token", "Token must not be empty")
			return
		}

		var (
			claims *auth.TeamClaims
			err    error
		)

		// API keys start with "nxs_"; everything else is treated as JWT.
		if strings.HasPrefix(token, "nxs_") {
			claims, err = svc.ValidateAPIKey(c.Request.Context(), token)
		} else {
			claims, err = svc.ValidateJWT(c.Request.Context(), token)
		}

		if err != nil {
			abort(c, http.StatusUnauthorized, "invalid_token", "Token is invalid or expired")
			return
		}

		c.Set(ClaimsKey, claims)
		c.Next()
	}
}

// GetClaims retrieves TeamClaims stored by AuthRequired. Returns nil if not set.
func GetClaims(c *gin.Context) *auth.TeamClaims {
	v, exists := c.Get(ClaimsKey)
	if !exists {
		return nil
	}
	claims, _ := v.(*auth.TeamClaims)
	return claims
}

func abort(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, models.ErrorResponse{
		Error: models.ErrorDetail{
			Message: msg,
			Type:    "auth_error",
			Code:    code,
		},
	})
}
