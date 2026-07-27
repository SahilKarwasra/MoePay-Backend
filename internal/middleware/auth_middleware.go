package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/sahilkarwasra/moepay/internal/utils"
)

const (
	AuthorizationHeaderKey = "Authorization"
	AuthorizationTypeBearer = "bearer"
	ContextUserIDKey        = "user_id"
	ContextPhoneKey         = "phone_number"
)

func AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader(AuthorizationHeaderKey)
		if len(authHeader) == 0 {
			utils.Unauthorized(c, "Authorization header is missing")
			c.Abort()
			return
		}

		fields := strings.Fields(authHeader)
		if len(fields) < 2 {
			utils.Unauthorized(c, "Invalid authorization header format")
			c.Abort()
			return
		}

		authorizationType := strings.ToLower(fields[0])
		if authorizationType != AuthorizationTypeBearer {
			utils.Unauthorized(c, "Unsupported authorization type")
			c.Abort()
			return
		}

		accessToken := fields[1]
		claims, err := utils.ValidateAccessToken(accessToken)
		if err != nil {
			utils.Unauthorized(c, "Invalid or expired access token")
			c.Abort()
			return
		}

		c.Set(ContextUserIDKey, claims.UserId)
		c.Set(ContextPhoneKey, claims.PhoneNumber)
		c.Next()
	}
}
